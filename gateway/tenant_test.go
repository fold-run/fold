package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
)

// Phase 1 of docs/design-tenancy.md: resolution only. Enforcement of a
// tenant's budget, rate limit, and visibility subset lands later, so these
// cover who resolves to what — and what happens when that is ambiguous.

func tenantRoutes(t *testing.T, tenants ...config.Tenant) *routes {
	t.Helper()
	return &routes{tenants: buildTenants(&config.Config{Tenants: tenants})}
}

func principal(sub, issuer string, groups []string, claims map[string]any) *auth.Principal {
	return &auth.Principal{Subject: sub, Issuer: issuer, Groups: groups, Claims: claims}
}

// A principal resolves to the tenant whose selector it satisfies.
func TestTenantResolvesByClaim(t *testing.T) {
	rt := tenantRoutes(t,
		config.Tenant{ID: "acme", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}}},
		config.Tenant{ID: "globex", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "globex"}}},
	)

	got, err := rt.resolveTenant(principal("u1", "https://idp", nil, map[string]any{"org_id": "globex"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.id() != "globex" {
		t.Fatalf("tenant = %q, want globex", got.id())
	}
}

// A principal matching nothing has no tenant, and that is not an error — it
// is governed by the gateway-wide rules, exactly as before tenancy existed.
func TestUnmatchedPrincipalHasNoTenant(t *testing.T) {
	rt := tenantRoutes(t,
		config.Tenant{ID: "acme", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}}},
	)

	got, err := rt.resolveTenant(principal("u1", "https://idp", nil, map[string]any{"org_id": "other"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != nil {
		t.Fatalf("tenant = %q, want none", got.id())
	}
}

// The case static validation cannot catch: two selectors that overlap only
// for some principals. Refused rather than guessed — picking one would hand
// this caller another tenant's allowance and visibility.
func TestAmbiguousTenantIsRefused(t *testing.T) {
	rt := tenantRoutes(t,
		config.Tenant{ID: "by-group", Subjects: &config.PolicySubjects{Groups: []string{"eng"}}},
		config.Tenant{ID: "by-claim", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}}},
	)

	// Matches only the first: fine.
	if _, err := rt.resolveTenant(principal("u1", "https://idp", []string{"eng"}, nil)); err != nil {
		t.Fatalf("single match errored: %v", err)
	}
	// Matches both: refused.
	_, err := rt.resolveTenant(principal("u2", "https://idp", []string{"eng"}, map[string]any{"org_id": "acme"}))
	if err == nil {
		t.Fatal("a principal matching two tenants resolved, want refusal")
	}
	for _, want := range []string{"by-group", "by-claim", "ambiguous"} {
		if !strings.Contains(err.Message, want) {
			t.Fatalf("message = %q, want it to name %q", err.Message, want)
		}
	}
}

// No tenants configured means no resolution work and no tenant — tenancy is
// additive, so an existing deployment behaves identically.
func TestNoTenantsConfigured(t *testing.T) {
	rt := tenantRoutes(t)
	got, err := rt.resolveTenant(principal("u1", "https://idp", []string{"eng"}, nil))
	if err != nil || got != nil {
		t.Fatalf("resolve = (%v, %v), want (nil, nil)", got, err)
	}
}

// The visibility subset is a set membership test; empty means all.
func TestTenantVisibilitySubset(t *testing.T) {
	rt := tenantRoutes(t,
		config.Tenant{
			ID:        "acme",
			Subjects:  &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}},
			Upstreams: []string{"billing"},
		},
		config.Tenant{ID: "all", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "all"}}},
	)
	scoped, _ := rt.resolveTenant(principal("u1", "https://idp", nil, map[string]any{"org_id": "acme"}))
	if !scoped.sees("billing") {
		t.Fatal("scoped tenant cannot see its own upstream")
	}
	if scoped.sees("crm") {
		t.Fatal("scoped tenant sees an upstream outside its subset")
	}
	unscoped, _ := rt.resolveTenant(principal("u2", "https://idp", nil, map[string]any{"org_id": "all"}))
	if !unscoped.sees("crm") {
		t.Fatal("an unscoped tenant must see every upstream")
	}
	// A caller with no tenant sees everything, as before.
	var none *tenant
	if !none.sees("crm") {
		t.Fatal("an untenanted caller must see every upstream")
	}
}

// ---- validation ----

