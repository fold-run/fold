package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/state"
)

// TestNoCrossHostCredentialLeak proves a hostile upstream cannot capture the
// upstream credential by returning a cross-host redirect.
func TestNoCrossHostCredentialLeak(t *testing.T) {
	var attackerGotAuth atomic.Value
	attackerGotAuth.Store("")
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerGotAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(attacker.Close)

	// The "upstream" immediately redirects every request to the attacker.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(upstream.Close)

	t.Setenv("LEAK_KEY", "SUPER-SECRET")
	gw, err := New(&config.Config{Upstreams: []config.Upstream{{
		ID: "u", URL: upstream.URL,
		Auth: &config.UpstreamAuth{Strategy: "static", SecretRef: "LEAK_KEY"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)

	// Drive a connect through the gateway's upstream client; it will fail
	// (the redirect is refused), but the point is what the attacker saw.
	u := gw.rt().upstreams[0]
	_, _ = u.connect(context.Background(), &mcp.ClientOptions{})

	if got := attackerGotAuth.Load().(string); got != "" {
		t.Fatalf("credential leaked to cross-host redirect target: %q", got)
	}
}

func TestResourcesPolicyGated(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	server.AddResource(&mcp.Resource{URI: "file:///public.txt", Name: "public"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "public"}}}, nil
		})
	server.AddResource(&mcp.Resource{URI: "file:///secret.txt", Name: "secret"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "secret"}}}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "files", URL: up.URL}},
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "public-only",
				Allow: []config.PolicyAllow{{Server: "files", Methods: []string{"resources/read"}, Names: []string{"file:///public*"}}},
			}},
		},
	})
	session := connect(t, ts.URL, nil)

	// List is filtered: secret is invisible.
	res, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var uris []string
	for _, r := range res.Resources {
		uris = append(uris, r.URI)
	}
	if strings.Join(uris, ",") != "file:///public.txt" {
		t.Errorf("resource list = %v, want only public", uris)
	}

	// Reading the allowed resource works.
	if _, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "file:///public.txt"}); err != nil {
		t.Errorf("read public: %v", err)
	}
	// Reading the denied resource is refused by policy, not served.
	if _, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "file:///secret.txt"}); err == nil {
		t.Error("expected policy denial reading secret resource")
	} else if !strings.Contains(err.Error(), "policy denied") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestCompletionPolicyGated(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, &mcp.ServerOptions{
		CompletionHandler: func(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
			return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: []string{"secret-value"}}}, nil
		},
	})
	server.AddPrompt(&mcp.Prompt{Name: "restricted"}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "p", URL: up.URL}},
		Policy:    &config.Policy{DefaultDecision: "deny"}, // deny everything
	})
	session := connect(t, ts.URL, nil)

	// completion/complete on a prompt the caller cannot get must be denied,
	// not silently enumerated.
	_, err := session.Complete(context.Background(), &mcp.CompleteParams{
		Ref:      &mcp.CompleteReference{Type: "ref/prompt", Name: "restricted"},
		Argument: mcp.CompleteParamsArgument{Name: "x", Value: ""},
	})
	if err == nil {
		t.Fatal("expected completion to be denied by policy")
	}
	if !strings.Contains(err.Error(), "policy denied") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestUnsubscribeCannotTearDownOthers(t *testing.T) {
	var upstreamUnsub atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, &mcp.ServerOptions{
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { upstreamUnsub.Add(1); return nil },
	})
	server.AddResource(&mcp.Resource{URI: "file:///watched.txt", Name: "watched"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "x"}}}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	victim := connect(t, ts.URL, nil)
	if err := victim.Subscribe(context.Background(), &mcp.SubscribeParams{URI: "file:///watched.txt"}); err != nil {
		t.Fatalf("victim subscribe: %v", err)
	}

	// Attacker never subscribed, but tries to unsubscribe the same URI.
	attacker := connect(t, ts.URL, nil)
	if err := attacker.Unsubscribe(context.Background(), &mcp.UnsubscribeParams{URI: "file:///watched.txt"}); err != nil {
		t.Fatalf("attacker unsubscribe: %v", err)
	}

	// The attacker's unsubscribe must NOT have reached the upstream — the
	// victim's subscription is still held.
	if got := upstreamUnsub.Load(); got != 0 {
		t.Errorf("attacker's unsubscribe tore down the shared upstream subscription (upstream unsub count=%d)", got)
	}
}

