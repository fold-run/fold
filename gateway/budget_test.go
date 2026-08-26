package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// Phase 2 of docs/design-consumption.md wires budgets into config and the
// routing snapshot; enforcement lands in phase 3. These cover the plumbing
// the reloadable-state checklist calls for — config validation, snapshot
// placement, reload semantics, and the construction-wired rejection.

func budgetCfg(upstreamURL string, b *config.Budget) *config.Config {
	return &config.Config{Upstreams: []config.Upstream{
		{ID: "a", URL: upstreamURL, Namespace: "a", Budget: b},
	}}
}

// A per-upstream budget lands on the snapshot's upstream, resolved and ready
// — no per-request parsing of config.
func TestUpstreamBudgetIsOnTheSnapshot(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	_, gw := startGateway(t, budgetCfg(up.URL, &config.Budget{Period: "day", UpstreamCalls: 25}))

	u := gw.rt().byID["a"]
	if u == nil {
		t.Fatal("upstream missing from snapshot")
	}
	if u.budget == nil {
		t.Fatal("upstream has no budget on the snapshot")
	}
	r := u.budget.Used(context.Background())
	if r.Limit != 25 {
		t.Fatalf("limit = %d, want 25", r.Limit)
	}
}

// No budget configured must mean unlimited, not zero — a zero allowance would
// refuse every request.
func TestNoBudgetMeansUnlimited(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	_, gw := startGateway(t, budgetCfg(up.URL, nil))

	u := gw.rt().byID["a"]
	r := u.budget.Add(context.Background(), 1_000_000)
	if !r.Allowed {
		t.Fatal("an unconfigured budget rejected consumption")
	}
	if r.Limit != 0 {
		t.Fatalf("limit = %d, want 0 for an unconfigured budget", r.Limit)
	}
}

// Changing a budget must take effect on reload without a restart. Upstream
// identity is a deep-equal on the whole config, so a budget change retires and
// rebuilds the upstream — this pins that the new value is what serves.
func TestReloadAppliesNewUpstreamBudget(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	_, gw := startGateway(t, budgetCfg(up.URL, &config.Budget{Period: "day", UpstreamCalls: 5}))

	if got := gw.rt().byID["a"].budget.Used(context.Background()).Limit; got != 5 {
		t.Fatalf("limit = %d, want 5", got)
	}
	if err := gw.Reload(budgetCfg(up.URL, &config.Budget{Period: "day", UpstreamCalls: 50})); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := gw.rt().byID["a"].budget.Used(context.Background()).Limit; got != 50 {
		t.Fatalf("limit = %d after reload, want 50", got)
	}
}

// An invalid budget must reject the reload whole, leaving the old snapshot
// serving — the fail-safe every other config error gets.
func TestReloadRejectsInvalidBudgetAndKeepsServing(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, gw := startGateway(t, budgetCfg(up.URL, &config.Budget{Period: "day", UpstreamCalls: 5}))
	session := connect(t, ts.URL, nil)

	err := gw.Reload(budgetCfg(up.URL, &config.Budget{Period: "fortnight", UpstreamCalls: 5}))
	if err == nil {
		t.Fatal("reload accepted an unknown budget period")
	}
	if !strings.Contains(err.Error(), "budget.period") {
		t.Fatalf("error = %v, want it to name budget.period", err)
	}
	// The old snapshot must still serve.
	if got := gw.rt().byID["a"].budget.Used(context.Background()).Limit; got != 5 {
		t.Fatalf("limit = %d after a rejected reload, want the old 5", got)
	}
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("gateway stopped serving after a rejected reload: %v", err)
	}
}

// A non-positive allowance is rejected rather than silently meaning
// "unlimited", which is the opposite of what `"budget": {}` intends.
func TestZeroAllowanceIsRejected(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	_, gw := startGateway(t, budgetCfg(up.URL, &config.Budget{Period: "day", UpstreamCalls: 5}))

	if err := gw.Reload(budgetCfg(up.URL, &config.Budget{Period: "day"})); err == nil {
		t.Fatal("reload accepted a budget with no allowance")
	}
}

// server.budget is construction-wired like the rest of that section: Reload
// must refuse to change it, so an allowance cannot be widened under a running
// gateway by editing config.
func TestReloadRejectsServerBudgetChange(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	base := &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: up.URL, Namespace: "a"}},
		Server:    &config.ServerSection{Budget: &config.Budget{Period: "month", UpstreamCalls: 100}},
	}
	_, gw := startGateway(t, base)

	changed := &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: up.URL, Namespace: "a"}},
		Server:    &config.ServerSection{Budget: &config.Budget{Period: "month", UpstreamCalls: 999999}},
	}
	err := gw.Reload(changed)
	if err == nil {
		t.Fatal("reload widened a construction-wired server budget")
	}
	if !strings.Contains(err.Error(), "server") {
		t.Fatalf("error = %v, want it to name the server section", err)
	}
	if got := gw.globalBudget.Used(context.Background()).Limit; got != 100 {
		t.Fatalf("limit = %d, want the original 100", got)
	}
}

