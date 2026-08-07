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

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
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

func TestHealthzHidesDetailsWhenAuthRequired(t *testing.T) {
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
