package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// pingCountingClient connects a real SDK client and counts the
// server-initiated pings it answers. The middleware is instrumentation only:
// the client is the SDK's own and answers each ping exactly as any client
// would, so what is being counted is what fold actually put on the wire.
func pingCountingClient(t *testing.T, url string) (*mcp.ClientSession, func() int64) {
	t.Helper()
	var pings atomic.Int64
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	client.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "ping" {
				pings.Add(1)
			}
			return next(ctx, method, req)
		}
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: url + "/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session, pings.Load
}

// TestKeepAlivePingsConnectedClients: with server.keepAliveMs set, fold pings
// each connected client on that interval, so a stream that would otherwise
// carry nothing between calls keeps carrying bytes past an intermediary's
// idle timeout.
//
// The assertion is a floor over a window several intervals long rather than a
// count: the ticker's cadence is the SDK's and a loaded CI machine is allowed
// to be late, but it is not allowed to be silent.
func TestKeepAlivePingsConnectedClients(t *testing.T) {
	// The upstream is wrapped so the test can also see what the keepalive
	// did *not* do: a downstream keepalive is fold's own conversation with
	// its client and must not fan out across the federation.
	var upstreamPings atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo"}}}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			if strings.Contains(string(body), `"method":"ping"`) {
				upstreamPings.Add(1)
			}
			r.Body = io.NopCloser(strings.NewReader(string(body)))
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
		Server:    &config.ServerSection{KeepAliveMs: 25},
	})

	session, pings := pingCountingClient(t, ts.URL)
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool { return pings() >= 2 },
		"no keepalive pings reached the client with server.keepAliveMs set")

	// The session is still usable afterwards: the pings are keeping the
	// stream warm, not consuming it.
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools after keepalive pings: %v", err)
	}
	if got := upstreamPings.Load(); got != 0 {
		t.Errorf("keepalive produced %d upstream pings; the downstream keepalive must not fan out", got)
	}
}

// TestKeepAliveOffByDefault is the frozen-default guard: absent the field,
// fold pings nobody, exactly as every release before it. Both spellings of
// "absent" are covered, because a server section that exists for some other
// reason is the common case.
//
// The window is calibrated rather than slept: a control gateway with the
// field set runs alongside, and the defaults are only checked once the
// control client has answered several pings — so "zero" is measured over a
// window that demonstrably produces pings when the field is on.
func TestKeepAliveOffByDefault(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	upstreams := []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}}

	controlTS, _ := startGateway(t, &config.Config{
		Upstreams: upstreams,
		Server:    &config.ServerSection{KeepAliveMs: 25},
	})
	_, controlPings := pingCountingClient(t, controlTS.URL)

	defaults := map[string]*config.ServerSection{
		"no server section":          nil,
		"server without keepAliveMs": {MaxBodyBytes: 1 << 20},
	}
	type probe struct {
		session *mcp.ClientSession
		pings   func() int64
	}
	probes := map[string]probe{}
	for name, section := range defaults {
		ts, _ := startGateway(t, &config.Config{Upstreams: upstreams, Server: section})
		session, pings := pingCountingClient(t, ts.URL)
		if _, err := session.ListTools(context.Background(), nil); err != nil {
			t.Fatalf("%s: ListTools: %v", name, err)
		}
		probes[name] = probe{session, pings}
	}

	const controlFloor = 4
	waitFor(t, 5*time.Second, func() bool { return controlPings() >= controlFloor },
		"control gateway never pinged: the window proves nothing about the defaults")

	for name, p := range probes {
		if got := p.pings(); got != 0 {
			t.Errorf("%s: %d keepalive pings; the default is off", name, got)
		}
		// A silent client that is silent because it is dead would pass the
		// check above for the wrong reason.
		if _, err := p.session.ListTools(context.Background(), nil); err != nil {
			t.Errorf("%s: session unusable after the window: %v", name, err)
		}
	}
}

