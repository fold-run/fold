package gateway

import (
	"context"
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/policy"
)

// A tenant is one resolved tenant declaration, built once per routing
// snapshot. Phase 1 of docs/design-tenancy.md resolves tenants and carries
// them on the request; the limits below are enforced in a later phase.
type tenant struct {
	cfg config.Tenant
	// upstreams is the visibility subset as a set, empty meaning "all".
	upstreams map[string]bool
}

// tenantSet is the snapshot's resolved tenants, partitioned for lookup.
//
// Resolution matches selectors rather than keys, so the general case is a
// scan — but the case that *grows* is not the general one. A document with
// thousands of tenants got there by writing one per customer, and those
// selectors name a single dimension: this claim equals that value, or this
// group. Both are map lookups, taken from the opposite side — the claim index
// is keyed by what the tenant requires, the group index by what the principal
// holds. So the set is an index plus a scan list for the compound selectors
// the index cannot narrow, and only that list is walked per request. See
// docs/design-tenancy.md, "the cardinality problem", and the measurement in
// docs/benchmarks.md.
type tenantSet struct {
	// byClaim indexes single-claim selectors: claim name → required value →
	// the tenants requiring it. The value keys are JSON scalars only, which
	// is what keeps a lookup both safe (a non-comparable key would panic)
	// and exact (for same-typed scalars, equality is what the matcher does).
	byClaim map[string]map[any][]*tenant
	// byGroup indexes group-only selectors: group → the tenants naming it.
	// A selector naming several groups appears under each, since holding any
	// one of them matches; the same tenant reached twice is deduplicated at
	// resolution.
	byGroup map[string][]*tenant
	// scan holds every tenant neither index covers — compound selectors
	// (issuer plus claims, groups plus claims), selectors naming individual
	// subjects, and claim values that are not scalars. Matched one by one.
	scan []*tenant
	// total counts every partition.
	total int
}

// count returns how many tenants the snapshot declares.
func (ts tenantSet) count() int { return ts.total }

// buildTenants resolves tenant declarations for a snapshot.
func buildTenants(cfg *config.Config) tenantSet {
	var ts tenantSet
	for i := range cfg.Tenants {
		tc := cfg.Tenants[i]
		t := &tenant{cfg: tc}
		if len(tc.Upstreams) > 0 {
			t.upstreams = make(map[string]bool, len(tc.Upstreams))
			for _, id := range tc.Upstreams {
				t.upstreams[id] = true
			}
		}
		ts.total++
		ts.index(t)
	}
	return ts
}

// index files one tenant under whichever index its selector allows, falling
// back to the scan list.
func (ts *tenantSet) index(t *tenant) {
	s := t.cfg.Subjects
	if key, val, ok := indexableClaim(s); ok {
		if ts.byClaim == nil {
			ts.byClaim = make(map[string]map[any][]*tenant)
		}
		byValue := ts.byClaim[key]
		if byValue == nil {
			byValue = make(map[any][]*tenant)
			ts.byClaim[key] = byValue
		}
		byValue[val] = append(byValue[val], t)
		return
	}
	if indexableGroups(s) {
		if ts.byGroup == nil {
			ts.byGroup = make(map[string][]*tenant)
		}
		for _, g := range s.Groups {
			ts.byGroup[g] = append(ts.byGroup[g], t)
		}
		return
	}
	ts.scan = append(ts.scan, t)
}

// indexableClaim reports whether a selector is one claim equal to one JSON
// scalar, with no other condition.
//
// The restriction to string/float64/bool is not caution about performance but
// about correctness. Config is JSON, so those are the types a claim value
// arrives as; admitting anything else would mean map keys whose equality is
// not the equality the matcher uses — pointers compare by address in a map and
// by pointee in reflect.DeepEqual — and a wrong key is a missed match, which
// for tenancy means a caller governed as though they had no tenant. Selectors
// of any other shape are matched by the scan, which is the general case.
func indexableClaim(s *config.PolicySubjects) (string, any, bool) {
	if s == nil || len(s.Claims) != 1 || len(s.Issuers) > 0 || len(s.Subs) > 0 || len(s.Groups) > 0 {
		return "", nil, false
	}
	for key, val := range s.Claims {
		switch val.(type) {
		case string, float64, bool:
			return key, val, true
		}
	}
	return "", nil, false
}

// indexableGroups reports whether a selector names groups and nothing else,
// in which case the principal's own groups are the lookup keys.
func indexableGroups(s *config.PolicySubjects) bool {
	return s != nil && len(s.Groups) > 0 && len(s.Claims) == 0 && len(s.Issuers) == 0 && len(s.Subs) == 0
}

// indexKey returns v as a lookup key when it is one of the scalar types the
// index holds, and nil otherwise. A map lookup with a non-comparable key
// (a decoded object or array) panics, and nil is never a stored key, so an
// unindexable value simply finds nothing and falls through to the scan.
func indexKey(v any) any {
	switch v.(type) {
	case string, float64, bool:
		return v
	}
	return nil
}

