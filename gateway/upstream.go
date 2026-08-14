package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/state"
	"github.com/fold-run/fold/policy"
)

// Gateway-minted JSON-RPC error codes, documented in the README.
const (
	codeRateLimited       = -32040 // per-upstream rate limit exceeded
	codeUpstreamDown      = -32041 // circuit open or upstream unreachable
	codeDenied            = -32042 // policy denied the invocation
	codeUnknownNamespace  = -32043 // name does not resolve to an upstream
	codeBudgetExhausted   = -32044 // consumption budget exhausted for the period
	defaultCacheTTL       = 30 * time.Second
	defaultConnectTimeout = 5 * time.Second
	defaultRequestTimeout = 60 * time.Second

	// bridgedIdleTimeout bounds how long a per-client upstream session
	// outlives its last use before the sweeper closes it.
	bridgedIdleTimeout = 5 * time.Minute

	// maxConnectCandidates bounds how many endpoints one connect attempt
	// walks before reporting the upstream down (see connect).
	maxConnectCandidates = 3

	// minProbeIntervalMs floors active health-probe intervals for static
	// upstreams, matching the discovery path's default floor
	// (config.Discovery.MinHealthCheckIntervalResolved).
	minProbeIntervalMs = 1000
)

// upstream wraps one configured MCP server: a shared root session (lists,
// health, completion, subscriptions), per-client bridged sessions (named
// invocations, so server-initiated traffic flows back to the right client),
// credential injection, and the per-upstream guards (rate limit, breaker).
type upstream struct {
	cfg   config.Upstream
	creds *auth.UpstreamCredentials

	// endpoints balances new sessions across the upstream's replicas
	// (single-endpoint upstreams hold a pool of one).
	endpoints *endpointPool

	// pins notices when this upstream rewrites a definition it already
	// advertised. Nil-safe and inert unless pinDefinitions enables it.
	pins *definitionPins

	limiter state.Limiter
	budget  state.Budget
	// globalBudget is the server-wide allowance, shared by every upstream.
	// Nil in tests that build an upstream directly.
	globalBudget state.Budget
	breaker      state.Breaker
	lists        state.ListCache

	connectTimeout time.Duration
	requestTimeout time.Duration
	cacheTTL       time.Duration

	httpClient *http.Client

	// onResourceUpdated forwards resources/updated notifications arriving on
	// the root session (which holds all upstream subscriptions).
	onResourceUpdated func(context.Context, *mcp.ResourceUpdatedNotificationParams)

	// onListChanged re-emits an upstream's list_changed notification to the
	// gateway's connected clients ("tools" | "prompts" | "resources").
	onListChanged func(kind string)

	// metrics is the owning gateway's instrumentation (set by Gateway).
	metrics *metricsSet
	// tracer is the owning gateway's tracer (nil → tracing disabled).
	tracer *gwTracer
	// log is the owning gateway's logger, tagged with this upstream id.
	log *slog.Logger

	// probeStop ends the active health-probe loop (nil when healthCheck is
	// not configured); stopProbeOnce makes Close safe to call repeatedly.
	probeStop     chan struct{}
	stopProbeOnce sync.Once
	// breaker-transition logging state.
	hadFailure       atomic.Bool
	lastBreakerState atomic.Value // string

	mu         sync.Mutex
	session    *mcp.ClientSession       // root session
	bridged    map[string]*bridgedEntry // downstream session ID → bridged session
	subscribed map[string]bool          // URIs subscribed on the root session

	// decoded memoizes the parsed form of each cached list, so a warm cache
	// hit does not re-run json.Unmarshal on every request (see cachedList).
	// Keyed by list kind, so it holds at most one entry per list method.
	decodedMu sync.Mutex
	decoded   map[string]decodedList

	// sep is the gateway's namespace separator, fixed for this upstream's
	// life (the routing section is construction-wired and cannot hot-reload).
	// Set by newWiredUpstream; empty for an un-namespaced upstream, which is
	// never rewritten anyway.
	sep string

	// publicTools / publicPrompts memoize the namespaced view of the cached
	// lists — the egress rewrite is a pure function of the decoded list, the
	// namespace, and the separator, so it belongs next to the decode rather
	// than on every request. See publicView.
	publicMu      sync.Mutex
	publicTools   publicView[mcp.Tool]
	publicPrompts publicView[mcp.Prompt]
}

// publicView is a namespaced list, index-aligned with the bare list it was
// built from. from is retained for the identity check that validates it:
// bare lists come from the decode memo, so a refill yields a different slice
// and rebuilds the view — the same self-invalidating trick cachedList uses.
type publicView[T any] struct {
	from  []*T
	items []*T
}

// get returns the memoized view for bare, if it was built from exactly that
// slice.
func (v publicView[T]) get(bare []*T) ([]*T, bool) {
	if len(v.from) != len(bare) {
		return nil, false
	}
	if len(bare) > 0 && &v.from[0] != &bare[0] {
		return nil, false
	}
	return v.items, true
}

// decodedList is one cached list's parsed form, valid only for the exact
// bytes it was parsed from. bytes is retained rather than just its address:
// holding the backing array alive is what makes the identity check in
// cachedList sound, because no later allocation can reuse that address while
// this reference exists.
type decodedList struct {
	bytes []byte
	items any // []T for the T the list was decoded as
}

type bridgedEntry struct {
	session  *mcp.ClientSession
	lastUsed time.Time
}