func TestHostAllowlistDropsLocalhost(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{AllowedHosts: []string{"gw.example.com"}},
	})

	// A pinned allowlist must NOT silently keep accepting Host: localhost.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader("{}"))
	req.Host = "localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("Host: localhost should be forbidden under a pinned allowlist, got %d", resp.StatusCode)
	}
}

func TestHealthHidesDetailsWhenAuthRequired(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, authedConfig(iss,
		[]config.Upstream{{ID: "u", URL: up.URL, Owner: &config.Owner{Org: "secret-org"}}}, nil))

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	if strings.Contains(text, "secret-org") || strings.Contains(text, up.URL) {
		t.Errorf("health leaked upstream url/owner to unauthenticated caller: %s", text)
	}
	// Basic liveness fields still present.
	if !strings.Contains(text, `"id":"u"`) {
		t.Errorf("health should still report upstream id: %s", text)
	}
}

func TestLoggerReceivesOperationalEvents(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	up, _ := newUpstreamServer(t, "tool")
	gw, err := New(&config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}}, WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)
	session := connect(t, ts.URL, nil)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil {
		t.Fatal(err)
	}
	gw.Close()

	logs := buf.String()
	for _, want := range []string{"gateway configured", "upstream connected", "gateway shutting down"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs missing %q; got:\n%s", want, logs)
		}
	}
	// The upstream label must be attached to per-upstream events.
	if !strings.Contains(logs, `upstream=u`) {
		t.Errorf("per-upstream logs missing upstream label; got:\n%s", logs)
	}
}

// One large POST must not pin unbounded gateway memory: a declared
// Content-Length beyond server.maxBodyBytes is answered 413 before any read,
// and a chunked body with no honest length is cut off at the cap.
func TestRequestBodyCap(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{MaxBodyBytes: 1024},
	})
	oversized := strings.Repeat("x", 4096)

	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(oversized))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("over-length POST: want 413, got %d", resp.StatusCode)
	}

	// Hiding the length behind chunked encoding must not slip past the cap.
	req, err := http.NewRequest("POST", ts.URL+"/mcp", struct{ io.Reader }{strings.NewReader(oversized)})
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Errorf("chunked oversized body accepted: %d", resp.StatusCode)
	}

	// Normal-size traffic is unaffected.
	session := connect(t, ts.URL, nil)
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("normal request through the cap: %v", err)
	}
}

// A DNS-rebinding rejection is a terminal response like any other: it must
// flow through the audit exit, not vanish with only a metrics bump.
func TestHostRejectionAudited(t *testing.T) {
	events := make(chan []byte, 1)
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		select {
		case events <- b:
		default:
		}
	}))
	t.Cleanup(sink.Close)

	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Audit:     &config.Audit{Sinks: []config.AuditSink{{Type: "webhook", URL: sink.URL}}},
	})

	req, err := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403 for forbidden host, got %d", resp.StatusCode)
	}
	select {
	case b := <-events:
		if !bytes.Contains(b, []byte(`"forbidden"`)) {
			t.Errorf("audit event does not record the forbidden outcome: %s", b)
		}
	case <-time.After(2 * time.Second):
		t.Error("host rejection was not audited")
	}
}

func TestAuthorityHostRejectsMalformedPort(t *testing.T) {
	for _, tc := range []struct {
		authority string
		want      string
		ok        bool
	}{
		{"localhost", "localhost", true},
		{"LocalHost:8080", "localhost", true},
		{"[::1]:8080", "::1", true},
		{"[::1]", "::1", true},
		// net.SplitHostPort splits at the last colon without checking what
		// follows, so these must not be read as the allowed host prefix.
		{"localhost:8080.evil.com", "", false},
		{"localhost:evil", "", false},
		{"", "", false},
	} {
		got, ok := authorityHost(tc.authority)
		if ok != tc.ok || got != tc.want {
			t.Errorf("authorityHost(%q) = (%q, %v), want (%q, %v)", tc.authority, got, ok, tc.want, tc.ok)
		}
	}
}

