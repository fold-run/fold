package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

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
