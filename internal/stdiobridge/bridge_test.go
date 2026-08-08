package stdiobridge_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/internal/stdiobridge"
)

// The tests run a real MCP stdio server: the test binary re-execs itself with
// serverEnv set, and TestMain serves stdio instead of running tests. Per repo
// rule 1, the peer behind the bridge is the official SDK's server, not a
// hand-rolled fixture.
const (
	serverEnv = "FOLD_STDIOBRIDGE_TEST_SERVER"
	markerEnv = "FOLD_STDIOBRIDGE_TEST_MARKER"
	secretEnv = "FOLD_STDIOBRIDGE_TEST_SECRET"
)

func TestMain(m *testing.M) {
	if os.Getenv(serverEnv) == "" {
		os.Exit(m.Run())
	}
	if err := runStdioServer(); err != nil {
		fmt.Fprintln(os.Stderr, "stdio server:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

type echoArgs struct {
	Text string `json:"text"`
}

// runStdioServer serves one SDK MCP server over stdin/stdout.
func runStdioServer() error {
	if mode := os.Getenv(serverEnv); mode == "crash" {
		os.Exit(3)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "test-stdio", Version: "v1"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "echo text back"},
		func(ctx context.Context, req *mcp.CallToolRequest, args echoArgs) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: args.Text}},
			}, nil, nil
		})
	// Reports what the child can see of the environment, so a test can prove
	// the bridge does not leak its own.
	mcp.AddTool(s, &mcp.Tool{Name: "env", Description: "report env visibility"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{
				Text: "marker=" + os.Getenv(markerEnv) + " secret=" + os.Getenv(secretEnv),
			}}}, nil, nil
		})
	// Asks the client to sample: a server-initiated request, which only
	// arrives if the bridge pumps both directions.
	mcp.AddTool(s, &mcp.Tool{Name: "ask", Description: "ask the client to sample"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			res, err := req.Session.CreateMessage(ctx, &mcp.CreateMessageParams{MaxTokens: 10})
			if err != nil {
				return nil, nil, err
			}
			text := ""
			if tc, ok := res.Content.(*mcp.TextContent); ok {
				text = tc.Text
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sampled:" + text}}}, nil, nil
		})
	// Reports the pid so a test can prove per-session process isolation.
	mcp.AddTool(s, &mcp.Tool{Name: "pid", Description: "report the server pid"},
		func(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{
				&mcp.TextContent{Text: fmt.Sprint(os.Getpid())},
			}}, nil, nil
		})
	return s.Run(context.Background(), &mcp.StdioTransport{})
}

// selfCommand returns the argv that re-execs this test binary as the server.
func selfCommand(t *testing.T) []string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	return []string{exe}
}

func serverEnviron(mode string, extra ...string) []string {
	return append([]string{serverEnv + "=" + mode}, extra...)
}

// newBridge starts a bridge in front of the test stdio server.
func newBridge(t *testing.T, opts stdiobridge.Options) (*httptest.Server, *stdiobridge.Bridge) {
	t.Helper()
	if opts.Command == nil {
		opts.Command = selfCommand(t)
	}
	if opts.Env == nil {
		opts.Env = serverEnviron("1")
	}
	b, err := stdiobridge.New(opts)
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	srv := httptest.NewServer(b)
	t.Cleanup(func() {
		srv.Close()
		_ = b.Close()
	})
	return srv, b
}

// connect drives the bridge with a real SDK streamable-HTTP client.
func connect(t *testing.T, url string, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, opts)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

func callText(t *testing.T, sess *mcp.ClientSession, name string, args any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("call %s: no content", name)
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("call %s: content is %T, want text", name, res.Content[0])
	}
	return tc.Text
}