func TestOriginAllowedRejectsMalformedOrigins(t *testing.T) {
	allowed := map[string]bool{"localhost": true}
	for _, tc := range []struct {
		origin string
		want   bool
	}{
		{"http://localhost", true},
		{"https://localhost:8080", true},
		{"null", false},
		{"localhost", false},        // schemeless
		{"file://localhost", false}, // not an http(s) origin
		{"https://evil.com", false},
		{"https://localhost:8080.evil.com", false}, // invalid port
		{"https://evil.com/localhost", false},
	} {
		if got := originAllowed(allowed, tc.origin); got != tc.want {
			t.Errorf("originAllowed(%q) = %v, want %v", tc.origin, got, tc.want)
		}
	}
}

// TestForbiddenHostWithMalformedPort proves the rebinding guard rejects an
// authority whose port is not numeric rather than reading it as its prefix.
func TestForbiddenHostWithMalformedPort(t *testing.T) {
	up, _ := newUpstreamServer(t, "echo")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/health", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "localhost:8080.evil.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Host %q should be forbidden, got %d", req.Host, resp.StatusCode)
	}
}

// TestUpstreamDownMessageRedacted: a fold-minted -31041 must not carry the
// raw transport error — it names internal endpoints and can name secret
// env vars, the same strings /health redacts for untrusted callers.
func TestUpstreamDownMessageRedacted(t *testing.T) {
	provider := state.NewMemory()
	t.Cleanup(func() { _ = provider.Close() })
	u := newUpstream(config.Upstream{
		ID:       "internal",
		URL:      "http://secret-internal-host.invalid:9",
		Timeouts: &config.Timeouts{ConnectMs: 200},
	}, provider)
	t.Cleanup(u.Close)

	err := u.ping(context.Background())
	var wire *jsonrpc.Error
	if !asWireError(err, &wire) || wire.Code != codeUpstreamDown {
		t.Fatalf("expected -31041, got %v", err)
	}
	if strings.Contains(wire.Message, "secret-internal-host") {
		t.Fatalf("minted error leaks the endpoint: %q", wire.Message)
	}
	if !strings.Contains(wire.Message, `upstream "internal" unavailable`) {
		t.Fatalf("unexpected message shape: %q", wire.Message)
	}
}

// A credentialed upstream over cleartext http is a supported topology inside
// a mesh and a mistake everywhere else, and fold cannot tell which. What it
// can do is name the upstream at startup. Passthrough counts — it carries the
// caller's own bearer token — and loopback does not, because that hop never
// leaves the machine.
func TestCleartextCredentialedUpstreamIsWarnedAtStartup(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Setenv("API_KEY", "k")
	// Upstreams connect lazily, so none of these need a server behind them.
	gw, err := New(&config.Config{Upstreams: []config.Upstream{
		{ID: "plain", URL: "http://mcp.internal:8080/mcp", Namespace: "plain"},
		{ID: "keyed", URL: "http://mcp.internal:8081/mcp", Namespace: "keyed", Auth: &config.UpstreamAuth{Strategy: "static", SecretRef: "API_KEY"}},
		{ID: "local", URL: "http://127.0.0.1:9/mcp", Namespace: "local", Auth: &config.UpstreamAuth{Strategy: "static", SecretRef: "API_KEY"}},
		{ID: "tls", URL: "https://mcp.internal/mcp", Namespace: "tls", Auth: &config.UpstreamAuth{Strategy: "static", SecretRef: "API_KEY"}},
	}}, WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	gw.Close()

	var warned []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "cleartext http") {
			warned = append(warned, line)
		}
	}
	if len(warned) != 1 || !strings.Contains(warned[0], "upstream=keyed") {
		t.Fatalf("want exactly one cleartext warning, for upstream=keyed; got %d:\n%s", len(warned), strings.Join(warned, "\n"))
	}
}

