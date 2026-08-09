package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/policy"
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

// ---- the resolution index ----
//
// Phase 2 of docs/design-tenancy.md replaced the scan with two indexes plus a
// scan list for what neither covers. The index narrows and the matcher still
// decides, so the only failure it can introduce is a *missed* candidate — a
// caller silently governed as though they had no tenant. These cover the
// shapes where a lookup key could be wrong, and the property that matters
// more than any of them: resolution agrees with a brute-force scan.

// A tenant filed under the claim index still resolves when the principal's
// claim is an array — the matcher's "array contains the value" rule — and a
// duplicated element is one match, not an ambiguity.
func TestTenantIndexMatchesArrayClaim(t *testing.T) {
	rt := tenantRoutes(t,
		config.Tenant{ID: "acme", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}}},
	)
	got, err := rt.resolveTenant(principal("u1", "https://idp", nil, map[string]any{
		"org_id": []any{"other", "acme", "acme"},
	}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.id() != "acme" {
		t.Fatalf("tenant = %q, want acme", got.id())
	}
}

// A claim value that is not a scalar cannot be a lookup key — a map is not
// even hashable — so it must fall through rather than panic or miss.
func TestTenantIndexToleratesNonScalarClaims(t *testing.T) {
	nested := map[string]any{"team": "a"}
	rt := tenantRoutes(t,
		// Indexed, and probed with a value that cannot be a key.
		config.Tenant{ID: "scalar", Subjects: &config.PolicySubjects{Claims: map[string]any{"org": "acme"}}},
		// Not indexable: the required value is an object, so it is scanned.
		config.Tenant{ID: "nested", Subjects: &config.PolicySubjects{Claims: map[string]any{"ctx": nested}}},
	)
	got, err := rt.resolveTenant(principal("u1", "https://idp", nil, map[string]any{
		"org": map[string]any{"unexpected": true},
		"ctx": map[string]any{"team": "a"},
	}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.id() != "nested" {
		t.Fatalf("tenant = %q, want nested", got.id())
	}
}

// Ambiguity must survive the partitioning: two tenants that match are still
// two tenants when one came from an index and the other from the scan.
func TestAmbiguityAcrossIndexAndScan(t *testing.T) {
	rt := tenantRoutes(t,
		config.Tenant{ID: "indexed", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}}},
		config.Tenant{ID: "grouped", Subjects: &config.PolicySubjects{Groups: []string{"eng", "ops"}}},
		config.Tenant{ID: "scanned", Subjects: &config.PolicySubjects{
			Issuers: []string{"https://idp"},
			Claims:  map[string]any{"org_id": "acme"},
		}},
	)
	for _, tc := range []struct {
		name   string
		p      *auth.Principal
		want   string // the single tenant, or "" when refusal is expected
		refuse []string
	}{
		{name: "index only", p: principal("u1", "https://idp", nil, map[string]any{"org_id": "acme2"}), want: ""},
		{
			name: "index and scan", refuse: []string{"indexed", "scanned"},
			p: principal("u2", "https://idp", nil, map[string]any{"org_id": "acme"}),
		},
		{
			name: "two groups of one tenant is one match", want: "grouped",
			p: principal("u3", "https://idp", []string{"eng", "ops"}, nil),
		},
		{
			// A different issuer keeps the scanned tenant out of it, so this
			// is an ambiguity between the two indexes alone.
			name: "group index and claim index", refuse: []string{"grouped", "indexed"},
			p: principal("u4", "https://other", []string{"eng"}, map[string]any{"org_id": "acme"}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The "index only" case names a claim no tenant requires, so it
			// resolves to nothing rather than to a tenant.
			got, err := rt.resolveTenant(tc.p)
			if len(tc.refuse) > 0 {
				if err == nil {
					t.Fatalf("resolved %q, want refusal", got.id())
				}
				for _, want := range tc.refuse {
					if !strings.Contains(err.Message, want) {
						t.Fatalf("message = %q, want it to name %q", err.Message, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.id() != tc.want {
				t.Fatalf("tenant = %q, want %q", got.id(), tc.want)
			}
		})
	}
}

// The property the index exists under: for every principal, resolution agrees
// with matching every declaration one by one. A missed candidate here is a
// caller handed the wrong governance, so this is checked exhaustively over a
// generated cross-product rather than by example.
func TestIndexedResolutionAgreesWithFullScan(t *testing.T) {
	var tenants []config.Tenant
	add := func(id string, s *config.PolicySubjects) {
		tenants = append(tenants, config.Tenant{ID: id, Subjects: s})
	}
	for _, org := range []string{"acme", "globex"} {
		add("claim-"+org, &config.PolicySubjects{Claims: map[string]any{"org_id": org}})
	}
	add("claim-tier", &config.PolicySubjects{Claims: map[string]any{"tier": float64(2)}})
	add("claim-beta", &config.PolicySubjects{Claims: map[string]any{"beta": true}})
	add("group-eng", &config.PolicySubjects{Groups: []string{"eng"}})
	add("group-many", &config.PolicySubjects{Groups: []string{"ops", "sre"}})
	add("sub-only", &config.PolicySubjects{Subs: []string{"u-special"}})
	add("compound", &config.PolicySubjects{
		Issuers: []string{"https://idp"},
		Claims:  map[string]any{"org_id": "initech"},
		Groups:  []string{"eng"},
	})
	rt := tenantRoutes(t, tenants...)

	subs := []string{"u-1", "u-special"}
	issuers := []string{"https://idp", "https://other"}
	groupSets := [][]string{nil, {"eng"}, {"ops"}, {"eng", "sre"}, {"unknown"}}
	claimSets := []map[string]any{
		nil,
		{"org_id": "acme"},
		{"org_id": "globex", "tier": float64(2)},
		{"org_id": []any{"initech", "acme"}},
		{"tier": float64(3), "beta": true},
		{"org_id": map[string]any{"not": "a scalar"}},
	}
	for _, sub := range subs {
		for _, iss := range issuers {
			for _, groups := range groupSets {
				for _, claims := range claimSets {
					p := principal(sub, iss, groups, claims)
					var want []string
					for i := range tenants {
						if policy.MatchSubjects(tenants[i].Subjects, p) {
							want = append(want, tenants[i].ID)
						}
					}
					got, err := rt.resolveTenant(p)
					switch {
					case len(want) > 1:
						if err == nil {
							t.Fatalf("principal %v resolved to %q, want refusal (matches %v)", p, got.id(), want)
						}
						for _, id := range want {
							if !strings.Contains(err.Message, id) {
								t.Fatalf("message = %q, want it to name %q", err.Message, id)
							}
						}
					case len(want) == 1:
						if err != nil || got.id() != want[0] {
							t.Fatalf("principal %v resolved to (%q, %v), want %q", p, got.id(), err, want[0])
						}
					default:
						if err != nil || got != nil {
							t.Fatalf("principal %v resolved to (%q, %v), want no tenant", p, got.id(), err)
						}
					}
				}
			}
		}
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

	if n := gw.rt().tenants.count(); n != 1 {
		t.Fatalf("tenants = %d, want 1", n)
	}
	if err := gw.Reload(tenantCfg(up.URL,
		config.Tenant{ID: "acme", Subjects: subj},
		config.Tenant{ID: "globex", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "globex"}}},
	)); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if n := gw.rt().tenants.count(); n != 2 {
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
	if n := gw.rt().tenants.count(); n != 1 {
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