// TestRoundTrip is the load-bearing test: a real SDK client, over streamable
// HTTP, against a real SDK stdio server, with only the bridge between them.
func TestRoundTrip(t *testing.T) {
	srv, _ := newBridge(t, stdiobridge.Options{})
	sess := connect(t, srv.URL, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tools, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(tools.Tools) == 0 {
		t.Fatal("no tools through the bridge")
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	if !contains(names, "echo") {
		t.Fatalf("tools = %v, want echo", names)
	}

	if got := callText(t, sess, "echo", echoArgs{Text: "hello"}); got != "hello" {
		t.Fatalf("echo = %q, want %q", got, "hello")
	}
}

// TestInitializeReportsChildImplementation proves the bridge is invisible: the
// client sees the stdio server's own identity, not the bridge's.
func TestInitializeReportsChildImplementation(t *testing.T) {
	srv, _ := newBridge(t, stdiobridge.Options{})
	sess := connect(t, srv.URL, nil)
	impl := sess.InitializeResult().ServerInfo
	if impl.Name != "test-stdio" {
		t.Fatalf("server name = %q, want %q — the bridge is not transparent", impl.Name, "test-stdio")
	}
}

// TestServerInitiatedSampling proves the pump is bidirectional: a request the
// server originates must reach the client and its reply must return.
func TestServerInitiatedSampling(t *testing.T) {
	srv, _ := newBridge(t, stdiobridge.Options{})
	sess := connect(t, srv.URL, &mcp.ClientOptions{
		CreateMessageHandler: func(ctx context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			return &mcp.CreateMessageResult{
				Model:   "test-model",
				Role:    "assistant",
				Content: &mcp.TextContent{Text: "pong"},
			}, nil
		},
	})
	if got := callText(t, sess, "ask", struct{}{}); got != "sampled:pong" {
		t.Fatalf("ask = %q, want %q", got, "sampled:pong")
	}
}

// TestEnvIsAllowlisted proves the child gets only the named variables and
// never inherits the bridge's own environment.
func TestEnvIsAllowlisted(t *testing.T) {
	t.Setenv(markerEnv, "visible")
	t.Setenv(secretEnv, "do-not-leak")

	srv, _ := newBridge(t, stdiobridge.Options{
		Env: serverEnviron("1", stdiobridge.BuildEnv([]string{markerEnv})...),
	})
	sess := connect(t, srv.URL, nil)
	got := callText(t, sess, "env", struct{}{})
	if !strings.Contains(got, "marker=visible") {
		t.Fatalf("env = %q, want the allowlisted marker", got)
	}
	if !strings.Contains(got, "secret=") || strings.Contains(got, "do-not-leak") {
		t.Fatalf("env = %q — the bridge leaked a variable outside the allowlist", got)
	}
}

// TestSessionsGetSeparateProcesses proves the default isolation: two sessions
// must not share a child, because stdio servers hold per-client state.
func TestSessionsGetSeparateProcesses(t *testing.T) {
	srv, b := newBridge(t, stdiobridge.Options{})
	a := connect(t, srv.URL, nil)
	c := connect(t, srv.URL, nil)

	pidA := callText(t, a, "pid", struct{}{})
	pidC := callText(t, c, "pid", struct{}{})
	if pidA == pidC {
		t.Fatalf("both sessions served by pid %s — sessions are sharing a process", pidA)
	}
	if got := b.Stats().Sessions; got != 2 {
		t.Fatalf("sessions = %d, want 2", got)
	}
}

// TestConcurrentSessionsDoNotCrossTalk pins the invariant that killed the
// shared-process option: two sessions must be independently correct. A stdio
// connection carries exactly one MCP session, so any sharing puts two id
// spaces on one pipe and the pumps steal each other's replies. Interleaving
// calls across two sessions catches that immediately.
func TestConcurrentSessionsDoNotCrossTalk(t *testing.T) {
	srv, _ := newBridge(t, stdiobridge.Options{})
	a := connect(t, srv.URL, nil)
	c := connect(t, srv.URL, nil)

	for i := range 5 {
		wantA := fmt.Sprintf("a-%d", i)
		wantC := fmt.Sprintf("c-%d", i)
		if got := callText(t, a, "echo", echoArgs{Text: wantA}); got != wantA {
			t.Fatalf("session A echo = %q, want %q", got, wantA)
		}
		if got := callText(t, c, "echo", echoArgs{Text: wantC}); got != wantC {
			t.Fatalf("session C echo = %q, want %q", got, wantC)
		}
	}
}

// TestMaxSessionsRefuses proves the bound the gateway could not have provided,
// and that refusal is a 503 the breaker can read.
func TestMaxSessionsRefuses(t *testing.T) {
	srv, b := newBridge(t, stdiobridge.Options{MaxSessions: 1})
	connect(t, srv.URL, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "overflow", Version: "v1"}, nil)
	_, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err == nil {
		t.Fatal("second session connected, want refusal at the bound")
	}
	if b.Stats().Rejected == 0 {
		t.Fatal("refusal not counted in Stats")
	}
}

// TestSpawnFailureIsVisible proves a server that cannot start surfaces as a
// failure rather than a hang — this is what lets the gateway's breaker eject
// the endpoint.
func TestSpawnFailureIsVisible(t *testing.T) {
	srv, b := newBridge(t, stdiobridge.Options{
		Command: []string{"/nonexistent/fold-stdio-test-binary"},
	})
	// A well-formed initialize, so the request reaches the spawn path rather
	// than being refused as malformed.
	res := post(t, srv.URL, "application/json",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`, nil)
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusBadGateway)
	}
	if b.Stats().SpawnErrors == 0 {
		t.Fatal("spawn failure not counted in Stats")
	}
}

// TestProbeReportsHealth proves /health can distinguish a runnable server from
// one that is missing.
func TestProbeReportsHealth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, good := newBridge(t, stdiobridge.Options{})
	if err := good.Probe(ctx); err != nil {
		t.Fatalf("probe of a runnable server failed: %v", err)
	}

	_, bad := newBridge(t, stdiobridge.Options{Command: []string{"/nonexistent/fold-stdio-test-binary"}})
	if err := bad.Probe(ctx); err == nil {
		t.Fatal("probe of a missing server succeeded, want failure")
	}
}

// TestDeleteEndsSession proves DELETE tears the session down and reaps it.
func TestDeleteEndsSession(t *testing.T) {
	srv, b := newBridge(t, stdiobridge.Options{})
	sess := connect(t, srv.URL, nil)
	callText(t, sess, "echo", echoArgs{Text: "x"}) // force the session open

	if got := b.Stats().Sessions; got != 1 {
		t.Fatalf("sessions = %d, want 1", got)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return b.Stats().Sessions == 0 })
}

// TestCloseLeavesNoOrphans proves shutdown reaps every child.
func TestCloseLeavesNoOrphans(t *testing.T) {
	srv, b := newBridge(t, stdiobridge.Options{})
	a := connect(t, srv.URL, nil)
	c := connect(t, srv.URL, nil)
	pidA := callText(t, a, "pid", struct{}{})
	pidC := callText(t, c, "pid", struct{}{})

	if err := b.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	for _, pid := range []string{pidA, pidC} {
		waitFor(t, 10*time.Second, func() bool { return !processAlive(pid) })
	}
}

// TestChildCrashEndsSessionCleanly proves a server that dies mid-session
// surfaces as a closed session rather than a hang.
func TestChildCrashEndsSessionCleanly(t *testing.T) {
	srv, b := newBridge(t, stdiobridge.Options{Env: serverEnviron("crash")})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v1"}, nil)
	sess, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL}, nil)
	if err == nil {
		_ = sess.Close()
		t.Fatal("connected to a server that exits immediately, want failure")
	}
	// The session must not be left behind holding a dead process.
	waitFor(t, 10*time.Second, func() bool { return b.Stats().Sessions == 0 })
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", limit)
}

// processAlive reports whether a pid is still running (and not a zombie).
func processAlive(pid string) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", pid).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return false // ps exits non-zero when the pid is gone
		}
		return false
	}
	stat := strings.TrimSpace(string(out))
	return stat != "" && !strings.HasPrefix(stat, "Z")
}

// post sends a raw POST to the bridge and returns the response.
func post(t *testing.T, url, contentType, body string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// The shim connects the SDK transport directly, which skips the inbound checks
// the SDK keeps in its own handler. A loopback listener must still refuse a
// foreign Host, or any page that can rebind DNS drives the local server.
func TestRejectsDNSRebinding(t *testing.T) {
	srv, _ := newBridge(t, stdiobridge.Options{})
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Host = "evil.example.com"
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want %d for a foreign Host on a loopback listener", res.StatusCode, http.StatusForbidden)
	}
}

// A non-JSON Content-Type would make a cross-origin POST a CORS "simple"
// request — no preflight — so it must be refused.
func TestRequiresJSONContentType(t *testing.T) {
	srv, b := newBridge(t, stdiobridge.Options{})
	res := post(t, srv.URL, "text/plain", `{"jsonrpc":"2.0","id":1,"method":"initialize"}`, nil)
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want %d", res.StatusCode, http.StatusUnsupportedMediaType)
	}
	if got := b.Stats().Spawned; got != 0 {
		t.Fatalf("spawned = %d, want 0 — a refused request must not start a process", got)
	}
}

// Every session-less POST costs a process, so a body that is not JSON-RPC must
// be refused before spawning rather than handed to a child.
func TestGarbageBodyDoesNotSpawn(t *testing.T) {
	srv, b := newBridge(t, stdiobridge.Options{})
	for _, body := range []string{"", "x", "123", "null"} {
		res := post(t, srv.URL, "application/json", body, nil)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("body %q: status = %d, want %d", body, res.StatusCode, http.StatusBadRequest)
		}
	}
	if got := b.Stats().Spawned; got != 0 {
		t.Fatalf("spawned = %d, want 0 — garbage bodies are a fork/exec amplifier", got)
	}
}

// The body cap must cover session-bearing POSTs too, not just the handshake:
// the SDK transport reads those with io.ReadAll.
func TestBodyCapAppliesToSessionPosts(t *testing.T) {
	srv, _ := newBridge(t, stdiobridge.Options{MaxBodyBytes: 2048})
	sess := connect(t, srv.URL, nil)
	callText(t, sess, "echo", echoArgs{Text: "warm"}) // establish the session

	// Reach into the live session by replaying its id on a raw POST.
	res := post(t, srv.URL, "application/json",
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"echo","arguments":{"text":"`+
			strings.Repeat("A", 8192)+`"}}}`,
		map[string]string{"Mcp-Session-Id": sessionIDOf(t, srv.URL)})
	// Either the oversize body is refused outright, or the session it names is
	// unknown; what must not happen is a 200 carrying 8 KiB past a 2 KiB cap.
	if res.StatusCode == http.StatusOK {
		t.Fatalf("status = 200 — an 8 KiB body passed a 2 KiB cap")
	}
}

// sessionIDOf opens a session and returns its id, for raw-HTTP tests.
func sessionIDOf(t *testing.T, url string) string {
	t.Helper()
	res := post(t, url, "application/json",
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`, nil)
	id := res.Header.Get("Mcp-Session-Id")
	if id == "" {
		t.Fatal("no session id returned from initialize")
	}
	return id
}

// The session ceiling must bound processes, not map entries: releasing a slot
// before the child is reaped lets open-and-abandon run far past the bound.
func TestSlotHeldUntilChildReaped(t *testing.T) {
	srv, b := newBridge(t, stdiobridge.Options{MaxSessions: 1})
	sess := connect(t, srv.URL, nil)
	pid := callText(t, sess, "pid", struct{}{})

	if err := sess.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Once the slot frees, the process it accounted for must already be gone.
	waitFor(t, 15*time.Second, func() bool { return b.Stats().Sessions == 0 })
	if processAlive(pid) {
		t.Fatalf("pid %s still alive after its slot was released — the ceiling bounds entries, not processes", pid)
	}
}

// /health is unauthenticated, so a caller that aborts its request must not be
// able to cache "unhealthy" for every other prober.
func TestHealthIgnoresCallerCancellation(t *testing.T) {
	_, b := newBridge(t, stdiobridge.Options{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already-cancelled: the caller has gone away
	_ = b.Health(ctx)

	// A fresh caller must still get the truth.
	ok, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	if err := b.Health(ok); err != nil {
		t.Fatalf("health = %v after a cancelled probe, want healthy", err)
	}
}

// An abandoned session must not pin its process forever.
func TestIdleSessionsAreSwept(t *testing.T) {
	srv, b := newBridge(t, stdiobridge.Options{IdleTimeout: 300 * time.Millisecond})
	sess := connect(t, srv.URL, nil)
	callText(t, sess, "echo", echoArgs{Text: "x"})
	if got := b.Stats().Sessions; got != 1 {
		t.Fatalf("sessions = %d, want 1", got)
	}
	waitFor(t, 10*time.Second, func() bool { return b.Stats().Sessions == 0 })
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}
