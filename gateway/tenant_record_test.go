package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// Phase 5 of docs/design-tenancy.md: the record. The audit field landed with
// resolution in phase 1; this covers the metric series and the console's
// federation view.
//
// The design said "as a metric label". It cannot be one on the existing
// metrics — the v1 contract freezes metric names *and label sets* — so the
// tenant dimension arrives as new series instead. These tests pin both halves
// of that: the new metrics exist and count, and the frozen ones did not grow
// a label.

// scrapeMetrics reads /metrics as text.
func scrapeMetrics(t *testing.T, base string) string {
	t.Helper()
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// metricLine returns the first scraped line starting with prefix.
func metricLine(scrape, prefix string) string {
	for _, line := range strings.Split(scrape, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}

// A tenant's traffic is attributable in Prometheus, in the same unit its
// budget is charged in — that is what makes "what did team A consume this
// month" answerable without post-processing the audit stream.
func TestTenantMetricsCountRequestsAndCalls(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _, mint := tenantAuthedGateway(t,
		[]config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}},
		acmeTenant(nil, nil),
	)
	ctx := context.Background()
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err != nil {
		t.Fatalf("call: %v", err)
	}

	scrape := scrapeMetrics(t, ts.URL)
	req := metricLine(scrape, `fold_tenant_requests_total{outcome="ok",tenant="acme"}`)
	if req == "" {
		t.Fatalf("no fold_tenant_requests_total series for the tenant:\n%s", scrape)
	}
	calls := metricLine(scrape, `fold_tenant_upstream_calls_total{tenant="acme"}`)
	if calls == "" {
		t.Fatalf("no fold_tenant_upstream_calls_total series for the tenant:\n%s", scrape)
	}
	// One named call is one upstream invocation — the budget's unit.
	if !strings.HasSuffix(calls, " 1") {
		t.Fatalf("upstream calls line = %q, want a count of 1", calls)
	}
}

// The frozen metrics stay frozen: the v1 contract covers label sets, so a
// tenant dimension on fold_requests_total would break every dashboard built
// on it. This is the test that fails if someone "simplifies" the two series
// above into a label later.
func TestFrozenMetricsGainNoTenantLabel(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _, mint := tenantAuthedGateway(t,
		[]config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}},
		acmeTenant(nil, nil),
	)
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "u__tool"}); err != nil {
		t.Fatalf("call: %v", err)
	}

	for _, line := range strings.Split(scrapeMetrics(t, ts.URL), "\n") {
		if strings.HasPrefix(line, "fold_tenant_") || !strings.HasPrefix(line, "fold_") {
			continue
		}
		if strings.Contains(line, "tenant=") {
			t.Fatalf("a frozen metric grew a tenant label, breaking the v1 contract: %q", line)
		}
	}
}

// An untenanted request must not mint a tenant="" series — an empty label
// reads as a tenant in a dashboard, and the request is already counted in
// fold_requests_total.
func TestUntenantedTrafficRecordsNoTenantSeries(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _, mint := tenantAuthedGateway(t,
		[]config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}},
		acmeTenant(nil, nil),
	)
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("carol", "globex")})
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "u__tool"}); err != nil {
		t.Fatalf("call: %v", err)
	}

	scrape := scrapeMetrics(t, ts.URL)
	if strings.Contains(scrape, `tenant=""`) {
		t.Fatalf("untenanted traffic recorded an empty-tenant series:\n%s",
			metricLine(scrape, "fold_tenant_"))
	}
	// It is still counted where every request is counted.
	if metricLine(scrape, `fold_requests_total{method="tools/call"`) == "" {
		t.Fatal("untenanted request missing from fold_requests_total")
	}
}