func newUpstream(cfg config.Upstream, provider state.Provider) *upstream {
	u := &upstream{
		cfg:            cfg,
		creds:          auth.NewUpstreamCredentials(cfg.Auth, http.DefaultClient),
		lists:          provider.ListCache("up:" + cfg.ID),
		connectTimeout: defaultConnectTimeout,
		requestTimeout: defaultRequestTimeout,
		cacheTTL:       defaultCacheTTL,
		bridged:        map[string]*bridgedEntry{},
		subscribed:     map[string]bool{},
		log:            slog.New(slog.DiscardHandler),
	}
	// The store is scoped per upstream, so a definition is pinned against the
	// server that advertised it and two upstreams offering the same tool name
	// cannot alias. report stays nil until the gateway wires it — "off"
	// compiles to a nil hook rather than to a branch on every refill.
	if cfg.PinDefinitions == "warn" {
		u.pins = &definitionPins{store: provider.Store("pin:" + cfg.ID)}
	}
	// streamIdle is opt-in, not defaulted: the SDK client only resets its
	// reconnect budget when Last-Event-ID advances, so a default idle cut
	// against an upstream that legitimately never sends notifications would
	// burn through the no-progress retries and kill the whole session (~5
	// cuts) — the gateway causing exactly the failure the knob exists to
	// bound. An operator sets it for upstreams whose streams actually wedge.
	streamIdle := time.Duration(0)
	if t := cfg.Timeouts; t != nil {
		if t.ConnectMs > 0 {
			u.connectTimeout = time.Duration(t.ConnectMs) * time.Millisecond
		}
		if t.RequestMs > 0 {
			u.requestTimeout = time.Duration(t.RequestMs) * time.Millisecond
		}
		if t.StreamIdleMs > 0 {
			streamIdle = time.Duration(t.StreamIdleMs) * time.Millisecond
		}
	}
	if cfg.CacheTTLMs > 0 {
		u.cacheTTL = time.Duration(cfg.CacheTTLMs) * time.Millisecond
	} else if cfg.CacheTTLMs < 0 {
		u.cacheTTL = 0
	}
	// A caller-derived credential (passthrough / token-exchange) means the
	// upstream may return a per-user list. The list cache is keyed per
	// upstream, not per principal, so caching one caller's list and serving
	// it to another would leak names across principals. Disable it.
	if cfg.Auth != nil && (cfg.Auth.Strategy == "passthrough" || cfg.Auth.Strategy == "token-exchange") {
		u.cacheTTL = 0
	}
	rpm := 0
	if rl := cfg.RateLimit; rl != nil {
		rpm = rl.RequestsPerMinute
	}
	u.limiter = provider.Limiter("up:"+cfg.ID, rpm)
	// The budget scope keys on the upstream id, not on this upstream object,
	// so consumption survives a reload that retires and rebuilds it. With
	// shared state that holds fleet-wide; without it, a rebuild starts a fresh
	// in-process counter, exactly as the rate-limit window does.
	u.budget = provider.Budget("up:"+cfg.ID,
		state.Period(cfg.Budget.ResolvedPeriod()), cfg.Budget.Allowance())
	threshold, halfOpen := 5, 30*time.Second
	if cb := cfg.CircuitBreaker; cb != nil {
		if cb.FailureThreshold > 0 {
			threshold = cb.FailureThreshold
		}
		if cb.HalfOpenAfterMs > 0 {
			halfOpen = time.Duration(cb.HalfOpenAfterMs) * time.Millisecond
		}
	}
	u.breaker = provider.Breaker("up:"+cfg.ID, threshold, halfOpen)
	// A failed endpoint rests for the breaker's half-open interval — the same
	// "retry the unhealthy thing after" semantic, applied per replica.
	u.endpoints = newEndpointPool(cfg.Endpoints(), halfOpen)
	sessionEra := cfg.Protocol == "" || cfg.Protocol == "session"
	upstreamHosts := map[string]bool{}
	for _, ep := range cfg.Endpoints() {
		if parsed, err := url.Parse(ep); err == nil && parsed.Host != "" {
			upstreamHosts[parsed.Host] = true
		}
	}
	ct := newCredentialTransport(u.creds, sessionEra, upstreamHosts)
	ct.streamIdle = streamIdle
	u.httpClient = &http.Client{
		Transport: ct,
		// Never follow a redirect that changes host: credentials are attached
		// per outgoing request, so a hostile upstream returning a cross-host
		// 3xx would otherwise capture the upstream API key or the caller's
		// bearer token. Same-host redirects are allowed (trailing-slash, etc).
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Host != via[0].URL.Host {
				u.log.Warn("refused cross-host redirect from upstream",
					"from", via[0].URL.Host, "to", req.URL.Host)
				return fmt.Errorf("refusing cross-host redirect to %q (upstream %q)", req.URL.Host, cfg.ID)
			}
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects")
			}
			return nil
		},
	}
	// The transport shares the upstream's logger once it exists.
	ct.log = func() *slog.Logger { return u.log }
	return u
}

// sseHeaderTimeout bounds how long an upstream may take to answer the
// standalone SSE GET with response headers. Real-world servers exist that
// accept the GET and never respond; the SDK client establishes this stream
// synchronously during the handshake, so a hang would eat the whole
// connect budget. A hang is converted into a synthetic 405 — the spec's
// "server does not offer an SSE stream" answer — which the client handles
// by proceeding without the stream. (var for tests)
var sseHeaderTimeout = 3 * time.Second

// credentialTransport attaches upstream credentials to every outgoing
// request, resolving caller-derived strategies (passthrough, token-exchange)
// from the request context. In session mode it also answers server/discover
// with 405, steering the SDK client onto the sessionful legacy handshake —
// the only era whose connections can carry server-initiated requests
// (sampling, elicitation) back through the gateway.
type credentialTransport struct {
	creds         *auth.UpstreamCredentials
	base          http.RoundTripper
	sse           http.RoundTripper // GETs: base + response-header timeout
	sessionEra    bool
	upstreamHosts map[string]bool     // credentials/trace attach only to these hosts
	log           func() *slog.Logger // resolved lazily (upstream logger set after construction)

	// streamIdle bounds silence on the standalone SSE stream (see the GET
	// branch of RoundTrip); 0 disables the bound.
	streamIdle time.Duration
}