// postWithOrigin sends a minimal /mcp request carrying Host and Origin and
// returns the status. Anything other than 403 means the request passed the
// host/origin stage; what the MCP layer then says about an empty body is not
// what these tests assert.
func postWithOrigin(t *testing.T, url, host, origin string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// allowedHosts ["*"] used to switch the Origin check off with it, and MCP
// requires Origin validation however Host is handled. The old behaviour is
// kept — a ["*"] deployment behind a proxy keeps working and is warned at
// startup — and server.allowedOrigins is the rule that closes the gap without
// forcing same-origin on the browser clients that work today.
func TestAllowedOriginsUnderWildcardHosts(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")

	// Wildcard alone: any Origin passes, and the gateway says so at startup.
	var buf bytes.Buffer
	gw, err := New(&config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{AllowedHosts: []string{"*"}},
	}, WithLogger(slog.New(slog.NewTextHandler(&buf, nil))))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(func() { ts.Close(); gw.Close() })
	if code := postWithOrigin(t, ts.URL, "127.0.0.1", "https://evil.example"); code == http.StatusForbidden {
		t.Fatalf("wildcard hosts without allowedOrigins refused an Origin; the compatibility behaviour changed")
	}
	if !strings.Contains(buf.String(), "browser Origins are not validated") {
		t.Fatalf("no startup warning for wildcard hosts without allowedOrigins; got:\n%s", buf.String())
	}

	// Wildcard plus allowedOrigins: the Origin rule applies on its own.
	var quiet bytes.Buffer
	gw2, err := New(&config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{AllowedHosts: []string{"*"}, AllowedOrigins: []string{"inspector.example", "*.apps.example"}},
	}, WithLogger(slog.New(slog.NewTextHandler(&quiet, nil))))
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(gw2.Handler())
	t.Cleanup(func() { ts2.Close(); gw2.Close() })
	if strings.Contains(quiet.String(), "browser Origins are not validated") {
		t.Fatal("warned although allowedOrigins is set")
	}
	if code := postWithOrigin(t, ts2.URL, "127.0.0.1", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("disallowed Origin passed under allowedOrigins: %d", code)
	}
	for _, good := range []string{"https://inspector.example:6274", "http://tool.apps.example"} {
		if code := postWithOrigin(t, ts2.URL, "127.0.0.1", good); code == http.StatusForbidden {
			t.Fatalf("allowed Origin %s refused", good)
		}
	}
	if code := postWithOrigin(t, ts2.URL, "127.0.0.1", "null"); code != http.StatusForbidden {
		t.Fatalf("Origin null passed under allowedOrigins: %d", code)
	}
	if code := postWithOrigin(t, ts2.URL, "127.0.0.1", ""); code == http.StatusForbidden {
		t.Fatal("a request with no Origin (non-browser client) was refused")
	}
}

// With an explicit host allowlist and no allowedOrigins, the Origin rule is
// derived from the hosts, as it always was. Setting allowedOrigins replaces
// that derivation rather than extending it.
//
// The allowed host is loopback on purpose: the SDK's streamable handler has
// its own DNS-rebinding guard that refuses a non-loopback Host on a
// connection that arrived over a loopback address — which is every httptest
// server with a rewritten Host header. fold's own check runs first and is
// what this test is about; a loopback Host keeps the SDK's guard out of it.
func TestAllowedOriginsReplaceTheDerivedRule(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{AllowedHosts: []string{"127.0.0.1"}, AllowedOrigins: []string{"app.example"}},
	})
	if code := postWithOrigin(t, ts.URL, "127.0.0.1", "https://app.example"); code == http.StatusForbidden {
		t.Fatal("listed Origin refused although it is not an allowed host — allowedOrigins must stand on its own")
	}
	if code := postWithOrigin(t, ts.URL, "127.0.0.1", "http://127.0.0.1"); code != http.StatusForbidden {
		t.Fatalf("the gateway's own host passed as an Origin though allowedOrigins does not list it: %d", code)
	}
	if code := postWithOrigin(t, ts.URL, "evil.example", "https://app.example"); code != http.StatusForbidden {
		t.Fatalf("bad Host passed because the Origin was fine: %d — the two checks are independent", code)
	}
}
