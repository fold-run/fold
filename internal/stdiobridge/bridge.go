// Package stdiobridge exposes a stdio MCP server over streamable HTTP.
//
// The bridge is deliberately protocol-blind. Each downstream HTTP session is
// paired with its own child process, and JSON-RPC messages are pumped between
// the two connections verbatim — no method table, no typed parameters, no
// rewriting. That is what keeps a shimmed server indistinguishable from a
// native streamable-HTTP one: methods the SDK has not learned yet, and
// server-initiated traffic like sampling and elicitation, cross the bridge
// without the bridge understanding either.
//
// Framing stays in the SDK on both sides: [mcp.CommandTransport] owns the
// stdio side and [mcp.StreamableServerTransport] owns the HTTP side. The
// bridge owns only session bookkeeping and the pump.
package stdiobridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	// sessionIDHeader is the streamable HTTP session header, as named by the
	// MCP spec. The SDK keeps its own unexported copy; this must match it.
	sessionIDHeader = "Mcp-Session-Id"

	// methodInitialize is the one method name the bridge knows, and it uses it
	// only to decide whether a session outlives its first request.
	methodInitialize = "initialize"

	// DefaultMaxBodyBytes bounds a single POST. Matches the gateway's own
	// server.maxBodyBytes default so a request that the gateway accepted is
	// not then refused one hop later.
	DefaultMaxBodyBytes = 1 << 20
)

// Options configures a Bridge.
type Options struct {
	// Command is the stdio MCP server to run, with its arguments. It is fixed
	// at construction and never derived from a request — see the package's
	// security note in docs/design-stdio.md.
	Command []string

	// Env is the exact environment handed to each child. Nil means an empty
	// environment; the caller builds this from an explicit allowlist rather
	// than letting the child inherit the bridge's own environment.
	Env []string

	// Dir is the child's working directory. Empty means the bridge's own.
	Dir string

	// MaxSessions bounds concurrent child processes. Zero means DefaultMaxSessions.
	MaxSessions int

	// MaxBodyBytes bounds one POST body. Zero means DefaultMaxBodyBytes.
	MaxBodyBytes int64

	// IdleTimeout closes sessions with no traffic for this long. Zero means
	// DefaultIdleTimeout.
	IdleTimeout time.Duration

	// Logger receives bridge events and the child's stderr. Nil discards.
	Logger *slog.Logger
}

// DefaultMaxSessions bounds concurrent children when Options.MaxSessions is
// unset. Each session is a process, so the ceiling is deliberately low enough
// that a misbehaving client cannot fork-bomb the host.
const DefaultMaxSessions = 64

// DefaultIdleTimeout closes sessions with no traffic for this long, matching
// the gateway's own bridged-session sweep. A session holds a process, so an
// abandoned one must not hold it forever.
const DefaultIdleTimeout = 5 * time.Minute

// A Bridge serves one stdio MCP server over streamable HTTP.
type Bridge struct {
	opts    Options
	log     *slog.Logger
	maxSes  int
	maxBody int64
	idle    time.Duration
	cors    *http.CrossOriginProtection

	stopSweep chan struct{}
	sweepOnce sync.Once

	mu       sync.Mutex
	sessions map[string]*session
	probing  int // health probes holding a slot
	closed   bool

	// Health probe memo: a probe costs a process, so it is reused for
	// healthTTL and single-flighted across concurrent callers.
	healthMu  sync.Mutex
	healthAt  time.Time
	healthErr error
	healthing chan struct{}

	// Counters exported through Stats.
	spawned  atomic.Int64
	failures atomic.Int64
	rejected atomic.Int64
}

// A session pairs one downstream HTTP session with the child serving it.
type session struct {
	id        string
	transport *mcp.StreamableServerTransport
	conn      mcp.Connection
	child     *child
	closeOnce sync.Once
	stop      context.CancelFunc

	lastUsed atomic.Int64 // unix nanos, for the idle sweeper
}

func (s *session) touch() { s.lastUsed.Store(time.Now().UnixNano()) }