func newCredentialTransport(creds *auth.UpstreamCredentials, sessionEra bool, upstreamHosts map[string]bool) *credentialTransport {
	base := http.RoundTripper(http.DefaultTransport)
	sse := base
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		clone := t.Clone()
		// A gateway talks to few upstream hosts with many requests in
		// flight; Go's default pool keeps only 2 idle conns per host, which
		// forces a fresh TCP(+TLS) handshake for most proxied requests once
		// more than two are concurrent — measured by tools/perf as ~14k
		// connections/s of churn at 8 client connections.
		clone.MaxIdleConns = 1024
		clone.MaxIdleConnsPerHost = 256
		base = clone
		sseClone := clone.Clone()
		sseClone.ResponseHeaderTimeout = sseHeaderTimeout
		sse = sseClone
	}
	return &credentialTransport{
		creds:         creds,
		base:          base,
		sse:           sse,
		sessionEra:    sessionEra,
		upstreamHosts: upstreamHosts,
	}
}

func synthetic405(req *http.Request) *http.Response {
	return &http.Response{
		StatusCode: http.StatusMethodNotAllowed,
		Status:     "405 Method Not Allowed",
		Proto:      req.Proto,
		ProtoMajor: req.ProtoMajor,
		ProtoMinor: req.ProtoMinor,
		Header:     http.Header{},
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Request:    req,
	}
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.sessionEra && req.Method == http.MethodPost && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		_ = req.Body.Close()
		if err != nil {
			return nil, err
		}
		if bytes.Contains(body, []byte(`"server/discover"`)) {
			return synthetic405(req), nil
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	req = req.Clone(req.Context())
	// Attach credentials and trace context only when the request is actually
	// bound for one of the configured upstream hosts. CheckRedirect already
	// blocks cross-host redirects; this is defense-in-depth so a credential
	// can never ride a request to any other host.
	if len(t.upstreamHosts) == 0 || t.upstreamHosts[req.URL.Host] {
		if err := t.creds.Apply(req.Context(), req.Header); err != nil {
			return nil, fmt.Errorf("upstream credentials: %w", err)
		}
		injectTraceContext(req.Context(), req.Header)
	}
	if req.Method == http.MethodGet {
		resp, err := t.sse.RoundTrip(req)
		if err != nil && strings.Contains(err.Error(), "timeout awaiting response headers") {
			if t.log != nil {
				t.log().Warn("upstream hung the standalone SSE GET; proceeding without server-initiated stream",
					"timeout", sseHeaderTimeout)
			}
			return synthetic405(req), nil
		}
		// timeouts.streamIdleMs: bound silence on the standalone stream.
		// sseHeaderTimeout above catches a server that never answers the
		// GET; this catches one that answers and then wedges — TCP alive,
		// no bytes — which otherwise holds a dead notification channel
		// open forever. Cutting the stream is safe by construction: the
		// SDK client reconnects interrupted standalone streams with
		// backoff and Last-Event-ID, so a healthy-but-quiet upstream sees
		// a periodic re-GET, and a wedged one is actually retried. POST
		// response streams are deliberately not wrapped — they carry an
		// in-flight call whose lifetime requestTimeout already governs,
		// and severed mid-call they may not be resumable.
		if err == nil && resp.StatusCode == http.StatusOK && t.streamIdle > 0 &&
			strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			resp.Body = newIdleTimeoutBody(resp.Body, t.streamIdle)
		}
		return resp, err
	}
	return t.base.RoundTrip(req)
}

// idleTimeoutBody closes an SSE stream that has gone silent for longer than
// idle. Each delivered byte re-arms the timer; when it fires, the underlying
// body is closed, which unblocks the SDK's pending Read with an error it
// treats as a transient interruption (and reconnects from).
type idleTimeoutBody struct {
	rc       io.ReadCloser
	timer    *time.Timer
	idle     time.Duration
	timedOut atomic.Bool
}

func newIdleTimeoutBody(rc io.ReadCloser, idle time.Duration) *idleTimeoutBody {
	b := &idleTimeoutBody{rc: rc, idle: idle}
	b.timer = time.AfterFunc(idle, func() {
		b.timedOut.Store(true)
		_ = rc.Close()
	})
	return b
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	if err != nil && b.timedOut.Load() {
		err = fmt.Errorf("upstream SSE stream idle for %s: %w", b.idle, err)
	}
	return n, err
}

func (b *idleTimeoutBody) Close() error {
	b.timer.Stop()
	return b.rc.Close()
}

// connect establishes a new session, load-balancing across the upstream's
// endpoints: candidates come back round-robin with recently-failed replicas
// last, each is tried in turn, and a connect failure ejects that endpoint
// for the pool's cooldown. Sessions stay pinned to the endpoint they
// connected to — balancing is per new session, matching MCP's sessionful
// wire protocol.
func (u *upstream) connect(ctx context.Context, opts *mcp.ClientOptions) (*mcp.ClientSession, error) {
	candidates := u.endpoints.candidates()
	// Bound the failover walk: with every replica of a K-replica upstream
	// black-holing SYNs, walking all of them costs K×connectTimeout per
	// caller before the breaker has even seen the failures. Three attempts
	// is enough to step over a couple of freshly-dead replicas (ejections
	// push known-bad ones to the back anyway); beyond that the upstream is
	// down and failing fast is the correct answer.
	if len(candidates) > maxConnectCandidates {
		candidates = candidates[:maxConnectCandidates]
	}
	var lastErr error
	for _, endpoint := range candidates {
		session, err := u.connectTo(ctx, endpoint, opts)
		if err != nil {
			u.endpoints.markDown(endpoint)
			u.log.Warn("upstream connect failed", "endpoint", endpoint, "err", err)
			lastErr = err
			continue
		}
		u.endpoints.markUp(endpoint)
		u.log.Debug("upstream connected", "endpoint", endpoint)
		return session, nil
	}
	return nil, fmt.Errorf("connect upstream %q: %w", u.cfg.ID, lastErr)
}

