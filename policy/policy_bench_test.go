package policy

import (
	"fmt"
	"testing"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
)

// Policy is evaluated once per named invocation and once per item of every
// list it filters, so an allocation here is an allocation per tool on every
// tools/list. Patterns are compiled when the engine is built — once per
// routing snapshot — which is what keeps this at zero.
//
//	go test ./policy -run '^$' -bench BenchmarkDecide -benchmem
func BenchmarkDecide(b *testing.B) {
	var rules []config.PolicyRule
	// Non-matching rules ahead of the match: a real policy is scanned, not
	// indexed, so the miss path is the common one.
	for i := range 8 {
		rules = append(rules, config.PolicyRule{
			ID:       fmt.Sprintf("rule-%d", i),
			Subjects: &config.PolicySubjects{Groups: []string{fmt.Sprintf("group-%d", i)}},
			Allow: []config.PolicyAllow{{
				Server:  "*",
				Methods: []string{"tools/call"},
				Names:   []string{"*-tool-*", "admin-*", "search-*"},
			}},
		})
	}
	rules = append(rules, config.PolicyRule{
		ID:       "match",
		Subjects: &config.PolicySubjects{Groups: []string{"eng"}},
		Allow: []config.PolicyAllow{{
			Server: "up", Methods: []string{"tools/call"}, Names: []string{"up-tool-*"},
		}},
	})
	e := New(&config.Policy{DefaultDecision: "deny", Rules: rules})
	p := &auth.Principal{Issuer: "https://idp", Subject: "u1", Groups: []string{"eng"}}

	b.ReportAllocs()
	for b.Loop() {
		if !e.Decide(p, "up", "tools/call", "up-tool-042").Allowed {
			b.Fatal("expected allow")
		}
	}
}

// BenchmarkDecideWithDeny is the cost of deny-wins: evaluation can no longer
// stop at the first matching allow, because a deny later in the document still
// refuses. The comparison that matters is against BenchmarkDecide above — a
// document carrying no deny rule must be indistinguishable from one written
// before deny existed, which is what the hasDeny short-circuit buys.
//
//	go test ./policy -run '^$' -bench 'BenchmarkDecide' -benchmem
func BenchmarkDecideWithDeny(b *testing.B) {
	var rules []config.PolicyRule
	for i := range 8 {
		rules = append(rules, config.PolicyRule{
			ID:       fmt.Sprintf("rule-%d", i),
			Subjects: &config.PolicySubjects{Groups: []string{fmt.Sprintf("group-%d", i)}},
			Allow: []config.PolicyAllow{{
				Server:  "*",
				Methods: []string{"tools/call"},
				Names:   []string{"*-tool-*", "admin-*", "search-*"},
			}},
		})
	}
	rules = append(rules,
		config.PolicyRule{
			ID:       "match",
			Subjects: &config.PolicySubjects{Groups: []string{"eng"}},
			Allow: []config.PolicyAllow{{
				Server: "up", Methods: []string{"tools/call"}, Names: []string{"up-tool-*"},
			}},
		},
		// The carve-out that forces the full scan: it matches this principal,
		// so every decision pays the deny pass before reaching its allow.
		config.PolicyRule{
			ID:       "no-danger",
			Subjects: &config.PolicySubjects{Groups: []string{"eng"}},
			Deny: []config.PolicyAllow{{
				Server: "up", Methods: []string{"tools/call"}, Names: []string{"danger-*"},
			}},
		},
	)
	e := New(&config.Policy{DefaultDecision: "deny", Rules: rules})
	p := &auth.Principal{Issuer: "https://idp", Subject: "u1", Groups: []string{"eng"}}

	b.ReportAllocs()
	for b.Loop() {
		if !e.Decide(p, "up", "tools/call", "up-tool-042").Allowed {
			b.Fatal("expected allow")
		}
	}
}