func (s *session) idleFor(now time.Time) time.Duration {
	return now.Sub(time.Unix(0, s.lastUsed.Load()))
}

// A child is a running stdio MCP server and its connection.
//
// There is exactly one child per session, and that is not a tuning choice: a
// stdio connection carries exactly one MCP session, so two downstream sessions
// sharing a process would share an id space. Two pumps reading one connection
// steal each other's replies, and demultiplexing them would mean rewriting
// JSON-RPC ids and synthesizing handshakes — a protocol-aware proxy, which is
// what the gateway in front of this shim already is. See docs/design-stdio.md.
type child struct {
	cmd  *exec.Cmd
	conn mcp.Connection
}

// New returns a Bridge for opts. It does not start any process — children are
// spawned lazily, when a downstream session is created.
func New(opts Options) (*Bridge, error) {
	if len(opts.Command) == 0 {
		return nil, errors.New("stdiobridge: no command")
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	maxSes := opts.MaxSessions
	if maxSes <= 0 {
		maxSes = DefaultMaxSessions
	}
	maxBody := opts.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = DefaultMaxBodyBytes
	}
	idle := opts.IdleTimeout
	if idle <= 0 {
		idle = DefaultIdleTimeout
	}
	b := &Bridge{
		opts:      opts,
		log:       log,
		maxSes:    maxSes,
		maxBody:   maxBody,
		idle:      idle,
		cors:      http.NewCrossOriginProtection(),
		sessions:  make(map[string]*session),
		stopSweep: make(chan struct{}),
	}
	go b.sweepLoop()
	return b, nil
}

// sweepLoop closes sessions idle past the timeout. Without it a client that
// opens sessions and walks away without a DELETE pins a process per session
// forever, and the ceiling becomes permanent rather than momentary.
func (b *Bridge) sweepLoop() {
	t := time.NewTicker(b.idle / 2)
	defer t.Stop()
	for {
		select {
		case <-b.stopSweep:
			return
		case <-t.C:
			b.sweepIdle()
		}
	}
}

func (b *Bridge) sweepIdle() {
	now := time.Now()
	b.mu.Lock()
	var stale []*session
	for _, s := range b.sessions {
		if s != nil && s.idleFor(now) > b.idle {
			stale = append(stale, s)
		}
	}
	b.mu.Unlock()
	for _, s := range stale {
		b.log.Debug("sweeping idle session", "session", s.id)
		b.closeSession(s)
	}
}

// ServeHTTP implements the streamable HTTP endpoint.
//
// Session routing mirrors the SDK's own stateful handler: a POST without a
// session header opens a session, a POST or GET carrying one is routed to it,
// and DELETE ends it. Everything past routing is the SDK's transport.
//
// Connecting the transport directly is what keeps the bridge protocol-blind,
// but it also skips [mcp.StreamableHTTPHandler], where the SDK keeps its
// inbound protections. Those are re-applied here: skipping them would leave a
// loopback shim drivable by any web page that can rebind DNS.
func (b *Bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !b.checkInbound(w, r) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		b.servePOST(w, r)
	case http.MethodGet:
		b.serveGET(w, r)
	case http.MethodDelete:
		b.serveDELETE(w, r)
	default:
		// RFC 9110 §15.5.6: a 405 must say what is allowed.
		w.Header().Set("Allow", "GET, POST, DELETE")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
	}
}