// connectTo establishes one session against a specific endpoint.
func (u *upstream) connectTo(ctx context.Context, endpoint string, opts *mcp.ClientOptions) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "fold-gateway", Version: version}, opts)
	// Register the federated task methods so the client may forward them to
	// this upstream as opaque custom methods.
	for _, method := range taskMethods {
		if err := mcp.AddSendingCustomMethod[*rawParams, *rawResult](client, method); err != nil {
			return nil, fmt.Errorf("register task method %q: %w", method, err)
		}
	}
	cctx, cancel := context.WithTimeout(ctx, u.connectTimeout)
	defer cancel()
	return client.Connect(cctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: u.httpClient,
	}, nil)
}

// chargeBudgets consumes one upstream invocation from the per-upstream,
// per-tenant, and server allowances, returning a wire error when any is
// exhausted.
//
// Charged narrowest first: the per-upstream budget names the upstream the
// caller actually asked for, the tenant's covers that customer across every
// upstream, and the server's is the widest net. An allowance that refuses does
// not spend the wider ones — being refused by one must not spend another.
func (u *upstream) chargeBudgets(ctx context.Context) *jsonrpc.Error {
	if r := u.budget.Add(ctx, 1); !r.Allowed {
		u.noteDegraded(r, "upstream")
		return budgetError(fmt.Sprintf("upstream %q", u.cfg.ID), r)
	}
	// The tenant rides the request context, so it is whatever this caller
	// resolved to — nil for an untenanted caller, which is governed exactly
	// as before tenancy existed.
	if t := tenantFrom(ctx); t != nil && t.budget != nil {
		if r := t.budget.Add(ctx, 1); !r.Allowed {
			u.noteDegraded(r, "tenant")
			return budgetError(fmt.Sprintf("tenant %q", t.id()), r)
		}
	}
	if u.globalBudget != nil {
		if r := u.globalBudget.Add(ctx, 1); !r.Allowed {
			u.noteDegraded(r, "server")
			return budgetError("server", r)
		}
	}
	return nil
}

// noteDegraded surfaces a budget decision made without shared state. Failing
// open is deliberate — a Redis blip must not refuse all service — but a fleet
// running unbudgeted has to be visible rather than inferred, so it is counted
// and logged rather than passed over in silence.
func (u *upstream) noteDegraded(r state.BudgetResult, scope string) {
	if !r.Degraded {
		return
	}
	if u.metrics != nil {
		u.metrics.observeBudgetDegraded(scope)
	}
	u.log.Warn("budget decided without shared state", "scope", scope)
}

// budgetError renders an exhausted allowance. It reports when the window
// resets rather than a retry delay: the answer to an exhausted monthly budget
// is "not until the 1st", and a client treating it as a backoff would sleep
// for a fortnight.
func budgetError(scope string, r state.BudgetResult) *jsonrpc.Error {
	return &jsonrpc.Error{
		Code: codeBudgetExhausted,
		Message: fmt.Sprintf("%s budget exhausted (%d/%d); resets %s",
			scope, r.Used, r.Limit, r.Resets.UTC().Format(time.RFC3339)),
	}
}

// startHealthProbes begins the active endpoint probe loop when healthCheck
// is configured. Called after the upstream is wired (logger set), so the
// goroutine never races construction. Idempotent.
func (u *upstream) startHealthProbes() {
	hc := u.cfg.HealthCheck
	if hc == nil || hc.IntervalMs <= 0 || u.probeStop != nil {
		return
	}
	// A probe runs on no caller's behalf, so an upstream whose credential is
	// derived from the caller has nothing to present: every probe would fail
	// at Apply and eject every endpoint, taking the upstream down for the
	// clients it works perfectly well for. Skipped rather than rejected at
	// validation, so an existing config keeps serving.
	if u.callerDerived() {
		u.log.Warn("ignoring healthCheck: this upstream's credential is derived from the caller, so it cannot be probed without one",
			"strategy", u.cfg.Auth.Strategy)
		return
	}
	interval := hc.IntervalMs
	// The same floor discovered upstreams get (clampDiscoveredUpstreams),
	// applied to static config: a probe is a full MCP handshake per endpoint
	// per interval, and a typo'd "1" would make the gateway a flood source
	// against its own upstream. Clamped rather than rejected so an existing
	// aggressive config keeps probing, just not pathologically.
	if interval < minProbeIntervalMs {
		u.log.Warn("clamping health-probe interval", "requested", interval, "floor", minProbeIntervalMs)
		interval = minProbeIntervalMs
	}
	u.probeStop = make(chan struct{})
	go u.probeLoop(time.Duration(interval) * time.Millisecond)
}

// probeLoop probes every endpoint once immediately — a replica that is
// already dead is ejected before the first client request — then on each
// interval until the upstream closes.
func (u *upstream) probeLoop(interval time.Duration) {
	u.safeProbe()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			u.safeProbe()
		case <-u.probeStop:
			return
		}
	}
}

// safeProbe runs one probe round, dropping a poisoned round rather than
// ending the loop (or, unrecovered, the process — see rescue.go).
func (u *upstream) safeProbe() {
	defer u.rescue("probe")
	u.probeEndpoints()
}

