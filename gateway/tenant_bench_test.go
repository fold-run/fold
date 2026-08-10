package gateway

import (
	"fmt"
	"testing"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/state"
)

// Phase 2 of docs/design-tenancy.md: the cardinality measurement.
//
// resolveTenant runs once per authenticated request and is linear in the
// number of declarations — and it cannot stop at the first match, because
// detecting ambiguity means seeing every match. For ten tenants that is free;
// the open question is where it stops being free, and whether the common case
// (one claim equalling one value) is worth indexing at snapshot time.
//
//	go test ./gateway -run '^$' -bench BenchmarkResolveTenant -benchmem
//
// Four selector shapes, because they index differently:
//
//   - claim    — one claim equalling one scalar. The shape candidate (1) in
//     the design proposes to index, and the one a per-customer document
//     repeats thousands of times.
//   - group    — a group-only selector. Indexed from the other side: the
//     principal's own groups are the lookup keys.
//   - compound — issuer plus claim plus group, which no index narrows. This
//     is what the scan partition actually holds, so it is the honest measure
//     of what stays linear.
//   - mixed    — mostly claim-selected with a minority compound, which is
//     what a real document looks like and what decides whether a partial
//     index is worth the fallback scan it still needs.
//
// Every case resolves a principal matching the *last* declaration, which is
// not pessimism about ordering — resolution visits every candidate regardless,
// because refusing ambiguity means seeing every match — but it keeps the match
// itself out of the noise.

func benchTenants(shape string, n int) tenantSet {
	cfg := &config.Config{}
	for i := range n {
		var subs *config.PolicySubjects
		switch {
		case shape == "claim", shape == "mixed" && i%8 != 0:
			subs = &config.PolicySubjects{Claims: map[string]any{"org_id": fmt.Sprintf("org-%05d", i)}}
		case shape == "group":
			subs = &config.PolicySubjects{Groups: []string{fmt.Sprintf("group-%05d", i)}}
		default:
			subs = &config.PolicySubjects{
				Issuers: []string{"https://idp.example.com"},
				Claims:  map[string]any{"org_id": fmt.Sprintf("org-%05d", i)},
				Groups:  []string{fmt.Sprintf("group-%05d", i)},
			}
		}
		cfg.Tenants = append(cfg.Tenants, config.Tenant{ID: fmt.Sprintf("t-%05d", i), Subjects: subs})
	}
	return buildTenants(cfg, state.NewMemory(), tenantSet{})
}

// benchPrincipal carries a claim set of realistic width — a resolver that
// looks up one claim should not care how many others the token holds, and a
// matcher that scans them does.
func benchPrincipal(orgID string, groups []string) *auth.Principal {
	return &auth.Principal{
		Subject: "user-42",
		Issuer:  "https://idp.example.com",
		Groups:  groups,
		Claims: map[string]any{
			"org_id": orgID,
			"sub":    "user-42",
			"iss":    "https://idp.example.com",
			"aud":    "https://fold.example.com",
			"email":  "user-42@example.com",
			"scope":  "mcp:invoke",
		},
	}
}

func BenchmarkResolveTenant(b *testing.B) {
	for _, shape := range []string{"claim", "group", "compound", "mixed"} {
		for _, n := range []int{10, 100, 1000, 10000} {
			b.Run(fmt.Sprintf("%s/n%d", shape, n), func(b *testing.B) {
				rt := &routes{tenants: benchTenants(shape, n)}
				last := n - 1
				p := benchPrincipal(fmt.Sprintf("org-%05d", last), []string{
					"engineering", "oncall", fmt.Sprintf("group-%05d", last),
				})
				if got, err := rt.resolveTenant(p); err != nil || got == nil {
					b.Fatalf("resolve: %v, %v", got, err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if _, err := rt.resolveTenant(p); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// The unmatched principal pays the same scan and resolves to nothing — worth
// measuring separately because it is the steady state for any deployment that
// declares tenants for some callers and not others.
func BenchmarkResolveTenantUnmatched(b *testing.B) {
	for _, n := range []int{100, 10000} {
		b.Run(fmt.Sprintf("n%d", n), func(b *testing.B) {
			rt := &routes{tenants: benchTenants("mixed", n)}
			p := benchPrincipal("org-not-declared", []string{"engineering", "oncall"})
			if got, _ := rt.resolveTenant(p); got != nil {
				b.Fatalf("tenant = %q, want none", got.id())
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := rt.resolveTenant(p); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