// A budget arriving from a discovery-sourced upstream must reach the snapshot
// and survive a base reload, the same as every other discovered field.
func TestDiscoveredBudgetSurvivesBaseReload(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha")
	upB, _ := newUpstreamServer(t, "beta")
	registry, doc := discoveryRegistry(t, "")

	discovery := &config.Discovery{URL: registry.URL, IntervalMs: 50}
	base := &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: upA.URL, Namespace: "a"}},
		Discovery: discovery,
	}
	_, gw := startGateway(t, base)

	doc.Store(fmt.Sprintf(
		`{"upstreams":[{"id":"b","url":%q,"namespace":"b","budget":{"period":"hour","upstreamCalls":7}}]}`,
		upB.URL))
	waitFor(t, 5*time.Second, func() bool { return gw.rt().byID["b"] != nil },
		"discovered upstream never applied")

	if got := gw.rt().byID["b"].budget.Used(context.Background()).Limit; got != 7 {
		t.Fatalf("discovered budget limit = %d, want 7", got)
	}

	// A base reload must preserve the discovery contribution, budget included.
	if err := gw.Reload(base); err != nil {
		t.Fatalf("base reload: %v", err)
	}
	u := gw.rt().byID["b"]
	if u == nil {
		t.Fatal("discovered upstream lost on base reload")
	}
	if got := u.budget.Used(context.Background()).Limit; got != 7 {
		t.Fatalf("discovered budget limit = %d after base reload, want 7", got)
	}
}

// The period from config must reach the budget, so a configured "hour" is not
// silently served as a month. Asserted by the reset instant's alignment, which
// is deterministic: an hourly window resets on the hour, a daily one at
// midnight UTC, a monthly one on the 1st. Elapsed-time bounds would look
// simpler and be flaky — "more than a day away" is false on the 31st.
func TestConfiguredPeriodReachesTheBudget(t *testing.T) {
	cases := []struct {
		period string
		check  func(time.Time) bool
		want   string
	}{
		{"hour", func(r time.Time) bool { return r.Minute() == 0 && r.Second() == 0 }, "an exact hour"},
		{"day", func(r time.Time) bool { return r.Hour() == 0 && r.Minute() == 0 }, "midnight UTC"},
		{"month", func(r time.Time) bool { return r.Day() == 1 && r.Hour() == 0 }, "the 1st at midnight UTC"},
	}
	for _, c := range cases {
		up, _ := newUpstreamServer(t, "tool")
		_, gw := startGateway(t, budgetCfg(up.URL, &config.Budget{Period: c.period, UpstreamCalls: 3}))

		resets := gw.rt().byID["a"].budget.Used(context.Background()).Resets.UTC()
		if !c.check(resets) {
			t.Fatalf("period %q resets at %v, want %s — the configured period did not reach the budget",
				c.period, resets, c.want)
		}
	}
}

// An unset period defaults to a month, not an hour — the difference between
// one allowance and 730 of them.
func TestBudgetPeriodDefaultsToMonth(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	_, gw := startGateway(t, budgetCfg(up.URL, &config.Budget{UpstreamCalls: 3}))

	resets := gw.rt().byID["a"].budget.Used(context.Background()).Resets.UTC()
	if resets.Day() != 1 || resets.Hour() != 0 {
		t.Fatalf("an unset period resets at %v, want the 1st at midnight UTC", resets)
	}
}

// ---- enforcement (phase 3) ----

// The per-upstream allowance stops calls once spent, and says so with -31044
// rather than the rate-limit code: the remedies differ.
func TestUpstreamBudgetExhaustionRejectsWith32044(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, budgetCfg(up.URL, &config.Budget{Period: "day", UpstreamCalls: 2}))
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	for i := range 2 {
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "a__tool"}); err != nil {
			t.Fatalf("call %d rejected inside the allowance: %v", i, err)
		}
	}
	_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "a__tool"})
	if err == nil {
		t.Fatal("call admitted past an exhausted budget")
	}
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("error = %v (%T), want a JSON-RPC error", err, err)
	}
	if wire.Code != codeBudgetExhausted {
		t.Fatalf("code = %d, want %d — exhaustion must not reuse the rate-limit code",
			wire.Code, codeBudgetExhausted)
	}
	// The message must point at the reset, not at a retry delay: a client
	// backing off by a monthly reset would sleep for a fortnight.
	if !strings.Contains(wire.Message, "resets") {
		t.Fatalf("message = %q, want it to name when the budget resets", wire.Message)
	}
}