// checkInbound applies the protections the SDK's own handler would have, and
// reports whether the request may proceed.
func (b *Bridge) checkInbound(w http.ResponseWriter, r *http.Request) bool {
	// DNS-rebinding protection for loopback listeners, matching the SDK and
	// the gateway's own host validation. A browser page that resolves its
	// hostname to 127.0.0.1 is same-origin to itself, so CORS never applies —
	// the Host check is what stops it driving a local server.
	if addr, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok && addr != nil {
		if isLoopbackAddr(addr.String()) && !isLoopbackHost(r.Host) {
			http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q", r.Host), http.StatusForbidden)
			return false
		}
	}
	if b.cors != nil {
		if err := b.cors.Check(r); err != nil {
			http.Error(w, "Forbidden: "+err.Error(), http.StatusForbidden)
			return false
		}
	}
	// Require application/json on POST. Without it a cross-origin fetch is a
	// CORS "simple" request, which is not preflighted and so reaches the child
	// as a blind CSRF write.
	if r.Method == http.MethodPost {
		if mediaType(r.Header.Get("Content-Type")) != "application/json" {
			http.Error(w, "Content-Type must be 'application/json'", http.StatusUnsupportedMediaType)
			return false
		}
		// Bound every body, not just the handshake: the SDK transport reads a
		// session-bearing POST with io.ReadAll.
		r.Body = http.MaxBytesReader(w, r.Body, b.maxBody)
	}
	return true
}

// mediaType strips parameters from a Content-Type value.
func mediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// isLoopbackHost reports whether a Host header names the loopback interface.
func isLoopbackHost(host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// isLoopbackAddr reports whether a listener address is on loopback.
func isLoopbackAddr(addr string) bool {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		addr = h
	}
	ip := net.ParseIP(strings.Trim(addr, "[]"))
	return ip != nil && ip.IsLoopback()
}

func (b *Bridge) servePOST(w http.ResponseWriter, r *http.Request) {
	if id := r.Header.Get(sessionIDHeader); id != "" {
		s, ok := b.lookup(w, id)
		if !ok {
			return
		}
		s.touch()
		defer s.touch()
		s.transport.ServeHTTP(w, r)
		return
	}
	// A session-less POST is either the handshake or a pre-handshake probe.
	// Only an initialize earns a durable session; everything else — notably
	// the protocol-negotiation probe the SDK client sends first — is served on
	// a session that is torn down with the request. Without this, every client
	// connect would strand a process for the life of the bridge.
	//
	// A body that is not JSON-RPC at all is refused before spawning: otherwise
	// an empty POST is a fork/exec amplifier.
	durable, err := peekInitialize(r)
	if err != nil {
		http.Error(w, "Bad Request: "+err.Error(), http.StatusBadRequest)
		return
	}
	s, err := b.open(r.Context())
	if err != nil {
		b.rejectOpen(w, err)
		return
	}
	if !durable {
		defer b.closeSession(s)
	}
	s.transport.ServeHTTP(w, r)
}

// peekInitialize reports whether the body carries an initialize request,
// leaving the body readable for the transport. It reads the JSON-RPC envelope
// only to decide session durability — nothing is inspected for content and
// nothing is rewritten.
//
// A body that does not parse as a JSON-RPC message or batch is an error, not a
// non-initialize: every session-less POST costs a process, so garbage must be
// refused before spawning rather than handed to a child.
func peekInitialize(r *http.Request) (bool, error) {
	if r.Body == nil {
		return false, errors.New("empty body")
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false, errors.New("unreadable body")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	// A POST carries either one message or a batch of them.
	var one struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
	}
	if err := json.Unmarshal(body, &one); err == nil && one.JSONRPC != "" {
		return one.Method == methodInitialize, nil
	}
	var batch []struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
	}
	if err := json.Unmarshal(body, &batch); err == nil && len(batch) > 0 {
		for _, m := range batch {
			if m.Method == methodInitialize {
				return true, nil
			}
		}
		return false, nil
	}
	return false, errors.New("not a JSON-RPC message")
}

func (b *Bridge) serveGET(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(sessionIDHeader)
	if id == "" {
		http.Error(w, "Bad Request: GET requires an "+sessionIDHeader+" header", http.StatusBadRequest)
		return
	}
	s, ok := b.lookup(w, id)
	if !ok {
		return
	}
	s.touch()
	defer s.touch()
	s.transport.ServeHTTP(w, r)
}

func (b *Bridge) serveDELETE(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(sessionIDHeader)
	if id == "" {
		http.Error(w, "Bad Request: DELETE requires an "+sessionIDHeader+" header", http.StatusBadRequest)
		return
	}
	s, ok := b.lookup(w, id)
	if !ok {
		return
	}
	b.closeSession(s)
	w.WriteHeader(http.StatusNoContent)
}