func tenantCfg(up string, tenants ...config.Tenant) *config.Config {
	return &config.Config{
		Upstreams: []config.Upstream{{ID: "billing", URL: up, Namespace: "b"}},
		Tenants:   tenants,
	}
}

func TestTenantValidationRejects(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	subj := &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}}

	cases := []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{"duplicate id", tenantCfg(up.URL,
			config.Tenant{ID: "acme", Subjects: subj},
			config.Tenant{ID: "acme", Subjects: &config.PolicySubjects{Groups: []string{"x"}}}),
			"duplicate id"},
		{"identical selectors", tenantCfg(up.URL,
			config.Tenant{ID: "a", Subjects: subj},
			config.Tenant{ID: "b", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}}}),
			"identical subjects"},
		{"missing subjects", tenantCfg(up.URL,
			config.Tenant{ID: "a"}),
			"subjects is required"},
		{"unknown upstream", tenantCfg(up.URL,
			config.Tenant{ID: "a", Subjects: subj, Upstreams: []string{"nope"}}),
			"unknown upstream"},
		{"per-principal rate limit", tenantCfg(up.URL,
			config.Tenant{ID: "a", Subjects: subj, RateLimit: &config.RateLimit{RequestsPerMinute: 10, PerPrincipalPerMinute: 5}}),
			"server-level only"},
		{"bad budget", tenantCfg(up.URL,
			config.Tenant{ID: "a", Subjects: subj, Budget: &config.Budget{Period: "fortnight", UpstreamCalls: 1}}),
			"budget.period"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.cfg.Validate()
			if err == nil {
				t.Fatalf("config accepted, want rejection naming %q", c.want)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %v, want it to name %q", err, c.want)
			}
		})
	}
}

// Tenants reload, unlike server.budget: a customer signing up must not need a
// gateway restart.
func TestTenantsReload(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	subj := &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}}
	_, gw := startGateway(t, tenantCfg(up.URL, config.Tenant{ID: "acme", Subjects: subj}))

	if n := len(gw.rt().tenants); n != 1 {
		t.Fatalf("tenants = %d, want 1", n)
	}
	if err := gw.Reload(tenantCfg(up.URL,
		config.Tenant{ID: "acme", Subjects: subj},
		config.Tenant{ID: "globex", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "globex"}}},
	)); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := len(gw.rt().tenants); n != 2 {
		t.Fatalf("tenants = %d after reload, want 2", n)
	}
	got, err := gw.rt().resolveTenant(principal("u", "https://idp", nil, map[string]any{"org_id": "globex"}))
	if err != nil || got.id() != "globex" {
		t.Fatalf("resolve after reload = (%v, %v), want globex", got.id(), err)
	}
}

// An invalid tenant rejects the reload whole, leaving the old snapshot.
func TestInvalidTenantRejectsReload(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	subj := &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}}
	_, gw := startGateway(t, tenantCfg(up.URL, config.Tenant{ID: "acme", Subjects: subj}))

	if err := gw.Reload(tenantCfg(up.URL,
		config.Tenant{ID: "acme", Subjects: subj},
		config.Tenant{ID: "bad", Subjects: subj}, // identical selector
	)); err == nil {
		t.Fatal("reload accepted identical tenant selectors")
	}
	if n := len(gw.rt().tenants); n != 1 {
		t.Fatalf("tenants = %d after a rejected reload, want the old 1", n)
	}
}

// The resolved tenant rides the request context, which is what later phases
// read to enforce a tenant's budget, rate limit, and visibility. Verified
// here so the carriage is not merely written and never checked.
func TestResolvedTenantRidesTheContext(t *testing.T) {
	rt := tenantRoutes(t,
		config.Tenant{ID: "acme", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}}},
	)
	resolved, err := rt.resolveTenant(principal("u", "https://idp", nil, map[string]any{"org_id": "acme"}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	ctx := withTenant(context.Background(), resolved)
	if got := tenantFrom(ctx); got.id() != "acme" {
		t.Fatalf("tenant from context = %q, want acme", got.id())
	}

	// An untenanted request reads back as no tenant, not as a panic.
	if got := tenantFrom(withTenant(context.Background(), nil)); got != nil {
		t.Fatalf("tenant from context = %q, want none", got.id())
	}
	// A context that never went through resolution also reads as none.
	if got := tenantFrom(context.Background()); got != nil {
		t.Fatalf("tenant from a bare context = %q, want none", got.id())
	}
}