// The server budget is the wider net: it stops calls regardless of which
// upstream they were routed to.
func TestServerBudgetSpansUpstreams(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha")
	upB, _ := newUpstreamServer(t, "beta")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{
			{ID: "a", URL: upA.URL, Namespace: "a"},
			{ID: "b", URL: upB.URL, Namespace: "b"},
		},
		Server: &config.ServerSection{Budget: &config.Budget{Period: "day", UpstreamCalls: 2}},
	})
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "a__alpha"}); err != nil {
		t.Fatalf("first call rejected: %v", err)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "b__beta"}); err != nil {
		t.Fatalf("second call rejected: %v", err)
	}
	// Third call, either upstream: the shared allowance is spent.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "a__alpha"}); err == nil {
		t.Fatal("a third call was admitted past a server budget of 2")
	}
}

// Exhaustion is audited with its own outcome — audit is the single exit door,
// and "we are being throttled" must not read the same as "we spent the month".
func TestBudgetExhaustionIsAudited(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")

	var mu sync.Mutex
	var events []audit.Event
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []audit.Event
		if err := json.NewDecoder(r.Body).Decode(&batch); err == nil {
			mu.Lock()
			events = append(events, batch...)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(collector.Close)

	cfg := budgetCfg(up.URL, &config.Budget{Period: "day", UpstreamCalls: 1})
	cfg.Audit = &config.Audit{Sinks: []config.AuditSink{{Type: "webhook", URL: collector.URL}}}
	ts, _ := startGateway(t, cfg)
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "a__tool"})
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "a__tool"}) // exhausts

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		found := false
		for _, e := range events {
			if e.Outcome == audit.OutcomeBudgetExhausted {
				found = true
			}
		}
		mu.Unlock()
		if found {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("no budget_exhausted audit event among %d events", len(events))
}

// A rate-limited call never reaches the upstream, so it must not spend budget.
// Otherwise a caller being throttled would also burn the month's allowance.
func TestRateLimitedCallsDoNotSpendBudget(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	_, gw := startGateway(t, &config.Config{Upstreams: []config.Upstream{{
		ID: "a", URL: up.URL, Namespace: "a",
		RateLimit: &config.RateLimit{RequestsPerMinute: 1},
		Budget:    &config.Budget{Period: "day", UpstreamCalls: 100},
	}}})

	u := gw.rt().byID["a"]
	ctx := context.Background()
	// Drive the upstream directly: the first call passes the limiter, the
	// rest are refused by it.
	for range 5 {
		_, _ = u.listTools(ctx)
	}
	if used := u.budget.Used(ctx).Used; used > 1 {
		t.Fatalf("budget used = %d after one admitted and four rate-limited calls, want 1", used)
	}
}

// An open circuit means the upstream is not serving, so those calls must not
// spend budget either — a month-long outage would otherwise burn a month's
// allowance on calls nobody answered.
func TestCircuitOpenDoesNotSpendBudget(t *testing.T) {
	// No server on this port: every connect fails and opens the breaker.
	_, gw := startGateway(t, &config.Config{Upstreams: []config.Upstream{{
		ID: "a", URL: "http://127.0.0.1:1", Namespace: "a",
		CircuitBreaker: &config.CircuitBreaker{FailureThreshold: 2, HalfOpenAfterMs: 60000},
		Timeouts:       &config.Timeouts{ConnectMs: 200},
		Budget:         &config.Budget{Period: "day", UpstreamCalls: 100},
	}}})

	u := gw.rt().byID["a"]
	ctx := context.Background()
	for range 6 {
		_, _ = u.listTools(ctx)
	}
	if used := u.budget.Used(ctx).Used; used != 0 {
		t.Fatalf("budget used = %d against an upstream that never connected, want 0", used)
	}
}

// ---- metering (phase 4) ----

