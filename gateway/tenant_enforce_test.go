package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// Phase 3 of docs/design-tenancy.md: the tenant's budget and bucket actually
// govern. The point of both is that they are shared by everyone in the tenant
// — a budget keyed per principal is what fold already had, and it answers a
// different question than "what did this team spend this month".

// tenantAuthedGateway starts an authenticated gateway with one upstream and
// the given tenants, and returns a way to mint tokens carrying an org_id.
func tenantAuthedGateway(t *testing.T, upstreams []config.Upstream, tenants ...config.Tenant) (*httptest.Server, *Gateway, func(sub, org string) string) {
	t.Helper()
	iss := newFixtureIssuer(t)
	cfg := authedConfig(iss, upstreams, nil)
	cfg.Tenants = tenants
	ts, gw := startGateway(t, cfg)
	mint := func(sub, org string) string {
		return iss.mintClaims(t, jwt.MapClaims{
			"sub": sub, "aud": "https://gw.example.com", "org_id": org,
			"exp": time.Now().Add(time.Hour).Unix(),
		})
	}
	return ts, gw, mint
}

func acmeTenant(budget *config.Budget, rl *config.RateLimit) config.Tenant {
	return config.Tenant{
		ID:        "acme",
		Subjects:  &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}},
		Budget:    budget,
		RateLimit: rl,
	}
}

// The tenant's allowance is spent by whoever belongs to it, and exhaustion
// mints the same -31044 a server or upstream budget does — the remedy is the
// same (wait for the period to roll), so the code is the same.
func TestTenantBudgetExhaustionRejectsWith32044(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _, mint := tenantAuthedGateway(t,
		[]config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}},
		acmeTenant(&config.Budget{Period: "day", UpstreamCalls: 2}, nil),
	)
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})
	ctx := context.Background()

	for i := range 2 {
		if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err != nil {
			t.Fatalf("call %d rejected inside the allowance: %v", i, err)
		}
	}
	_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"})
	if err == nil {
		t.Fatal("call admitted past an exhausted tenant budget")
	}
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("error = %v (%T), want a JSON-RPC error", err, err)
	}
	if wire.Code != codeBudgetExhausted {
		t.Fatalf("code = %d, want %d", wire.Code, codeBudgetExhausted)
	}
	// The message must name the tenant: an operator reading it needs to know
	// which allowance ran out, and "server" and "tenant acme" are different
	// problems with different owners.
	if !strings.Contains(wire.Message, `tenant "acme"`) {
		t.Fatalf("message = %q, want it to name the tenant", wire.Message)
	}
	if !strings.Contains(wire.Message, "resets") {
		t.Fatalf("message = %q, want it to name when the budget resets", wire.Message)
	}
}

// The whole point: two principals in one tenant spend one allowance. Keyed
// per principal, this test would pass with twice the traffic.
func TestTenantBudgetIsSharedByItsPrincipals(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _, mint := tenantAuthedGateway(t,
		[]config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}},
		acmeTenant(&config.Budget{Period: "day", UpstreamCalls: 2}, nil),
	)
	ctx := context.Background()
	alice := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})
	bob := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("bob", "acme")})

	if _, err := alice.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err != nil {
		t.Fatalf("alice's call rejected: %v", err)
	}
	if _, err := bob.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err != nil {
		t.Fatalf("bob's call rejected: %v", err)
	}
	if _, err := alice.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err == nil {
		t.Fatal("a third call was admitted: the tenant's principals are not sharing one allowance")
	}
}

// A caller who resolves to no tenant is governed exactly as before tenancy
// existed — an exhausted tenant must not spill onto everyone else.
func TestUntenantedCallerIsUnaffectedByATenantBudget(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _, mint := tenantAuthedGateway(t,
		[]config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}},
		acmeTenant(&config.Budget{Period: "day", UpstreamCalls: 1}, nil),
	)
	ctx := context.Background()
	acme := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})
	other := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("carol", "globex")})

	if _, err := acme.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err != nil {
		t.Fatalf("acme's first call rejected: %v", err)
	}
	if _, err := acme.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err == nil {
		t.Fatal("acme's second call was admitted past an allowance of 1")
	}
	// Carol matches no tenant, so nothing of hers was ever charged.
	for i := range 3 {
		if _, err := other.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err != nil {
			t.Fatalf("untenanted call %d rejected by another tenant's exhausted budget: %v", i, err)
		}
	}
}