// TestKeepAliveResolution pins the config contract: absent, zero and negative
// all mean off, and a positive value is milliseconds. Negative resolving to 0
// rather than to a negative duration is the one that matters — the SDK starts
// its ticker on any non-zero value, and a negative interval there is a panic
// waiting for the config that spells it.
func TestKeepAliveResolution(t *testing.T) {
	cases := []struct {
		name string
		cfg  *config.Config
		want time.Duration
	}{
		{"nil config", nil, 0},
		{"nil server section", &config.Config{}, 0},
		{"absent field", &config.Config{Server: &config.ServerSection{}}, 0},
		{"explicit zero", &config.Config{Server: &config.ServerSection{KeepAliveMs: 0}}, 0},
		{"negative is off", &config.Config{Server: &config.ServerSection{KeepAliveMs: -1}}, 0},
		{"positive", &config.Config{Server: &config.ServerSection{KeepAliveMs: 30000}}, 30 * time.Second},
	}
	for _, tc := range cases {
		if got := tc.cfg.KeepAlive(); got != tc.want {
			t.Errorf("%s: KeepAlive() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestKeepAliveSurvivesAnIdenticalReload: the other direction of the
// construction-wiring rule (TestReloadRejectsNonReloadableSections has the
// refusal). A reload that leaves the server section identical is accepted,
// and the client it is pinging neither loses its session nor its pings —
// swapping the routing snapshot must not disturb the SDK server the ticker
// lives on.
func TestKeepAliveSurvivesAnIdenticalReload(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha_tool")
	upB, _ := newUpstreamServer(t, "beta_tool")
	section := &config.ServerSection{KeepAliveMs: 25}
	ts, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: upA.URL}},
		Server:    section,
	})

	session, pings := pingCountingClient(t, ts.URL)
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return pings() >= 2 }, "no pings before the reload")

	before := pings()
	if err := gw.Reload(&config.Config{
		Upstreams: []config.Upstream{
			{ID: "a", Namespace: "a", URL: upA.URL},
			{ID: "b", Namespace: "b", URL: upB.URL},
		},
		Server: &config.ServerSection{KeepAliveMs: 25},
	}); err != nil {
		t.Fatalf("reload with an identical server section: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool { return pings() > before }, "pings stopped after a reload")
	if got := downstreamSessionCount(gw); got == 0 {
		t.Fatal("the reload took the pinged session with it")
	}
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools after reload: %v", err)
	}
	if len(toolNames(res)) != 2 {
		t.Errorf("tools after reload = %v, want both upstreams", toolNames(res))
	}

	// Changing only the interval is refused, which is the narrowest form of
	// the rule: the document is otherwise the one just accepted.
	err = gw.Reload(&config.Config{
		Upstreams: []config.Upstream{
			{ID: "a", Namespace: "a", URL: upA.URL},
			{ID: "b", Namespace: "b", URL: upB.URL},
		},
		Server: &config.ServerSection{KeepAliveMs: 50},
	})
	if err == nil || !strings.Contains(err.Error(), "server section") {
		t.Errorf("changing only keepAliveMs: Reload should fail naming the server section, got %v", err)
	}
	// And the refusal left the running gateway pinging.
	stillPinging := pings()
	waitFor(t, 5*time.Second, func() bool { return pings() > stillPinging },
		"pings stopped after a rejected reload")
}

// TestKeepAliveClosesASessionThatStopsAnswering: the other half of the
// bargain. An unanswered ping is how a dead client is noticed, so a client
// that stops answering has its session closed rather than left to hold
// gateway state forever.
//
// It takes more than one miss to do it. fold sets KeepAliveFailureThreshold
// to 3 rather than the SDK's close-on-first-failure default, because a
// gateway's clients sit behind whatever network an operator has; the test
// asserts a floor on how long the teardown takes, which a threshold of 1
// could not satisfy (it would close roughly one interval in).
func TestKeepAliveClosesASessionThatStopsAnswering(t *testing.T) {
	const intervalMs = 150
	interval := intervalMs * time.Millisecond

	up, _ := newUpstreamServer(t, "echo")
	ts, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
		Server:    &config.ServerSection{KeepAliveMs: intervalMs},
	})

	// A client that receives pings and never answers them: the fault
	// injection is one blocked handler, everything else is the real SDK
	// client speaking the real protocol.
	var pings atomic.Int64
	release := make(chan struct{})
	client := mcp.NewClient(&mcp.Implementation{Name: "mute-client", Version: "1.0"}, nil)
	client.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "ping" {
				pings.Add(1)
				<-release
			}
			return next(ctx, method, req)
		}
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	// Registered after the session's own cleanup so it runs first: Close
	// cannot finish while a handler goroutine is still parked.
	t.Cleanup(func() { close(release) })

	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	start := time.Now()
	waitFor(t, 10*time.Second, func() bool { return downstreamSessionCount(gw) == 0 },
		"a client that never answers a ping kept its session: keepalive is not reclaiming it")
	elapsed := time.Since(start)

	if elapsed < 2*interval {
		t.Errorf("session closed after %v, under two ping intervals (%v): "+
			"one missed ping should not be enough, KeepAliveFailureThreshold is not 3", elapsed, 2*interval)
	}
	if got := pings.Load(); got < 2 {
		t.Errorf("client saw %d pings before teardown; more than one should have gone unanswered first", got)
	}
}