// collectAudit runs a webhook sink and returns a snapshot accessor.
func collectAudit(t *testing.T) (*config.Audit, func() []audit.Event) {
	t.Helper()
	var mu sync.Mutex
	var events []audit.Event
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []audit.Event
		if err := json.NewDecoder(r.Body).Decode(&batch); err == nil {
			mu.Lock()
			events = append(events, batch...)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return &config.Audit{Sinks: []config.AuditSink{{Type: "webhook", URL: srv.URL}}},
		func() []audit.Event {
			mu.Lock()
			defer mu.Unlock()
			return append([]audit.Event(nil), events...)
		}
}

// awaitEvent polls for the first audit event matching pred.
func awaitEvent(t *testing.T, snap func() []audit.Event, pred func(audit.Event) bool) audit.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, e := range snap() {
			if pred(e) {
				return e
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no matching audit event among %d", len(snap()))
	return audit.Event{}
}

// The metered cost of a federated list is the fan-out, not 1. This is the
// number that makes a cheap-looking client request show its real price.
func TestMeteredFanOutCountsEveryUpstream(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha")
	upB, _ := newUpstreamServer(t, "beta")
	upC, _ := newUpstreamServer(t, "gamma")
	auditCfg, snap := collectAudit(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{
			{ID: "a", URL: upA.URL, Namespace: "a"},
			{ID: "b", URL: upB.URL, Namespace: "b"},
			{ID: "c", URL: upC.URL, Namespace: "c"},
		},
		Audit: auditCfg,
	})
	session := connect(t, ts.URL, nil)
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("list: %v", err)
	}

	e := awaitEvent(t, snap, func(e audit.Event) bool { return e.Method == "tools/list" })
	if e.UpstreamCalls != 3 {
		t.Fatalf("upstreamCalls = %d for a 3-upstream list, want 3", e.UpstreamCalls)
	}
}

// A named call routes to exactly one upstream, so it costs one.
func TestMeteredNamedCallCostsOne(t *testing.T) {
	upA, _ := newUpstreamServer(t, "alpha")
	upB, _ := newUpstreamServer(t, "beta")
	auditCfg, snap := collectAudit(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{
			{ID: "a", URL: upA.URL, Namespace: "a"},
			{ID: "b", URL: upB.URL, Namespace: "b"},
		},
		Audit: auditCfg,
	})
	session := connect(t, ts.URL, nil)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "a__alpha"}); err != nil {
		t.Fatalf("call: %v", err)
	}

	e := awaitEvent(t, snap, func(e audit.Event) bool { return e.Method == "tools/call" })
	if e.UpstreamCalls != 1 {
		t.Fatalf("upstreamCalls = %d for a named call, want 1", e.UpstreamCalls)
	}
}

// itemsServed is what this caller received after policy filtering — the size
// of the surface handed over, not the federation's total.
func TestMeteredItemsServedIsPostPolicy(t *testing.T) {
	up, _ := newUpstreamServer(t, "alpha", "beta", "gamma")
	auditCfg, snap := collectAudit(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: up.URL, Namespace: "a"}},
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "some",
				Allow: []config.PolicyAllow{{Server: "a", Methods: []string{"tools/list", "tools/call"}, Names: []string{"alpha", "beta"}}},
			}},
		},
		Audit: auditCfg,
	})
	session := connect(t, ts.URL, nil)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(res.Tools) != 2 {
		t.Fatalf("served %d tools, want 2", len(res.Tools))
	}

	e := awaitEvent(t, snap, func(e audit.Event) bool { return e.Method == "tools/list" })
	if e.ItemsServed != 2 {
		t.Fatalf("itemsServed = %d, want 2 — it must count what policy let through, not the upstream's 3", e.ItemsServed)
	}
}

// Usage an upstream publishes in _meta is carried verbatim. fold never
// synthesizes it, so an upstream reporting nothing yields no usage field.
func TestMeteredUsageIsCarriedVerbatim(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "usage-fixture", Version: "1.0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "reports", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Meta:    mcp.Meta{"usage": map[string]any{"inputTokens": float64(12), "outputTokens": float64(34)}},
				Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
			}, nil
		})
	srv.AddTool(&mcp.Tool{Name: "silent", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(up.Close)

	auditCfg, snap := collectAudit(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: up.URL, Namespace: "a"}},
		Audit:     auditCfg,
	})
	session := connect(t, ts.URL, nil)
	ctx := context.Background()
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "a__reports"}); err != nil {
		t.Fatalf("call: %v", err)
	}

	e := awaitEvent(t, snap, func(e audit.Event) bool {
		return e.Method == "tools/call" && e.Name == "a__reports"
	})
	if e.Usage == nil {
		t.Fatal("usage absent for an upstream that reported it")
	}
	if got := e.Usage["inputTokens"]; got != float64(12) {
		t.Fatalf("usage[inputTokens] = %v, want 12 — it must pass through verbatim", got)
	}

	// An upstream that reports nothing must not gain an invented usage record.
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "a__silent"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	e2 := awaitEvent(t, snap, func(e audit.Event) bool {
		return e.Method == "tools/call" && e.Name == "a__silent"
	})
	if e2.Usage != nil {
		t.Fatalf("usage = %v for an upstream that reported none — fold must not synthesize it", e2.Usage)
	}
}
