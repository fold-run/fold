package gateway

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// TestReloadAddsAndRemovesUpstreams: the reload swaps the upstream set —
// added upstreams become routable, removed ones disappear from lists and
// answer the unknown-namespace error on named calls.
func TestReloadAddsAndRemovesUpstreams(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha_tool")
	upB, _ := newUpstreamServer(t, "beta_tool")
	ts, gw := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "a", URL: upA.URL, Namespace: "a"},
	}})
	session := connect(t, ts.URL, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toolNames(res), ","); got != "a__alpha_tool" {
		t.Fatalf("initial list = %q", got)
	}

	// Grow the federation.
	if err := gw.Reload(&config.Config{Upstreams: []config.Upstream{
		{ID: "a", URL: upA.URL, Namespace: "a"},
		{ID: "b", URL: upB.URL, Namespace: "b"},
	}}); err != nil {
		t.Fatalf("Reload add: %v", err)
	}
	res, err = session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(toolNames(res), ",")
	if !strings.Contains(got, "a__alpha_tool") || !strings.Contains(got, "b__beta_tool") {
		t.Fatalf("post-add list = %q, want both upstreams", got)
	}
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "b__beta_tool"}); err != nil {
		t.Fatalf("call added upstream: %v", err)
	}

	// Shrink it.
	if err := gw.Reload(&config.Config{Upstreams: []config.Upstream{
		{ID: "b", URL: upB.URL, Namespace: "b"},
	}}); err != nil {
		t.Fatalf("Reload remove: %v", err)
	}
	res, err = session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toolNames(res), ","); strings.Contains(got, "a__") {
		t.Fatalf("removed upstream still listed: %q", got)
	}
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "a__alpha_tool"}); err == nil {
		t.Fatal("call to removed upstream should fail")
	} else if !strings.Contains(err.Error(), "no upstream owns this namespace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestReloadKeepsUnchangedUpstreamSessions: an upstream whose config did not
// change is carried over live — no new upstream session is established — and
// a changed one is rebuilt.
func TestReloadKeepsUnchangedUpstreamSessions(t *testing.T) {
	upA, sessionsA := countingUpstreamServer(t, "alpha_tool")
	cfgA := config.Upstream{ID: "a", URL: upA.URL, Namespace: "a"}
	ts, gw := startGateway(t, &config.Config{Upstreams: []config.Upstream{cfgA}})
	session := connect(t, ts.URL, nil)

	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	before := sessionsA.Load()

	// Same upstream config → the live root session survives the reload.
	if err := gw.Reload(&config.Config{Upstreams: []config.Upstream{cfgA}}); err != nil {
		t.Fatal(err)
	}
	// The reload emitted list_changed, which invalidated nothing for this
	// unchanged upstream — but force a fresh fetch to prove the session is
	// reused rather than reconnected.
	gw.rt().byID["a"].lists.Invalidate(context.Background(), "tools")
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if after := sessionsA.Load(); after != before {
		t.Errorf("unchanged upstream reconnected on reload: sessions %d → %d", before, after)
	}

	// Changed config (different cache TTL) → the upstream is rebuilt and the
	// next request opens a fresh session.
	changed := cfgA
	changed.CacheTTLMs = 1
	if err := gw.Reload(&config.Config{Upstreams: []config.Upstream{changed}}); err != nil {
		t.Fatal(err)
	}
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if after := sessionsA.Load(); after != before+1 {
		t.Errorf("changed upstream should reconnect exactly once: sessions %d → %d", before, after)
	}
}

// TestReloadSwapsPolicy: a policy edit takes effect atomically — list
// filtering and call denial flip without a restart.
func TestReloadSwapsPolicy(t *testing.T) {
	up, _ := newUpstreamServer(t, "get_thing", "delete_thing")
	base := []config.Upstream{{ID: "things", URL: up.URL, Namespace: "things"}}
	ts, gw := startGateway(t, &config.Config{Upstreams: base})
	session := connect(t, ts.URL, nil)

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "things__delete_thing"}); err != nil {
		t.Fatalf("pre-reload call should be allowed: %v", err)
	}

	if err := gw.Reload(&config.Config{
		Upstreams: base,
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "readers",
				Allow: []config.PolicyAllow{{Server: "things", Methods: []string{"tools/call"}, Names: []string{"get_*"}}},
			}},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "things__delete_thing"}); err == nil {
		t.Fatal("post-reload call should be denied")
	} else if !strings.Contains(err.Error(), "policy denied") {
		t.Fatalf("unexpected error: %v", err)
	}
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toolNames(res), ","); got != "things__get_thing" {
		t.Errorf("post-reload list = %q, want only things__get_thing", got)
	}
}

// TestReloadRejectsNonReloadableSections: auth, server, routing, and audit
// are wired at construction; changing them must fail loudly and leave the
// running snapshot untouched.
func TestReloadRejectsNonReloadableSections(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool_x")
	base := []config.Upstream{{ID: "u", URL: up.URL}}
	_, gw := startGateway(t, &config.Config{Upstreams: base})

	cases := map[string]*config.Config{
		"server": {Upstreams: base, Server: &config.ServerSection{RateLimit: &config.RateLimit{RequestsPerMinute: 10}}},
		"routing": {Upstreams: []config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}},
			Routing: &config.Routing{NamespaceSeparator: "::"}},
		"audit": {Upstreams: base, Audit: &config.Audit{Sinks: []config.AuditSink{{Type: "stdout"}}}},
		"auth": {Upstreams: base, Auth: &config.Auth{Mode: "required", Resource: "https://gw.example.com",
			Issuers: []config.Issuer{{Issuer: "https://idp.example.com"}}}},
		"tracing": {Upstreams: base, Tracing: &config.Tracing{OTLPEndpoint: "http://collector.example.com:4318"}},
	}
	for section, cfg := range cases {
		err := gw.Reload(cfg)
		if err == nil {
			t.Errorf("%s change: Reload should fail", section)
			continue
		}
		if !strings.Contains(err.Error(), section+" section") {
			t.Errorf("%s change: error %q should name the section", section, err)
		}
	}

	// An invalid document is rejected by validation before any diffing.
	if err := gw.Reload(&config.Config{}); err == nil {
		t.Error("invalid config: Reload should fail")
	}

	// The running snapshot is untouched by failed reloads.
	if got := len(gw.rt().upstreams); got != 1 {
		t.Errorf("failed reloads must not alter routing: %d upstreams", got)
	}
}

// TestReloadNotifiesClients: after a successful reload, connected clients
// receive list_changed so they refetch and see the new world.
func TestReloadNotifiesClients(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha_tool")
	upB, _ := newUpstreamServer(t, "beta_tool")
	ts, gw := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "a", URL: upA.URL, Namespace: "a"},
	}})

	var notified atomic.Bool
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { notified.Store(true) },
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	if err := gw.Reload(&config.Config{Upstreams: []config.Upstream{
		{ID: "a", URL: upA.URL, Namespace: "a"},
		{ID: "b", URL: upB.URL, Namespace: "b"},
	}}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !notified.Load() {
		time.Sleep(20 * time.Millisecond)
	}
	if !notified.Load() {
		t.Fatal("client never received tools/list_changed after reload")
	}
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toolNames(res), ","); !strings.Contains(got, "b__beta_tool") {
		t.Errorf("refetched list %q missing the added upstream", got)
	}
}