// sees reports whether this tenant may see an upstream at all. Policy remains
// the authority on what may be invoked; this is the coarser cut.
func (t *tenant) sees(upstreamID string) bool {
	if t == nil || len(t.upstreams) == 0 {
		return true
	}
	return t.upstreams[upstreamID]
}

// id returns the tenant's id, or "" for an unmatched principal.
func (t *tenant) id() string {
	if t == nil {
		return ""
	}
	return t.cfg.ID
}

// errAmbiguousTenant is returned when a principal matches more than one
// tenant. It is deliberately not a minted error code: a client cannot act on
// it, because it is the operator's configuration that is wrong.
//
// Refusing is the point. Serving the request would mean picking a tenant, and
// picking means some caller silently gets another tenant's allowance and
// another tenant's visibility — the exact failure the design refuses to
// resolve by precedence. Static validation cannot catch this (selector
// overlap is not decidable for the shapes fold supports), so it is caught
// here, against a real principal, and made loud.
func errAmbiguousTenant(sub string, ids []string) *jsonrpc.Error {
	return &jsonrpc.Error{
		Code: jsonrpc.CodeInternalError,
		Message: fmt.Sprintf(
			"tenant configuration is ambiguous: principal %q matches tenants %v; a principal must match at most one",
			sub, ids),
	}
}

// resolveTenant finds the single tenant a principal belongs to.
//
// It runs once per authenticated request, and it cannot stop at the first
// match: refusing ambiguity means seeing every match, not the first one. So
// the cost is the whole set — which is why the indexes exist, and why the scan
// partition is the only thing that grows linearly. A document of ten thousand
// single-dimension tenants resolves in the same time as a document of ten
// (docs/benchmarks.md).
//
// The index narrows; policy decides. Every candidate the index produces is
// still put to policy.MatchSubjects, so there remains exactly one definition
// of who a selector covers and the index can only ever be an accelerator —
// the failure it could otherwise introduce is silently placing a caller in
// the wrong tenant.
func (rt *routes) resolveTenant(p *auth.Principal) (*tenant, *jsonrpc.Error) {
	ts := rt.tenants
	if ts.total == 0 || p == nil {
		return nil, nil
	}
	var found *tenant
	var also []*tenant
	for key, byValue := range ts.byClaim {
		got, ok := p.Claims[key]
		if !ok {
			continue
		}
		// A claim holding an array matches if any element does, which is the
		// rule the matcher applies — so every element is a lookup.
		if arr, isArr := got.([]any); isArr {
			for _, v := range arr {
				found, also = considerTenants(byValue[indexKey(v)], p, found, also)
			}
			continue
		}
		found, also = considerTenants(byValue[indexKey(got)], p, found, also)
	}
	if ts.byGroup != nil {
		for _, g := range p.Groups {
			found, also = considerTenants(ts.byGroup[g], p, found, also)
		}
	}
	for _, t := range ts.scan {
		if policy.MatchSubjects(t.cfg.Subjects, p) {
			found, also = considerTenant(t, found, also)
		}
	}
	if len(also) > 0 {
		ids := make([]string, 0, len(also)+1)
		ids = append(ids, found.cfg.ID)
		for _, t := range also {
			ids = append(ids, t.cfg.ID)
		}
		// Sorted because half the matches came out of a map, and an error
		// naming the same two tenants in a different order each time reads
		// like two different problems.
		slices.Sort(ids)
		return nil, errAmbiguousTenant(p.Subject, ids)
	}
	return found, nil
}

// considerTenants records the index's candidates that the matcher confirms.
func considerTenants(candidates []*tenant, p *auth.Principal, found *tenant, also []*tenant) (*tenant, []*tenant) {
	for _, t := range candidates {
		if !policy.MatchSubjects(t.cfg.Subjects, p) {
			continue
		}
		found, also = considerTenant(t, found, also)
	}
	return found, also
}

// considerTenant records one match, deduplicating. The same tenant can be
// reached twice — a claim array holding the same value twice is enough — and
// counting that as ambiguity would refuse a caller over a duplicated string.
//
// State is threaded through arguments rather than captured in a closure so
// resolution stays allocation-free on the common path: nothing is allocated
// unless a principal genuinely matches more than one tenant, which is the
// path that ends in a refusal anyway.
func considerTenant(t, found *tenant, also []*tenant) (*tenant, []*tenant) {
	if found == nil {
		return t, also
	}
	if found == t || slices.Contains(also, t) {
		return found, also
	}
	return found, append(also, t)
}

// tenantKey carries the resolved tenant through the request.
type tenantKey struct{}

// withTenant attaches the resolved tenant to ctx. A nil tenant is still
// attached, so a later reader can distinguish "no tenant" from "not resolved".
func withTenant(ctx context.Context, t *tenant) context.Context {
	return context.WithValue(ctx, tenantKey{}, t)
}

// tenantFrom returns the request's tenant, or nil when the principal matched
// none (or when tenancy is not configured).
func tenantFrom(ctx context.Context) *tenant {
	t, _ := ctx.Value(tenantKey{}).(*tenant)
	return t
}
