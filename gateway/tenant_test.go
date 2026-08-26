package gateway

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/state"
	"github.com/fold-run/fold/policy"
)

// Phase 1 of docs/design-tenancy.md: resolution only. Enforcement of a
// tenant's budget, rate limit, and visibility subset lands later, so these
// cover who resolves to what — and what happens when that is ambiguous.

func tenantRoutes(t *testing.T, tenants ...config.Tenant) *routes {
	t.Helper()
	return &routes{tenants: buildTenants(&config.Config{Tenants: tenants}, state.NewMemory(), tenantSet{})}
}

// principal builds a caller. Scopes are variadic and trailing because most
// selectors do not name any, and a caller that holds none is the common shape
// — but every generated principal in the cross-product below carries them, so
// the index cannot be exercised only by the tests that remembered to.
func principal(sub, issuer string, groups []string, claims map[string]any, scopes ...string) *auth.Principal {
	return &auth.Principal{Subject: sub, Issuer: issuer, Groups: groups, Claims: claims, Scopes: scopes}
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
	// The four shapes a scope can appear in, because they partition
	// differently and only one of them is indexable. A selector that names a
	// scope *and* something else must fall to the scan: filing it under the
	// other dimension alone would drop the scope requirement, and dropping a
	// requirement is how a caller ends up in a tenant they were kept out of.
	add("scope-one", &config.PolicySubjects{Scopes: []string{"read"}})
	add("scope-both", &config.PolicySubjects{Scopes: []string{"read", "write"}})
	add("group-scope", &config.PolicySubjects{Groups: []string{"ops"}, Scopes: []string{"admin"}})
	add("claim-scope", &config.PolicySubjects{
		Claims: map[string]any{"tier": float64(2)},
		Scopes: []string{"admin"},
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
	scopeSets := [][]string{nil, {"read"}, {"read", "write"}, {"admin"}, {"unknown"}}
	for _, sub := range subs {
		for _, iss := range issuers {
			for _, groups := range groupSets {
				for _, claims := range claimSets {
					for _, scopes := range scopeSets {
						p := principal(sub, iss, groups, claims, scopes...)
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
}

// The partitioning itself, asserted rather than inferred. The cross-product
// above can only catch an index that produces a *wrong* answer, and it cannot
// catch a selector filed in the wrong partition while the matcher covers for
// it — resolveTenant re-runs policy.MatchSubjects on every candidate an index
// produces, so a mis-filed selector still resolves correctly today. That
// makes this the only test that fails if indexableGroups or indexableClaim
// stops excluding scope-bearing selectors, and the re-match is the only thing
// standing between that and admitting a caller to a tenant on their group
// alone.
func TestScopeBearingSelectorsAreNotIndexedByAnotherDimension(t *testing.T) {
	ts := buildTenants(&config.Config{Tenants: []config.Tenant{
		{ID: "scope-one", Subjects: &config.PolicySubjects{Scopes: []string{"read"}}},
		{ID: "scope-both", Subjects: &config.PolicySubjects{Scopes: []string{"read", "write"}}},
		{ID: "group-scope", Subjects: &config.PolicySubjects{Groups: []string{"eng"}, Scopes: []string{"admin"}}},
		{ID: "claim-scope", Subjects: &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}, Scopes: []string{"admin"}}},
	}}, state.NewMemory(), tenantSet{})

	// A single scope and nothing else is the one indexable shape: the
	// principal's held scopes are the lookup keys.
	if got := ts.byScope["read"]; len(got) != 1 || got[0].id() != "scope-one" {
		t.Fatalf("byScope[read] = %v, want just scope-one", ids(got))
	}
	// A conjunctive requirement cannot be answered by a held-scope lookup —
	// holding "read" is not holding "read write" — so it must not be filed
	// under either of its scopes.
	if got := ts.byScope["write"]; len(got) != 0 {
		t.Fatalf("byScope[write] = %v, want nothing: a multi-scope selector is not satisfied by one of its scopes", ids(got))
	}
	// And a scope alongside another dimension belongs to neither index.
	if got := ts.byGroup["eng"]; len(got) != 0 {
		t.Fatalf("byGroup[eng] = %v, want nothing: the scope requirement would be dropped by the lookup", ids(got))
	}
	if got := ts.byClaim["org_id"][any("acme")]; len(got) != 0 {
		t.Fatalf("byClaim[org_id=acme] = %v, want nothing: the scope requirement would be dropped by the lookup", ids(got))
	}
	if got := ids(ts.scan); len(got) != 3 {
		t.Fatalf("scan partition = %v, want the three unindexable selectors", got)
	}
}

func ids(ts []*tenant) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.id())
	}
	return out
}

// The bug the index predicates exist to prevent, stated as behaviour: a
// selector naming a group *and* a scope admits nobody who holds only the
// group. Scopes are conjunctive — they say what the token was granted, not
// who holds it — so they are a requirement on top of the identity match, not
// another way to satisfy it.
func TestTenantSelectorRequiresBothGroupAndScope(t *testing.T) {
	rt := tenantRoutes(t,
		config.Tenant{ID: "acme-admins", Subjects: &config.PolicySubjects{
			Groups: []string{"eng"}, Scopes: []string{"admin"},
		}},
	)
	cases := []struct {
		name   string
		p      *auth.Principal
		tenant string
	}{
		{"group without the scope", principal("u1", "https://idp", []string{"eng"}, nil), ""},
		{"scope without the group", principal("u2", "https://idp", []string{"sales"}, nil, "admin"), ""},
		{"another scope entirely", principal("u3", "https://idp", []string{"eng"}, nil, "read"), ""},
		{"both", principal("u4", "https://idp", []string{"eng"}, nil, "admin"), "acme-admins"},
		{"both, among others", principal("u5", "https://idp", []string{"sales", "eng"}, nil, "read", "admin"), "acme-admins"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := rt.resolveTenant(c.p)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.id() != c.tenant {
				t.Fatalf("tenant = %q, want %q", got.id(), c.tenant)
			}
		})
	}
}

// The same for a claim alongside a scope, which is the other index.
func TestTenantSelectorRequiresBothClaimAndScope(t *testing.T) {
	rt := tenantRoutes(t,
		config.Tenant{ID: "acme-admins", Subjects: &config.PolicySubjects{
			Claims: map[string]any{"org_id": "acme"}, Scopes: []string{"admin"},
		}},
	)
	acme := map[string]any{"org_id": "acme"}
	if got, err := rt.resolveTenant(principal("u1", "https://idp", nil, acme)); err != nil || got != nil {
		t.Fatalf("resolve = (%q, %v), want no tenant: the claim alone must not admit", got.id(), err)
	}
	if got, err := rt.resolveTenant(principal("u2", "https://idp", nil, nil, "admin")); err != nil || got != nil {
		t.Fatalf("resolve = (%q, %v), want no tenant: the scope alone must not admit", got.id(), err)
	}
	got, err := rt.resolveTenant(principal("u3", "https://idp", nil, acme, "admin"))
	if err != nil || got.id() != "acme-admins" {
		t.Fatalf("resolve = (%q, %v), want acme-admins", got.id(), err)
	}
}

// A multi-scope selector matches conjunctively, and it is the scan that says
// so — holding one of its scopes is not holding the set.
func TestMultiScopeTenantSelectorIsConjunctive(t *testing.T) {
	rt := tenantRoutes(t,
		config.Tenant{ID: "writers", Subjects: &config.PolicySubjects{Scopes: []string{"read", "write"}}},
	)
	for _, held := range [][]string{nil, {"read"}, {"write"}, {"read", "admin"}} {
		got, err := rt.resolveTenant(principal("u", "https://idp", nil, nil, held...))
		if err != nil || got != nil {
			t.Fatalf("scopes %v resolved to (%q, %v), want no tenant", held, got.id(), err)
		}
	}
	for _, held := range [][]string{{"read", "write"}, {"write", "read"}, {"admin", "read", "write"}} {
		got, err := rt.resolveTenant(principal("u", "https://idp", nil, nil, held...))
		if err != nil || got.id() != "writers" {
			t.Fatalf("scopes %v resolved to (%q, %v), want writers", held, got.id(), err)
		}
	}
}

// A single-scope selector resolves out of a document large enough that a scan
// would be the thing being measured rather than the index — the behavioural
// statement of "this shape is indexed", which is what the byScope partition
// is for. A miss here is a caller silently governed as though they had no
// tenant.
func TestSingleScopeTenantResolvesAcrossManyDeclarations(t *testing.T) {
	var tenants []config.Tenant
	for i := range 5000 {
		tenants = append(tenants, config.Tenant{
			ID:       fmt.Sprintf("t-%05d", i),
			Subjects: &config.PolicySubjects{Scopes: []string{fmt.Sprintf("scope-%05d", i)}},
		})
	}
	rt := tenantRoutes(t, tenants...)
	if n := len(rt.tenants.scan); n != 0 {
		t.Fatalf("scan partition holds %d single-scope selectors, want 0", n)
	}
	got, err := rt.resolveTenant(principal("u", "https://idp", nil, nil, "openid", "scope-04999"))
	if err != nil || got.id() != "t-04999" {
		t.Fatalf("resolve = (%q, %v), want t-04999", got.id(), err)
	}
	if got, err := rt.resolveTenant(principal("u", "https://idp", nil, nil, "scope-99999")); err != nil || got != nil {
		t.Fatalf("resolve = (%q, %v), want no tenant", got.id(), err)
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
		// A scope nobody can hold selects nobody, and a tenant that selects
		// nobody leaves its principals ungoverned rather than failing loudly.
		{"empty scope", tenantCfg(up.URL,
			config.Tenant{ID: "a", Subjects: &config.PolicySubjects{Scopes: []string{"read", ""}}}),
			"scopes must not contain empty values"},
		{"whitespace scope", tenantCfg(up.URL,
			config.Tenant{ID: "a", Subjects: &config.PolicySubjects{Scopes: []string{"  "}}}),
			"scopes must not contain empty values"},
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