// probeEndpoints connects to each endpoint concurrently (a full MCP
// handshake — the only honest liveness check an MCP server offers) and
// updates the balancer: failures eject, successes restore, transitions log.
// Probe sessions are separate from the root and bridged sessions and are
// closed immediately.
func (u *upstream) probeEndpoints() {
	var wg sync.WaitGroup
	for _, endpoint := range u.endpoints.all() {
		wg.Go(func() {
			defer u.rescue("probe") // own goroutine; see rescue.go
			ctx, cancel := context.WithTimeout(context.Background(), u.connectTimeout)
			defer cancel()
			session, err := u.connectTo(ctx, endpoint, &mcp.ClientOptions{})
			if err != nil {
				if u.endpoints.markDown(endpoint) {
					u.log.Warn("health probe ejected endpoint", "endpoint", endpoint, "err", err)
				}
				return
			}
			_ = session.Close()
			if u.endpoints.markUp(endpoint) {
				u.log.Info("health probe restored endpoint", "endpoint", endpoint)
			}
		})
	}
	wg.Wait()
}

// callerDerived reports whether this upstream's credential comes from the
// caller (passthrough, token-exchange) rather than from the gateway's own
// configuration. Such an upstream can only be reached on behalf of an
// authenticated principal, which is what makes the credential-free paths —
// health probes above all — unable to speak to it at all.
func (u *upstream) callerDerived() bool { return u.creds.PerRequest() }

// rootSession connects (or reuses) the shared upstream session. List-changed
// notifications invalidate the list caches; resource-updated notifications
// are forwarded so subscribed clients hear them.
func (u *upstream) rootSession(ctx context.Context) (*mcp.ClientSession, error) {
	u.mu.Lock()
	if u.session != nil {
		s := u.session
		u.mu.Unlock()
		return s, nil
	}
	resub := make([]string, 0, len(u.subscribed))
	for uri := range u.subscribed {
		resub = append(resub, uri)
	}
	u.mu.Unlock()

	notifyDownstream := func(kind string) {
		if u.onListChanged != nil {
			u.onListChanged(kind)
		}
	}
	// These run on the SDK client's read-loop goroutine off upstream-supplied
	// input, with no recovery above them — and an upstream that panics its
	// handler on every notification would otherwise crash-loop the gateway.
	// See rescue.go.
	opts := &mcp.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			defer u.rescue("notify")
			u.lists.Invalidate(ctx, "tools")
			notifyDownstream("tools")
		},
		PromptListChangedHandler: func(ctx context.Context, _ *mcp.PromptListChangedRequest) {
			defer u.rescue("notify")
			u.lists.Invalidate(ctx, "prompts")
			notifyDownstream("prompts")
		},
		ResourceListChangedHandler: func(ctx context.Context, _ *mcp.ResourceListChangedRequest) {
			defer u.rescue("notify")
			u.lists.Invalidate(ctx, "resources")
			notifyDownstream("resources")
		},
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			defer u.rescue("notify")
			if u.onResourceUpdated != nil {
				u.onResourceUpdated(ctx, req.Params)
			}
		},
	}
	session, err := u.connect(ctx, opts)
	if err != nil {
		return nil, err
	}

	u.mu.Lock()
	if u.session != nil {
		// Lost the race; use the winner.
		s := u.session
		u.mu.Unlock()
		_ = session.Close()
		return s, nil
	}
	u.session = session
	u.mu.Unlock()

	// Restore upstream subscriptions lost with the previous session.
	for _, uri := range resub {
		_ = session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri}) // best effort
	}
	return session, nil
}

// bridgeOpts builds the client options for a bridged session on demand. It is
// only called when a session must actually be connected — see bridgeFor.
type bridgeOpts func() *mcp.ClientOptions

// build resolves the options, tolerating a nil thunk (unbridged callers).
func (b bridgeOpts) build() *mcp.ClientOptions {
	if b == nil {
		return nil
	}
	return b()
}

// bridgedSession connects (or reuses) the per-client session for downstream
// session key. opts builds the handlers that forward server-initiated
// traffic (sampling, elicitation, logging, progress) back to that client;
// it is resolved only on the connect path.
func (u *upstream) bridgedSession(ctx context.Context, key string, opts bridgeOpts) (*mcp.ClientSession, error) {
	u.mu.Lock()
	if e := u.bridged[key]; e != nil {
		e.lastUsed = time.Now()
		s := e.session
		u.mu.Unlock()
		return s, nil
	}
	u.mu.Unlock()

	session, err := u.connect(ctx, opts.build())
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	if e := u.bridged[key]; e != nil {
		e.lastUsed = time.Now()
		s := e.session
		u.mu.Unlock()
		_ = session.Close()
		return s, nil
	}
	u.bridged[key] = &bridgedEntry{session: session, lastUsed: time.Now()}
	u.mu.Unlock()
	return session, nil
}

func (u *upstream) dropSession(s *mcp.ClientSession) {
	u.mu.Lock()
	dropped := false
	if u.session == s {
		u.session = nil
		dropped = true
	}
	for key, e := range u.bridged {
		if e.session == s {
			delete(u.bridged, key)
			dropped = true
		}
	}
	u.mu.Unlock()
	if dropped {
		u.log.Warn("dropped upstream session after transport error; will reconnect on next request")
	}
	if s != nil {
		_ = s.Close()
	}
}

// subscribedURIs returns the URIs subscribed on the root session (used to
// carry subscriptions over to a replacement upstream on reload).
func (u *upstream) subscribedURIs() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	uris := make([]string, 0, len(u.subscribed))
	for uri := range u.subscribed {
		uris = append(uris, uri)
	}
	return uris
}

// isSubscribed reports whether the root session holds a subscription for uri.
func (u *upstream) isSubscribed(uri string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.subscribed[uri]
}

// bridgedCount reports how many per-client sessions this upstream currently
// holds (scraped by sessionCollector).
func (u *upstream) bridgedCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.bridged)
}