// rejectOpen maps an open failure to a status. A refused session is a 503 with
// Retry-After: the bound is a capacity limit, not a client error, and saying so
// lets the gateway's breaker treat the endpoint as temporarily unhealthy rather
// than as a permanent failure.
func (b *Bridge) rejectOpen(w http.ResponseWriter, err error) {
	if errors.Is(err, errTooManySessions) {
		b.rejected.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Service Unavailable: session limit reached", http.StatusServiceUnavailable)
		return
	}
	b.failures.Add(1)
	b.log.Error("session open failed", "err", err)
	http.Error(w, "Bad Gateway: upstream server unavailable", http.StatusBadGateway)
}

func (b *Bridge) lookup(w http.ResponseWriter, id string) (*session, bool) {
	b.mu.Lock()
	s := b.sessions[id]
	b.mu.Unlock()
	if s == nil {
		http.Error(w, "Not Found: no such session", http.StatusNotFound)
		return nil, false
	}
	return s, true
}

var errTooManySessions = errors.New("stdiobridge: session limit reached")

// open spawns (or joins) a child and wires a downstream session to it.
func (b *Bridge) open(ctx context.Context) (*session, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("stdiobridge: closed")
	}
	if len(b.sessions)+b.probing >= b.maxSes {
		b.mu.Unlock()
		return nil, errTooManySessions
	}
	// Reserve the slot before the (slow) spawn so concurrent opens cannot
	// overshoot the bound.
	id := rand.Text()
	b.sessions[id] = nil
	b.mu.Unlock()

	s, err := b.wire(ctx, id)
	if err != nil {
		b.mu.Lock()
		delete(b.sessions, id)
		b.mu.Unlock()
		return nil, err
	}
	b.mu.Lock()
	if b.closed {
		// Close ran while this session was being wired; it snapshotted only
		// registered sessions, so registering now would strand the child.
		b.mu.Unlock()
		b.teardown(s)
		return nil, errors.New("stdiobridge: closed")
	}
	s.touch()
	b.sessions[id] = s
	b.mu.Unlock()
	return s, nil
}

// wire acquires a child and connects a transport to it, pumping in both
// directions until either side closes.
func (b *Bridge) wire(ctx context.Context, id string) (*session, error) {
	c, err := b.spawn()
	if err != nil {
		return nil, err
	}
	transport := &mcp.StreamableServerTransport{SessionID: id}
	// Detached: the pump outlives the request that created the session.
	pumpCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	conn, err := transport.Connect(pumpCtx)
	if err != nil {
		stop()
		closeChild(c)
		return nil, fmt.Errorf("connect transport: %w", err)
	}
	s := &session{id: id, transport: transport, conn: conn, child: c, stop: stop}
	go b.pump(pumpCtx, s, conn, c.conn, "downstream→server")
	go b.pump(pumpCtx, s, c.conn, conn, "server→downstream")
	return s, nil
}

// pump forwards every message from one connection to the other verbatim, and
// tears the session down when either side ends. Messages are neither inspected
// nor rewritten — ids, methods, and payloads cross untouched.
func (b *Bridge) pump(ctx context.Context, s *session, from, to mcp.Connection, dir string) {
	defer b.closeSession(s)
	for {
		msg, err := from.Read(ctx)
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, mcp.ErrConnectionClosed) {
				b.log.Debug("bridge read ended", "session", s.id, "dir", dir, "err", err)
			}
			return
		}
		if err := to.Write(ctx, msg); err != nil {
			if ctx.Err() == nil {
				b.log.Debug("bridge write ended", "session", s.id, "dir", dir, "err", err)
			}
			return
		}
	}
}

