package gateway

import (
	"context"
	"fmt"

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

// tenantSet is the snapshot's resolved tenant list. It is a slice rather than
// a map because resolution matches selectors, not keys — see resolveTenant.
type tenantSet []*tenant

// buildTenants resolves tenant declarations for a snapshot.
func buildTenants(cfg *config.Config) tenantSet {
	if len(cfg.Tenants) == 0 {
		return nil
	}
	out := make(tenantSet, 0, len(cfg.Tenants))
	for i := range cfg.Tenants {
		tc := cfg.Tenants[i]
		t := &tenant{cfg: tc}
		if len(tc.Upstreams) > 0 {
			t.upstreams = make(map[string]bool, len(tc.Upstreams))
			for _, id := range tc.Upstreams {
				t.upstreams[id] = true
			}
		}
		out = append(out, t)
	}
	return out
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
// Matching is linear in the number of declarations and runs per authenticated
// request. That is fine for the tens of tenants a document realistically
// holds and is the open question the design names: whether the common case
// (one claim equalling one value) is worth indexing is a measurement, taken
// before enforcement depends on the answer.
func (rt *routes) resolveTenant(p *auth.Principal) (*tenant, *jsonrpc.Error) {
	if len(rt.tenants) == 0 || p == nil {
		return nil, nil
	}
	var found *tenant
	var all []string
	for _, t := range rt.tenants {
		if !policy.MatchSubjects(t.cfg.Subjects, p) {
			continue
		}
		if found == nil {
			found = t
			all = append(all, t.cfg.ID)
			continue
		}
		all = append(all, t.cfg.ID)
	}
	if len(all) > 1 {
		return nil, errAmbiguousTenant(p.Subject, all)
	}
	return found, nil
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