// sweepBridged closes bridged sessions idle longer than bridgedIdleTimeout.
func (u *upstream) sweepBridged() {
	u.mu.Lock()
	var stale []*mcp.ClientSession
	for key, e := range u.bridged {
		if time.Since(e.lastUsed) > bridgedIdleTimeout {
			stale = append(stale, e.session)
			delete(u.bridged, key)
		}
	}
	u.mu.Unlock()
	for _, s := range stale {
		_ = s.Close()
	}
}

// Close shuts all held sessions down and stops the health-probe loop.
func (u *upstream) Close() {
	if u.probeStop != nil {
		u.stopProbeOnce.Do(func() { close(u.probeStop) })
	}
	u.mu.Lock()
	sessions := []*mcp.ClientSession{u.session}
	u.session = nil
	for _, e := range u.bridged {
		sessions = append(sessions, e.session)
	}
	u.bridged = map[string]*bridgedEntry{}
	u.mu.Unlock()
	for _, s := range sessions {
		if s != nil {
			_ = s.Close()
		}
	}
}

// do runs one guarded request on the root session.
func (u *upstream) do(ctx context.Context, fn func(context.Context, *mcp.ClientSession) error) error {
	return u.guardedDo(ctx, u.rootSession, fn)
}

// doBridged runs one guarded request on the per-client session for key.
func (u *upstream) doBridged(ctx context.Context, key string, opts bridgeOpts, fn func(context.Context, *mcp.ClientSession) error) error {
	if key == "" {
		return u.do(ctx, fn)
	}
	return u.guardedDo(ctx, func(ctx context.Context) (*mcp.ClientSession, error) {
		return u.bridgedSession(ctx, key, opts)
	}, fn)
}

// guardedDo applies the per-upstream guards around fn: rate limit, circuit
// breaker, request timeout, session drop on transport failure.
func (u *upstream) guardedDo(ctx context.Context, acquire func(context.Context) (*mcp.ClientSession, error), fn func(context.Context, *mcp.ClientSession) error) error {
	start := time.Now()
	ctx, span := u.tracer.startClient(ctx, u.cfg.ID)
	observe := func(outcome string) {
		if u.metrics != nil {
			u.metrics.observeUpstream(u.cfg.ID, outcome, time.Since(start))
		}
		endClient(span, outcome)
	}
	if ok, retry := u.limiter.Allow(ctx); !ok {
		observe("rate_limited")
		return &jsonrpc.Error{
			Code:    codeRateLimited,
			Message: fmt.Sprintf("upstream %q rate limit exceeded; retry in %s", u.cfg.ID, retry.Round(time.Second)),
		}
	}
	if !u.breaker.Allow(ctx) {
		observe("circuit_open")
		return &jsonrpc.Error{
			Code:    codeUpstreamDown,
			Message: fmt.Sprintf("upstream %q unavailable (circuit open)", u.cfg.ID),
		}
	}
	session, err := acquire(ctx)
	if err != nil {
		u.breaker.Record(ctx, false)
		observe("connect_error")
		// The client-facing message is generic on purpose: a raw connect
		// error names internal endpoints and can name secret env vars, the
		// same strings /health and /api/federation already redact for
		// untrusted callers. The details are in the gateway log (connect
		// warns per endpoint as it fails).
		return &jsonrpc.Error{
			Code:    codeUpstreamDown,
			Message: fmt.Sprintf("upstream %q unavailable (connect failed; details in gateway logs)", u.cfg.ID),
		}
	}
	// Budgets are checked here, after the session is in hand, so consumption
	// counts only invocations that actually reach the upstream. Checking
	// earlier would charge the allowance for requests a rate limit, an open
	// circuit, or a failed connect stopped — and a month-long outage would
	// then burn a month's budget on calls nobody served.
	if err := u.chargeBudgets(ctx); err != nil {
		observe("budget_exhausted")
		return err
	}
	// Counted here, next to the budget charge and for the same reason: this is
	// the point at which an invocation is really made, so the metered cost and
	// the billed cost cannot drift apart.
	countUpstreamCall(ctx)
	rctx, cancel := context.WithTimeout(ctx, u.requestTimeout)
	defer cancel()
	err = fn(rctx, session)
	if err == nil {
		u.recordBreaker(ctx, true)
		observe("ok")
		return nil
	}
	// A JSON-RPC error response proves the upstream is alive: pass it
	// through verbatim and don't count it against the breaker.
	if wire, ok := errors.AsType[*jsonrpc.Error](err); ok {
		u.recordBreaker(ctx, true)
		observe("ok")
		return wire
	}
	u.recordBreaker(ctx, false)
	u.log.Warn("upstream request failed", "err", err)
	if isConnectionError(err) {
		u.dropSession(session)
	}
	observe("error")
	// Generic like the connect branch above — a transport error's text can
	// carry internal hostnames or a credential injector's env-var name.
	// The full error is on the warn line just logged.
	return &jsonrpc.Error{
		Code:    codeUpstreamDown,
		Message: fmt.Sprintf("upstream %q request failed (details in gateway logs)", u.cfg.ID),
	}
}

// recordBreaker reports an outcome to the breaker and logs open/close
// transitions. To avoid extra shared-state reads on the healthy path, the
// breaker state is only inspected around failures and the first success
// after a failure.
func (u *upstream) recordBreaker(ctx context.Context, success bool) {
	if success && !u.hadFailure.Load() {
		u.breaker.Record(ctx, true)
		return
	}
	u.breaker.Record(ctx, success)
	if !success {
		u.hadFailure.Store(true)
	}
	newState := u.breaker.State(ctx)
	if success {
		u.hadFailure.Store(false)
	}
	if prev := u.lastBreakerState.Swap(string(newState)); prev != nil && prev.(string) != string(newState) {
		u.log.Warn("circuit breaker state changed", "from", prev, "to", string(newState))
	}
}