// TestKeepAliveClosesAClientWithNoListeningStream documents a consequence of
// turning the field on that is not obvious from the field: the standalone GET
// SSE stream is a MAY in the spec (§2.2), and a client that declines it has
// nowhere for a server-initiated ping to land. fold's pings therefore fail
// against such a client from the first tick, and it is disconnected after the
// threshold — a client that is perfectly healthy and merely quiet.
//
// This is pinned rather than asserted as desirable. If it changes — the SDK
// growing a "only keepalive a session with a listening stream" behaviour, or
// fold declining to ping one — this test is the place that says so.
func TestKeepAliveClosesAClientWithNoListeningStream(t *testing.T) {
	const intervalMs = 150
	interval := intervalMs * time.Millisecond

	up, _ := newUpstreamServer(t, "echo")
	ts, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
		Server:    &config.ServerSection{KeepAliveMs: intervalMs},
	})

	client := mcp.NewClient(&mcp.Implementation{Name: "post-only-client", Version: "1.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:             ts.URL + "/mcp",
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	start := time.Now()
	waitFor(t, 10*time.Second, func() bool { return downstreamSessionCount(gw) == 0 },
		"a client with no listening stream kept its session")
	if elapsed := time.Since(start); elapsed < 2*interval {
		t.Errorf("session closed after %v, under two ping intervals (%v)", elapsed, 2*interval)
	}
	// And the client finds out the way any client finds out: its next
	// request is refused and it reconnects.
	if _, err := session.ListTools(context.Background(), nil); err == nil {
		t.Error("a call on the torn-down session succeeded")
	}
}

// TestKeepAliveKeepsAnIdleSessionFromExpiring pins the interaction between
// server.keepAliveMs and server.sessionIdleTimeoutMs, which is not the
// independence it looks like.
//
// The idle timer is reset by client->server POSTs, and a client answers a
// server-initiated ping with a POST. So fold's own pings reset the timer they
// are unrelated to: with keepAliveMs below sessionIdleTimeoutMs, a session
// whose client is present but doing nothing never expires. Compare
// TestSessionIdleExpiry, where the same idle timeout with no keepalive
// reclaims a quiet session.
//
// Whether that is right is a product question — a client that answers pings
// is arguably not the abandoned session the idle timeout exists to reclaim,
// and one that has genuinely gone away is now reclaimed *faster*, after three
// misses rather than after the whole timeout. What is not right is expecting
// both settings to apply: enabling this one does supersede the other for any
// client that answers.
func TestKeepAliveKeepsAnIdleSessionFromExpiring(t *testing.T) {
	const (
		intervalMs = 25
		idleMs     = 200
	)
	up, _ := newUpstreamServer(t, "echo")
	ts, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}},
		Server: &config.ServerSection{
			KeepAliveMs:          intervalMs,
			SessionIdleTimeoutMs: idleMs,
		},
	})

	session, pings := pingCountingClient(t, ts.URL)
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	// Wait out several times the idle timeout, measured in delivered pings
	// so the wait is driven by the traffic under test rather than a clock.
	// The client sends nothing of its own for the whole window.
	const quietFor = 3 * idleMs / intervalMs
	waitFor(t, 10*time.Second, func() bool { return pings() >= quietFor },
		"keepalive stopped before the idle timeout could be observed")

	if got := downstreamSessionCount(gw); got == 0 {
		t.Fatalf("session expired after %d idle timeouts of client silence; "+
			"keepalive pings are expected to hold it open", 3)
	}
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("session unusable after the idle window: %v", err)
	}
}
