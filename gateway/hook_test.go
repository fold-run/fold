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

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// hookServer stands in for an operator's inspector. It records the last
// envelope it received, so the wire contract can be asserted rather than
// assumed.
func hookServer(t *testing.T, reply func(hookRequest) (int, string)) (url string, last func() hookRequest) {
	t.Helper()
	var seen atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req hookRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen.Store(req)
		code, body := reply(req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() hookRequest {
		v, _ := seen.Load().(hookRequest)
		return v
	}
}

func hookConfig(url, onError string) *config.Hook {
	return &config.Hook{URL: url, TimeoutMs: 2000, OnError: onError, Stages: []string{"ingress"}}
}

// TestHookDeniesAndSaysWhy is the feature in one pass: the hook refuses, the
// caller learns why, and the trail records that the inspector rather than the
// policy said no.
func TestHookDeniesAndSaysWhy(t *testing.T) {
	url, _ := hookServer(t, func(hookRequest) (int, string) {
		return 200, `{"decision":"deny","reason":"matched DLP rule 12"}`
	})
	up, _ := newUpstreamServer(t, "send")
	auditCfg, snapshot := collectAudit(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Audit:     auditCfg,
		Hook:      hookConfig(url, "deny"),
	})
	session := connect(t, ts.URL, nil)

	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "send", Arguments: map[string]any{"body": "4111111111111111"},
	})
	if err == nil {
		t.Fatal("hook denial did not refuse the call")
	}
	if !strings.Contains(err.Error(), "matched DLP rule 12") {
		t.Errorf("caller was not told why: %v", err)
	}

	evt := awaitEvent(t, snapshot, func(e audit.Event) bool { return e.Method == "tools/call" })
	if evt.Outcome != audit.OutcomeHookDenied {
		t.Errorf("outcome = %q, want hook_denied — policy and the inspector are different systems", evt.Outcome)
	}
	if evt.HookOutcome != "deny" {
		t.Errorf("hookOutcome = %q, want deny", evt.HookOutcome)
	}
}

// TestHookSeesTheContract pins the envelope. Someone writes a hook against
// these field names, which makes them a public interface from the first
// release.
func TestHookSeesTheContract(t *testing.T) {
	url, last := hookServer(t, func(hookRequest) (int, string) {
		return 200, `{"decision":"allow"}`
	})
	up, _ := newUpstreamServer(t, "send")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL, Namespace: "n"}},
		Hook:      hookConfig(url, "deny"),
	})
	session := connect(t, ts.URL, nil)

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "n__send", Arguments: map[string]any{"to": "ops"},
	}); err != nil {
		t.Fatalf("allowed call failed: %v", err)
	}

	got := last()
	if got.Version != "1" || got.Stage != "ingress" || got.Method != "tools/call" {
		t.Errorf("envelope header = %+v", got)
	}
	if got.Name != "send" {
		t.Errorf("name = %q, want the bare name the upstream knows", got.Name)
	}
	if got.Upstream != "u" {
		t.Errorf("upstream = %q, want u", got.Upstream)
	}
	var args map[string]any
	if err := json.Unmarshal(got.Arguments, &args); err != nil || args["to"] != "ops" {
		t.Errorf("arguments = %s, want the caller's verbatim", got.Arguments)
	}
}

// TestHookRunsAfterPolicy: the hook's allow is necessary but never
// sufficient, and its operator is never handed traffic the gateway has
// already refused.
func TestHookRunsAfterPolicy(t *testing.T) {
	var called atomic.Bool
	url, _ := hookServer(t, func(hookRequest) (int, string) {
		called.Store(true)
		return 200, `{"decision":"allow"}`
	})
	up, _ := newUpstreamServer(t, "get_thing", "delete_thing")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Hook:      hookConfig(url, "deny"),
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "reads",
				Allow: []config.PolicyAllow{{Server: "u", Methods: []string{"tools/call"}, Names: []string{"get_*"}}},
			}},
		},
	})
	session := connect(t, ts.URL, nil)

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "delete_thing"}); err == nil {
		t.Fatal("policy denial did not refuse")
	}
	if called.Load() {
		t.Error("the hook was shown a call policy had already refused")
	}
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_thing"}); err != nil {
		t.Fatalf("allowed call failed: %v", err)
	}
	if !called.Load() {
		t.Error("the hook never saw the call policy allowed")
	}
}