// spawn starts one child process and connects to its stdio.
func (b *Bridge) spawn() (*child, error) {
	// #nosec G204 -- the command is fixed at construction from the operator's
	// argv and is never derived from request data. See docs/design-stdio.md.
	cmd := exec.Command(b.opts.Command[0], b.opts.Command[1:]...)
	// A nil Env means "inherit" to os/exec — the opposite of what this package
	// promises. Normalize so a caller that leaves it unset gets an empty
	// environment rather than the bridge's own, which may hold its bearer.
	cmd.Env = b.opts.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = b.opts.Dir
	// Own the child's process group. The canonical command is a wrapper
	// (`npx`, `uvx`) that forks the real server; signalling only the direct
	// child would orphan the grandchild, which then reparents to PID 1 — which
	// in the shim's own image is the shim. The group is swept in closeChild.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// cmd.Stderr rather than StderrPipe: the SDK's transport calls Wait when
	// the connection closes, and StderrPipe documents that Wait must not run
	// until reads finish — which would drop exactly the diagnostics a dying
	// child emits. An os.Pipe hands the drain to a goroutine Wait does not
	// close underneath us.
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	cmd.Stderr = pw
	conn, err := (&mcp.CommandTransport{Command: cmd}).Connect(context.Background())
	if err != nil {
		_ = pr.Close()
		_ = pw.Close()
		b.failures.Add(1)
		return nil, fmt.Errorf("start %q: %w", b.opts.Command[0], err)
	}
	// The child holds the write end; this copy would otherwise keep the reader
	// from ever seeing EOF.
	_ = pw.Close()
	b.spawned.Add(1)
	go relayStderr(b.log, pr)
	return &child{cmd: cmd, conn: conn}, nil
}

// closeSession tears down one session exactly once and then releases its slot.
//
// The slot is freed only after the child is gone. Deleting first would let a
// client loop open-and-abandon: teardown takes seconds (the SDK closes stdin,
// waits, SIGTERMs, waits, SIGKILLs), so an early release would admit new
// sessions while the old processes were still alive and the documented process
// ceiling would bound map entries rather than processes.
func (b *Bridge) closeSession(s *session) {
	s.closeOnce.Do(func() {
		b.teardown(s)
		b.mu.Lock()
		delete(b.sessions, s.id)
		b.mu.Unlock()
	})
}

// teardown stops the pumps and reaps the child, without touching the session
// map. Callers that hold a slot release it after this returns.
func (b *Bridge) teardown(s *session) {
	s.stop()
	_ = s.conn.Close()
	closeChild(s.child)
}

// Close ends every session and stops every child. It is safe to call twice.
func (b *Bridge) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	live := make([]*session, 0, len(b.sessions))
	for _, s := range b.sessions {
		if s != nil {
			live = append(live, s)
		}
	}
	b.mu.Unlock()

	b.sweepOnce.Do(func() { close(b.stopSweep) })
	for _, s := range live {
		b.closeSession(s)
	}
	return nil
}

// closeChild closes the child's connection — which closes stdin and, after the
// SDK's grace period, signals and reaps the direct child — then sweeps the
// whole process group so a wrapper's grandchildren cannot survive it.
//
// The connection's Close already calls Wait; calling it again here would race
// the SDK's own reaping goroutine on one exec.Cmd, so it is deliberately not
// repeated.
func closeChild(c *child) {
	if c == nil {
		return
	}
	_ = c.conn.Close()
	killGroup(c)
}