// One bucket per tenant, not per person: this is what "team A cannot flood
// team B" means, and what perPrincipalPerMinute cannot express.
func TestTenantRateLimitIsSharedByItsPrincipals(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _, mint := tenantAuthedGateway(t,
		[]config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}},
		acmeTenant(nil, &config.RateLimit{RequestsPerMinute: 3}),
		config.Tenant{
			ID:        "globex",
			Subjects:  &config.PolicySubjects{Claims: map[string]any{"org_id": "globex"}},
			RateLimit: &config.RateLimit{RequestsPerMinute: 3},
		},
	)
	post := func(token string) int {
		req, err := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	alice, bob := mint("alice", "acme"), mint("bob", "acme")
	carol := mint("carol", "globex")

	// Three requests across two of the tenant's principals fill one bucket.
	for i, tok := range []string{alice, bob, alice} {
		if code := post(tok); code == http.StatusTooManyRequests {
			t.Fatalf("request %d rejected inside the tenant's allowance", i)
		}
	}
	if code := post(bob); code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 — the tenant's principals are not sharing one bucket", code)
	}
	// Another tenant's bucket is untouched by acme's flood.
	if code := post(carol); code == http.StatusTooManyRequests {
		t.Fatal("one tenant's flood 429'd another tenant")
	}
}

// A tenant rate-limit rejection is audited with the tenant named — audit is
// the single exit door, and "which customer is being throttled" is the
// question the record has to answer.
func TestTenantRateLimitRejectionIsAudited(t *testing.T) {
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

	iss := newFixtureIssuer(t)
	cfg := authedConfig(iss, []config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}}, nil)
	cfg.Tenants = []config.Tenant{acmeTenant(nil, &config.RateLimit{RequestsPerMinute: 1})}
	cfg.Audit = &config.Audit{Sinks: []config.AuditSink{{Type: "webhook", URL: collector.URL}}}
	ts, _ := startGateway(t, cfg)

	tok := iss.mintClaims(t, jwt.MapClaims{
		"sub": "alice", "aud": "https://gw.example.com", "org_id": "acme",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	for range 3 {
		req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, e := range events {
			if e.Outcome == audit.OutcomeRateLimited && e.Tenant == "acme" {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("no rate-limited audit event naming the tenant; got %d events", len(events))
}

// A reload that has nothing to do with tenancy must not hand the tenant a
// fresh month. Rebuilding the snapshot rebuilds every tenant, so without
// carrying the counter over, an operator adding an upstream would reset every
// customer's allowance — and reloads are meant to be routine.
func TestTenantBudgetSurvivesAnUnrelatedReload(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	other, _ := newUpstreamServer(t, "second")
	upstreams := []config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}}
	ts, gw, mint := tenantAuthedGateway(t, upstreams,
		acmeTenant(&config.Budget{Period: "day", UpstreamCalls: 2}, nil),
	)
	ctx := context.Background()
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err != nil {
		t.Fatalf("first call rejected: %v", err)
	}

	// Reload with an added upstream: nothing about the tenant changed.
	next := gw.rt().cfg
	reloaded := *next
	reloaded.Upstreams = append(append([]config.Upstream{}, upstreams...),
		config.Upstream{ID: "v", URL: other.URL, Namespace: "v"})
	if err := gw.Reload(&reloaded); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err != nil {
		t.Fatalf("second call rejected inside the allowance: %v", err)
	}
	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "u__tool"}); err == nil {
		t.Fatal("a third call was admitted: the reload reset the tenant's budget")
	}
}

// Changing the allowance is a deliberate act and does apply — the carry-over
// is keyed on that dimension's configuration being unchanged, not on the
// tenant's id alone.
func TestReloadAppliesANewTenantAllowance(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	upstreams := []config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}}
	_, gw, _ := tenantAuthedGateway(t, upstreams,
		acmeTenant(&config.Budget{Period: "day", UpstreamCalls: 2}, nil),
	)

	reloaded := *gw.rt().cfg
	reloaded.Tenants = []config.Tenant{acmeTenant(&config.Budget{Period: "day", UpstreamCalls: 500}, nil)}
	if err := gw.Reload(&reloaded); err != nil {
		t.Fatalf("reload: %v", err)
	}

	tn := gw.rt().tenants.byID["acme"]
	if tn == nil {
		t.Fatal("tenant missing from the snapshot after reload")
	}
	if r := tn.budget.Used(context.Background()); r.Limit != 500 {
		t.Fatalf("limit = %d, want 500", r.Limit)
	}
}

// An unconfigured budget or rate limit must mean unlimited, not zero: a
// tenant declared only to appear in the audit record must not be refused
// every request.
func TestTenantWithoutLimitsIsUnlimited(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	_, gw, _ := tenantAuthedGateway(t,
		[]config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}},
		acmeTenant(nil, nil),
	)
	tn := gw.rt().tenants.byID["acme"]
	if tn == nil {
		t.Fatal("tenant missing from the snapshot")
	}
	if r := tn.budget.Add(context.Background(), 1_000_000); !r.Allowed {
		t.Fatal("a tenant without a budget refused a call")
	}
	if ok, _ := tn.limiter.Allow(context.Background()); !ok {
		t.Fatal("a tenant without a rate limit refused a request")
	}
}