// TestHookFailureModes covers what an unavailable inspector does, in both
// postures. The fail-open case must leave a record: "this call was allowed
// without inspection" is precisely what a compliance review asks about, and
// what a fail-open deployment would otherwise lose.
func TestHookFailureModes(t *testing.T) {
	for _, c := range []struct {
		name    string
		onError string
		reply   func(hookRequest) (int, string)
		allowed bool
	}{
		{"non-2xx fails closed", "deny", func(hookRequest) (int, string) { return 500, `` }, false},
		{"non-2xx fails open", "allow", func(hookRequest) (int, string) { return 500, `` }, true},
		{"garbage body fails closed", "deny", func(hookRequest) (int, string) { return 200, `not json` }, false},
		{"unknown decision fails closed", "deny", func(hookRequest) (int, string) { return 200, `{"decision":"maybe"}` }, false},
		{"unknown decision fails open", "allow", func(hookRequest) (int, string) { return 200, `{"decision":"maybe"}` }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			url, _ := hookServer(t, c.reply)
			up, _ := newUpstreamServer(t, "send")
			auditCfg, snapshot := collectAudit(t)
			ts, _ := startGateway(t, &config.Config{
				Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
				Audit:     auditCfg,
				Hook:      hookConfig(url, c.onError),
			})
			session := connect(t, ts.URL, nil)

			_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send"})
			if c.allowed && err != nil {
				t.Fatalf("fail-open refused the call: %v", err)
			}
			if !c.allowed && err == nil {
				t.Fatal("fail-closed allowed the call")
			}
			if !c.allowed {
				return
			}
			evt := awaitEvent(t, snapshot, func(e audit.Event) bool { return e.Method == "tools/call" })
			if evt.HookOutcome != "error" {
				t.Errorf("hookOutcome = %q, want error — a call that proceeded uninspected must say so", evt.HookOutcome)
			}
		})
	}
}

// TestHookTimeoutIsNotAdvisory: a slow hook is more dangerous than a broken
// one, because failing open turns it into an invisible bypass. The bound
// abandons the request rather than waiting.
func TestHookTimeoutIsNotAdvisory(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	// Cleanups run LIFO, so the handler must be released before Close waits
	// on it — the reverse order deadlocks the test rather than the gateway.
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })

	up, _ := newUpstreamServer(t, "send")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Hook:      &config.Hook{URL: srv.URL, TimeoutMs: 150, OnError: "deny", Stages: []string{"ingress"}},
	})
	session := connect(t, ts.URL, nil)

	start := time.Now()
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send"}); err == nil {
		t.Fatal("a hanging hook allowed the call under onError deny")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("waited %v on a 150ms bound", elapsed)
	}
}

// TestHookOffByDefault: absent config changes nothing on the request path.
func TestHookOffByDefault(t *testing.T) {
	up, _ := newUpstreamServer(t, "send")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})
	session := connect(t, ts.URL, nil)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send"}); err != nil {
		t.Fatalf("call failed with no hook configured: %v", err)
	}
}

// TestHookStageMustBeNamed: a hook configured with no stage inspects nothing.
// Opting in twice is deliberate — configuring an endpoint is not the same act
// as putting it on the request path.
func TestHookStageMustBeNamed(t *testing.T) {
	var called atomic.Bool
	url, _ := hookServer(t, func(hookRequest) (int, string) {
		called.Store(true)
		return 200, `{"decision":"deny"}`
	})
	up, _ := newUpstreamServer(t, "send")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Hook:      &config.Hook{URL: url, TimeoutMs: 1000, OnError: "deny"},
	})
	session := connect(t, ts.URL, nil)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send"}); err != nil {
		t.Fatalf("call failed: %v", err)
	}
	if called.Load() {
		t.Error("a hook with no stage named was called anyway")
	}
}

// BenchmarkHookIngress measures what one decision costs against a local
// no-op endpoint — the floor, not a realistic inspector. The design record
// promises this number rather than an assurance that the hook is cheap: it is
// an HTTP round trip on the proxy path, and no implementation trick removes
// it. What fold controls is that the round trip is pooled and bounded, and
// that a deployment without a hook pays a nil check.
//
//	go test ./gateway -run '^$' -bench BenchmarkHookIngress -benchmem
func BenchmarkHookIngress(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decision":"allow"}`))
	}))
	b.Cleanup(srv.Close)

	h := newDecisionHook(&config.Hook{
		URL: srv.URL, TimeoutMs: 5000, OnError: "deny", Stages: []string{"ingress"},
	})
	req := hookRequest{
		Version: hookWireVersion, Stage: stageIngress, Method: "tools/call",
		Name: "send", Upstream: "u",
		Principal: &hookPrincipal{Sub: "alice", Issuer: "https://idp", Groups: []string{"eng"}},
		Arguments: json.RawMessage(`{"to":"ops","body":"a routine message"}`),
	}
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		if out, _ := h.decide(ctx, req); out != hookAllowed {
			b.Fatalf("unexpected outcome %q", out)
		}
	}
}