// killGroup SIGKILLs the child's process group. By the time this runs the
// direct child is normally already reaped, and killing a group whose leader is
// gone is a no-op — the point is the grandchildren an `npx`-style wrapper
// leaves behind, which nothing else would ever signal.
func killGroup(c *child) {
	if c.cmd == nil || c.cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(c.cmd.Process.Pid)
	if err != nil || pgid <= 1 { // gone, or not its own group — never signal 0/1
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// relayStderr forwards the child's diagnostics to the bridge's logger. Stdio
// servers use stderr for logging (stdout is the protocol), so dropping it
// would make a misbehaving server silent.
func relayStderr(log *slog.Logger, r io.ReadCloser) {
	defer func() { _ = r.Close() }()
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			log.Debug("server stderr", "msg", string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

// Stats is a point-in-time view of the bridge, for /health and /metrics.
type Stats struct {
	Sessions    int   `json:"sessions"`
	MaxSessions int   `json:"maxSessions"`
	Spawned     int64 `json:"spawned"`
	SpawnErrors int64 `json:"spawnErrors"`
	Rejected    int64 `json:"rejected"`
}

// Stats returns the current counters.
func (b *Bridge) Stats() Stats {
	b.mu.Lock()
	n := len(b.sessions)
	b.mu.Unlock()
	return Stats{
		Sessions:    n,
		MaxSessions: b.maxSes,
		Spawned:     b.spawned.Load(),
		SpawnErrors: b.failures.Load(),
		Rejected:    b.rejected.Load(),
	}
}

// Probe reports whether the configured command can be started at all, by
// spawning one child and stopping it. A shim that cannot start its server is
// unhealthy, and saying so is what lets the gateway's breaker eject the
// endpoint instead of hanging on it.
//
// Probe always spawns. Health is the polled entry point — use it for /health.
func (b *Bridge) Probe(ctx context.Context) error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return errors.New("stdiobridge: closed")
	}
	// A probe is a process like any other, so it takes a slot: otherwise it
	// spawns outside the ceiling, and one in flight during Close leaves a child
	// nothing will reap.
	if len(b.sessions)+b.probing >= b.maxSes {
		b.mu.Unlock()
		return errTooManySessions
	}
	b.probing++
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		b.probing--
		b.mu.Unlock()
	}()

	done := make(chan error, 1)
	go func() {
		c, err := b.spawn()
		if err != nil {
			done <- err
			return
		}
		closeChild(c)
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// healthTTL bounds how often Health will spawn. A probe costs a process, so an
// unbounded /health would be a fork bomb driven by whatever polls it — the
// same load-amplification the gateway closed for its own /health.
const healthTTL = time.Second

// Health reports whether the server is runnable, without letting a polling
// probe become a process spawner. A live session is itself proof of health, so
// the common case costs nothing; otherwise the answer is a probe result reused
// for healthTTL and single-flighted across concurrent callers.
func (b *Bridge) Health(ctx context.Context) error {
	b.mu.Lock()
	live := len(b.sessions) > 0
	b.mu.Unlock()
	if live {
		return nil
	}

	b.healthMu.Lock()
	if time.Since(b.healthAt) < healthTTL {
		err := b.healthErr
		b.healthMu.Unlock()
		return err
	}
	if b.healthing != nil { // a probe is already in flight; wait for it
		wait := b.healthing
		b.healthMu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return ctx.Err()
		}
		b.healthMu.Lock()
		err := b.healthErr
		b.healthMu.Unlock()
		return err
	}
	inflight := make(chan struct{})
	b.healthing = inflight
	b.healthMu.Unlock()

	// The probe runs on a context the caller cannot cancel, bounded by the
	// bridge's own timeout. /health is unauthenticated, so a caller that
	// aborted its connection must not be able to decide the answer — and a
	// cancellation result must never be cached, or one aborted request would
	// report the shim unhealthy to every prober for healthTTL and drive an
	// endpoint ejection or a liveness restart loop.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), probeTimeout)
	err := b.Probe(pctx)
	cancel()

	b.healthMu.Lock()
	if !errors.Is(err, context.Canceled) {
		b.healthErr = err
		b.healthAt = time.Now()
	}
	cached := b.healthErr
	b.healthing = nil
	b.healthMu.Unlock()
	close(inflight)
	return cached
}

// probeTimeout bounds one health probe. Generous, because a cold `npx` start
// is seconds and reporting that as unhealthy would be wrong.
const probeTimeout = 30 * time.Second

// BuildEnv returns the child environment: the named variables, taken from the
// bridge's own environment, and nothing else. The child never inherits by
// default, so a shim holding its own bearer secret does not hand it to the
// server it supervises.
func BuildEnv(names []string) []string {
	env := make([]string, 0, len(names))
	for _, n := range names {
		if v, ok := os.LookupEnv(n); ok {
			env = append(env, n+"="+v)
		}
	}
	return env
}