func isConnectionError(err error) bool {
	if errors.Is(err, mcp.ErrConnectionClosed) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection") || strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "broken pipe") || strings.Contains(msg, "refused")
}

// cachedList fetches a list through the upstream's shared cache, which
// stores serialized JSON so entries can live in Redis and be shared across
// gateway instances.
//
// The returned slice and the items it points at are shared with every other
// caller of this list and MUST be treated as read-only: the parsed form is
// memoized per upstream (see loadDecoded), so decoding is paid once per cache
// generation rather than once per request — the difference between ~450 µs
// and ~60 ns on a 200-tool upstream, multiplied by every upstream in the
// federation on every list request. Callers that rewrite a name (egress
// namespacing) copy the item first.
func cachedList[T any](ctx context.Context, u *upstream, key string, fetch func(context.Context, *mcp.ClientSession) ([]T, error)) ([]T, error) {
	data, err := u.lists.GetOrFill(ctx, key, u.cacheTTL, func(ctx context.Context) ([]byte, error) {
		var items []T
		err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
			var err error
			items, err = fetch(ctx, s)
			return err
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(items)
	})
	if err != nil {
		return nil, err
	}
	if items, ok := loadDecoded[T](u, key, data); ok {
		return items, nil
	}
	var items []T
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("decode cached %s for upstream %q: %w", key, u.cfg.ID, err)
	}
	storeDecoded(u, key, data, items)
	return items, nil
}

// loadDecoded returns the memoized parse of data, if the last parse of this
// list was of these exact bytes.
//
// Identity, not equality: the in-process cache hands back the same backing
// array on every hit, so this matches whenever the entry has not been
// refilled — and any refill or Invalidate produces a different array, which
// misses and re-decodes. That makes the memo self-invalidating, with no
// second invalidation path to keep in sync with the cache's own. The Redis
// provider decodes a fresh slice per call, so it always misses and keeps
// exactly today's behavior.
func loadDecoded[T any](u *upstream, key string, data []byte) ([]T, bool) {
	if !u.memoizable(data) {
		return nil, false
	}
	u.decodedMu.Lock()
	d, ok := u.decoded[key]
	u.decodedMu.Unlock()
	if !ok || !sameBytes(d.bytes, data) {
		return nil, false
	}
	items, ok := d.items.([]T)
	return items, ok
}

func storeDecoded[T any](u *upstream, key string, data []byte, items []T) {
	if !u.memoizable(data) {
		return
	}
	u.decodedMu.Lock()
	defer u.decodedMu.Unlock()
	if u.decoded == nil {
		u.decoded = map[string]decodedList{}
	}
	u.decoded[key] = decodedList{bytes: data, items: items}
}

// memoizable reports whether a fetched list may be memoized. An upstream with
// caching disabled fetches fresh bytes per request — which is the point when
// the credential is caller-derived and the list is per-user — so nothing is
// retained for it.
func (u *upstream) memoizable(data []byte) bool {
	return u.cacheTTL > 0 && len(data) > 0
}

// sameBytes reports whether two slices share a backing array and length.
func sameBytes(a, b []byte) bool {
	return len(a) == len(b) && len(a) > 0 && &a[0] == &b[0]
}

// publicName rewrites a bare upstream name into the namespaced name clients
// see. An upstream without a namespace runs passthrough and is never
// rewritten.
func (u *upstream) publicName(bare string) string {
	if u.cfg.Namespace == "" {
		return bare
	}
	return u.cfg.Namespace + u.sep + bare
}

// namespacedTools returns the tool list as clients see it — names rewritten
// to {namespace}{sep}{name} — index-aligned with bare, so a caller can decide
// visibility on the bare name and emit the public item at the same index.
//
// The rewrite used to run per request, copying every tool and building every
// name again for a result that only changes when the cache refills. Both the
// returned slice and its items are shared and read-only, like everything else
// from the list path (see cachedList).
func (u *upstream) namespacedTools(bare []*mcp.Tool) []*mcp.Tool {
	if u.cfg.Namespace == "" {
		return bare // passthrough: names are already public, nothing to rewrite
	}
	u.publicMu.Lock()
	defer u.publicMu.Unlock()
	if items, ok := u.publicTools.get(bare); ok {
		return items
	}
	items := make([]*mcp.Tool, len(bare))
	for i, t := range bare {
		nt := *t
		nt.Name = u.publicName(t.Name)
		items[i] = &nt
	}
	u.publicTools = publicView[mcp.Tool]{from: bare, items: items}
	return items
}

// namespacedPrompts is namespacedTools for prompts.
func (u *upstream) namespacedPrompts(bare []*mcp.Prompt) []*mcp.Prompt {
	if u.cfg.Namespace == "" {
		return bare
	}
	u.publicMu.Lock()
	defer u.publicMu.Unlock()
	if items, ok := u.publicPrompts.get(bare); ok {
		return items
	}
	items := make([]*mcp.Prompt, len(bare))
	for i, p := range bare {
		np := *p
		np.Name = u.publicName(p.Name)
		items[i] = &np
	}
	u.publicPrompts = publicView[mcp.Prompt]{from: bare, items: items}
	return items
}