// TestHookEgressWithholdsTheResult is the egress stage, and the caveat that
// comes with it: the upstream has already acted, so a denial here is a
// data-loss control rather than a way to stop anything. The caller is told so
// explicitly, because "denied" would otherwise read as "did not happen".
func TestHookEgressWithholdsTheResult(t *testing.T) {
	var called atomic.Int32
	url, last := hookServer(t, func(req hookRequest) (int, string) {
		called.Add(1)
		if req.Stage == "egress" {
			return 200, `{"decision":"deny","reason":"result contained a card number"}`
		}
		return 200, `{"decision":"allow"}`
	})
	// The upstream records that it ran, which is the point: egress does not
	// prevent the side effect it inspects.
	var ran atomic.Bool
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{Name: "charge", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			ran.Store(true)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "4111111111111111"}}}, nil
		})
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Hook: &config.Hook{
			URL: url, TimeoutMs: 2000, OnError: "deny",
			Stages: []string{"ingress", "egress"},
		},
	})
	session := connect(t, ts.URL, nil)

	_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "charge"})
	if err == nil {
		t.Fatal("egress denial did not withhold the result")
	}
	if !strings.Contains(err.Error(), "already acted") {
		t.Errorf("denial does not warn that the effect happened: %v", err)
	}
	if !strings.Contains(err.Error(), "card number") {
		t.Errorf("hook's reason did not reach the caller: %v", err)
	}
	if !ran.Load() {
		t.Error("test is not exercising the caveat: the upstream never ran")
	}
	if called.Load() != 2 {
		t.Errorf("hook called %d times, want 2 (ingress and egress)", called.Load())
	}
	if got := last(); got.Stage != "egress" || len(got.Result) == 0 {
		t.Errorf("egress envelope = %+v, want the result verbatim", got)
	}
}

// TestHookEgressOversizeTakesTheErrorPath: a result too large to inspect is
// not truncated — a partial body is the blind spot an inspector must not be
// handed — so it takes onError, which under "deny" means refusing results
// nobody could have inspected.
func TestHookEgressOversizeTakesTheErrorPath(t *testing.T) {
	url, _ := hookServer(t, func(hookRequest) (int, string) {
		return 200, `{"decision":"allow"}`
	})
	big := strings.Repeat("x", hookMaxResultBytes+1)
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{Name: "dump", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: big}}}, nil
		})
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Hook:      &config.Hook{URL: url, TimeoutMs: 2000, OnError: "deny", Stages: []string{"egress"}},
	})
	session := connect(t, ts.URL, nil)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "dump"}); err == nil {
		t.Error("an uninspectable result was disclosed under onError deny")
	}
}

// TestHookRefusesAnElicitationThatAsksForASecret is the case
// design-server-initiated.md deferred to this feature by name. Policy decides
// whether an upstream may elicit at all; the hook decides whether *this*
// elicitation is acceptable — and unlike egress, refusing here prevents the
// thing rather than withholding it: the human is never shown the prompt.
func TestHookRefusesAnElicitationThatAsksForASecret(t *testing.T) {
	url, last := hookServer(t, func(req hookRequest) (int, string) {
		if strings.Contains(string(req.Arguments), "API key") {
			return 200, `{"decision":"deny","reason":"an upstream may not ask a human for credentials"}`
		}
		return 200, `{"decision":"allow"}`
	})
	upURL := borrowingUpstream(t)
	auditCfg, snapshot := collectAudit(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: upURL}},
		Audit:     auditCfg,
		Hook: &config.Hook{
			URL: url, TimeoutMs: 2000, OnError: "deny",
			Stages: []string{"serverInitiated"},
		},
	})
	session, sampled, elicited := lendingClient(t, ts.URL)

	// borrowingUpstream's tool samples (benign) and then elicits, asking for
	// an API key.
	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "borrow",
		Arguments: map[string]any{},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got, want := out.Content[0].(*mcp.TextContent).Text, "sampling=allowed elicitation=refused"; got != want {
		t.Errorf("upstream saw %q, want %q", got, want)
	}
	if !sampled.Load() {
		t.Error("the benign sampling request was refused")
	}
	if elicited.Load() {
		t.Error("the human was shown a prompt the hook refused — this stage must prevent, not withhold")
	}
	if got := last(); got.Stage != "serverInitiated" || got.Method != "elicitation/create" {
		t.Errorf("last envelope = %+v", got)
	}

	evt := awaitEvent(t, snapshot, func(e audit.Event) bool {
		return e.Method == "elicitation/create" && e.Outcome == audit.OutcomeHookDenied
	})
	if evt.Direction != "server_initiated" {
		t.Errorf("direction = %q, want server_initiated", evt.Direction)
	}
	if evt.HookOutcome != "deny" {
		t.Errorf("hookOutcome = %q, want deny", evt.HookOutcome)
	}
}