// A denial is attributable too: the outcome dimension is what turns the
// tenant series into "is this customer being refused", which is the question
// an operator asks before a customer does.
func TestTenantMetricsCarryTheOutcome(t *testing.T) {
	upA, _ := countingUpstream(t, "alpha", "")
	upB, _ := countingUpstream(t, "beta", "")
	iss := newFixtureIssuer(t)
	cfg := authedConfig(iss, []config.Upstream{
		{ID: "a", URL: upA.URL, Namespace: "a"},
		{ID: "b", URL: upB.URL, Namespace: "b"},
	}, nil)
	cfg.Tenants = []config.Tenant{{
		ID:        "acme",
		Subjects:  &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}},
		Upstreams: []string{"a"},
	}}
	ts, _ := startGateway(t, cfg)
	token := iss.mintClaims(t, jwt.MapClaims{
		"sub": "alice", "aud": "https://gw.example.com", "org_id": "acme",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + token})
	_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "b__beta"})

	scrape := scrapeMetrics(t, ts.URL)
	if metricLine(scrape, `fold_tenant_requests_total{outcome="denied",tenant="acme"}`) == "" {
		t.Fatalf("no denied series for the tenant:\n%s", scrape)
	}
}

// The console's federation view is the viewer's. A tenant scoped to one
// upstream must not read another's URL and owner metadata out of the
// dashboard — the one place a topology listing could undo the subset.
func TestConsoleFederationViewIsTenantScoped(t *testing.T) {
	upA, _ := countingUpstream(t, "alpha", "")
	upB, _ := countingUpstream(t, "beta", "")
	iss := newFixtureIssuer(t)
	cfg := authedConfig(iss, []config.Upstream{
		{ID: "a", URL: upA.URL, Namespace: "a"},
		{ID: "b", URL: upB.URL, Namespace: "b"},
	}, nil)
	cfg.Tenants = []config.Tenant{{
		ID:        "acme",
		Subjects:  &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}},
		Upstreams: []string{"a"},
		RateLimit: &config.RateLimit{RequestsPerMinute: 120},
		Budget:    &config.Budget{Period: "month", UpstreamCalls: 5000},
	}}
	cfg.Server = &config.ServerSection{Console: &config.Console{Enabled: true}}
	ts, _ := startGateway(t, cfg)

	state := func(org string) consoleState {
		t.Helper()
		token := iss.mintClaims(t, jwt.MapClaims{
			"sub": "viewer-" + org, "aud": "https://gw.example.com", "org_id": org,
			"exp": time.Now().Add(time.Hour).Unix(),
		})
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/console/api/state", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("console state: %d", resp.StatusCode)
		}
		var st consoleState
		if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
			t.Fatal(err)
		}
		return st
	}

	scoped := state("acme")
	if scoped.Tenant != "acme" {
		t.Fatalf("tenant = %q, want acme", scoped.Tenant)
	}
	if len(scoped.Upstreams) != 1 || scoped.Upstreams[0].ID != "a" {
		t.Fatalf("upstreams = %+v, want only a", scoped.Upstreams)
	}
	// The counts must agree with the list, or the console reports a
	// federation the viewer cannot see the rest of.
	if scoped.StaticUpstreams != 1 || scoped.DiscoveredUpstreams != 0 {
		t.Fatalf("counts = %d static / %d discovered, want 1 / 0",
			scoped.StaticUpstreams, scoped.DiscoveredUpstreams)
	}
	// Its own governance is reported, so a customer-facing console can
	// answer "what am I allowed".
	if scoped.TenantRequestsPerMinute != 120 || scoped.TenantUpstreamCalls != 5000 || scoped.TenantBudgetPeriod != "month" {
		t.Fatalf("tenant governance = %d/min, %d calls/%s; want 120, 5000, month",
			scoped.TenantRequestsPerMinute, scoped.TenantUpstreamCalls, scoped.TenantBudgetPeriod)
	}

	// A viewer in no tenant still sees the whole federation.
	all := state("globex")
	if all.Tenant != "" {
		t.Fatalf("untenanted viewer reported tenant %q", all.Tenant)
	}
	if len(all.Upstreams) != 2 {
		t.Fatalf("untenanted viewer saw %d upstreams, want 2", len(all.Upstreams))
	}
}