// annotationsFor resolves one tool's annotations for a toolKind rule, from
// the list this upstream caches. It reports false when they cannot be
// established, which policy treats as a denial.
//
// Two ways that happens, both deliberate. An upstream whose credential is
// caller-derived has list caching disabled — one caller's list must never
// serve another — so there is nothing to consult and no per-call round trip
// will be made to invent one: toolKind and passthrough/token-exchange do not
// compose, and the refusal says so by happening. And a tool the upstream does
// not list cannot be judged, which is the same answer for the same reason.
//
// The scan is linear over a cached, already-decoded slice, on a path that is
// about to make a network call to the upstream anyway.
func (u *upstream) annotationsFor(ctx context.Context, bare string) (policy.ToolAnnotations, bool) {
	if u.cacheTTL <= 0 {
		return policy.ToolAnnotations{}, false
	}
	tools, err := u.listTools(ctx)
	if err != nil {
		return policy.ToolAnnotations{}, false
	}
	for _, t := range tools {
		if t != nil && t.Name == bare {
			return toolAnnotations(t)
		}
	}
	return policy.ToolAnnotations{}, false
}

// listTools returns the upstream's full (un-namespaced) tool list, cached.
func (u *upstream) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	return cachedList(ctx, u, "tools", func(ctx context.Context, s *mcp.ClientSession) ([]*mcp.Tool, error) {
		var tools []*mcp.Tool
		for t, err := range s.Tools(ctx, nil) {
			if err != nil {
				return nil, err
			}
			tools = append(tools, t)
		}
		// Inside the fill, so the comparison happens once per cache
		// generation rather than once per request: this is the refill that
		// just paid for a round trip and a decode, and the only moment the
		// definitions can have changed.
		u.pins.checkTools(ctx, tools)
		return tools, nil
	})
}

func (u *upstream) listPrompts(ctx context.Context) ([]*mcp.Prompt, error) {
	return cachedList(ctx, u, "prompts", func(ctx context.Context, s *mcp.ClientSession) ([]*mcp.Prompt, error) {
		var prompts []*mcp.Prompt
		for p, err := range s.Prompts(ctx, nil) {
			if err != nil {
				return nil, err
			}
			prompts = append(prompts, p)
		}
		u.pins.checkPrompts(ctx, prompts)
		return prompts, nil
	})
}

func (u *upstream) listResources(ctx context.Context) ([]*mcp.Resource, error) {
	return cachedList(ctx, u, "resources", func(ctx context.Context, s *mcp.ClientSession) ([]*mcp.Resource, error) {
		var res []*mcp.Resource
		for r, err := range s.Resources(ctx, nil) {
			if err != nil {
				return nil, err
			}
			res = append(res, r)
		}
		return res, nil
	})
}

func (u *upstream) listResourceTemplates(ctx context.Context) ([]*mcp.ResourceTemplate, error) {
	return cachedList(ctx, u, "resources/templates", func(ctx context.Context, s *mcp.ClientSession) ([]*mcp.ResourceTemplate, error) {
		var res []*mcp.ResourceTemplate
		for r, err := range s.ResourceTemplates(ctx, nil) {
			if err != nil {
				return nil, err
			}
			res = append(res, r)
		}
		return res, nil
	})
}

func (u *upstream) callTool(ctx context.Context, key string, opts bridgeOpts, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	var out *mcp.CallToolResult
	err := u.doBridged(ctx, key, opts, func(ctx context.Context, s *mcp.ClientSession) error {
		var err error
		out, err = s.CallTool(ctx, params)
		return err
	})
	return out, err
}

func (u *upstream) getPrompt(ctx context.Context, key string, opts bridgeOpts, params *mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	var out *mcp.GetPromptResult
	err := u.doBridged(ctx, key, opts, func(ctx context.Context, s *mcp.ClientSession) error {
		var err error
		out, err = s.GetPrompt(ctx, params)
		return err
	})
	return out, err
}

func (u *upstream) readResource(ctx context.Context, params *mcp.ReadResourceParams) (*mcp.ReadResourceResult, error) {
	var out *mcp.ReadResourceResult
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		var err error
		out, err = s.ReadResource(ctx, params)
		return err
	})
	return out, err
}

func (u *upstream) complete(ctx context.Context, params *mcp.CompleteParams) (*mcp.CompleteResult, error) {
	var out *mcp.CompleteResult
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		var err error
		out, err = s.Complete(ctx, params)
		return err
	})
	return out, err
}

// subscribe subscribes the root session to uri, remembering it for
// resubscription after a reconnect.
func (u *upstream) subscribe(ctx context.Context, uri string) error {
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		return s.Subscribe(ctx, &mcp.SubscribeParams{URI: uri})
	})
	if err == nil {
		u.mu.Lock()
		u.subscribed[uri] = true
		u.mu.Unlock()
	}
	return err
}

func (u *upstream) unsubscribe(ctx context.Context, uri string) error {
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		return s.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: uri})
	})
	if err == nil {
		u.mu.Lock()
		delete(u.subscribed, uri)
		u.mu.Unlock()
	}
	return err
}

// setLoggingLevel propagates a client's logging level to its bridged
// session (logging notifications flow back per client).
func (u *upstream) setLoggingLevel(ctx context.Context, key string, opts bridgeOpts, level mcp.LoggingLevel) error {
	return u.doBridged(ctx, key, opts, func(ctx context.Context, s *mcp.ClientSession) error {
		return s.SetLoggingLevel(ctx, &mcp.SetLoggingLevelParams{Level: level})
	})
}

func (u *upstream) ping(ctx context.Context) error {
	return u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		return s.Ping(ctx, nil)
	})
}

// callTask forwards an opaque task method to this upstream on the root
// session. A JSON-RPC error (e.g. the upstream not owning the task) passes
// through as a wire error; only transport failures count against the breaker.
func (u *upstream) callTask(ctx context.Context, method string, raw json.RawMessage) (json.RawMessage, error) {
	var out json.RawMessage
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		res, err := mcp.CallCustomMethod[*rawParams, *rawResult](ctx, s, method, &rawParams{raw: raw})
		if err != nil {
			return err
		}
		out = res.raw
		return nil
	})
	return out, err
}
