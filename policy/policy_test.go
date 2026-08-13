package policy

import (
	"testing"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
)

func TestAllowAllWhenAbsent(t *testing.T) {
	e := New(nil)
	if !e.Decide(nil, "any", "tools/call", "x").Allowed {
		t.Error("absent policy must allow all")
	}
}

func TestDenyByDefault(t *testing.T) {
	e := New(&config.Policy{DefaultDecision: "deny"})
	if e.Decide(nil, "any", "tools/call", "x").Allowed {
		t.Error("deny-by-default must deny with no rules")
	}
}

func TestFirstMatchingRuleAllows(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID:       "eng-github",
			Subjects: &config.PolicySubjects{Groups: []string{"engineering"}},
			Allow: []config.PolicyAllow{
				{Server: "github", Methods: []string{"tools/call"}, Names: []string{"get_*", "create_pr"}},
				{Server: "search"},
			},
		}},
	})
	eng := &auth.Principal{Subject: "alice", Groups: []string{"engineering"}}
	other := &auth.Principal{Subject: "bob", Groups: []string{"sales"}}

	cases := []struct {
		p       *auth.Principal
		server  string
		method  string
		name    string
		allowed bool
		rule    string
	}{
		{eng, "github", "tools/call", "get_repo", true, "eng-github"},
		{eng, "github", "tools/call", "create_pr", true, "eng-github"},
		{eng, "github", "tools/call", "delete_repo", false, ""},
		{eng, "github", "prompts/get", "get_repo", false, ""},
		{eng, "search", "tools/call", "anything", true, "eng-github"},
		{other, "github", "tools/call", "get_repo", false, ""},
		{nil, "github", "tools/call", "get_repo", false, ""},
	}
	for _, c := range cases {
		d := e.Decide(c.p, c.server, c.method, c.name)
		if d.Allowed != c.allowed || d.RuleID != c.rule {
			t.Errorf("Decide(%v, %s, %s, %s) = %+v, want allowed=%v rule=%q",
				c.p, c.server, c.method, c.name, d, c.allowed, c.rule)
		}
	}
}

func TestSubjectMatch(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID:       "alice-only",
			Subjects: &config.PolicySubjects{Subs: []string{"alice"}},
			Allow:    []config.PolicyAllow{{Server: "*"}},
		}},
	})
	if !e.Decide(&auth.Principal{Subject: "alice"}, "x", "tools/call", "t").Allowed {
		t.Error("sub match should allow")
	}
	if e.Decide(&auth.Principal{Subject: "eve"}, "x", "tools/call", "t").Allowed {
		t.Error("non-matching sub should deny")
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"get_*", "get_repo", true},
		{"get_*", "getrepo", false},
		{"*", "anything", true},
		{"exact", "exact", true},
		{"exact", "exact2", false},
		{"a*b*c", "aXbYc", true},
		{"a*b*c", "aXcYb", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}

func TestIssuerScoping(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID:       "corp-admins",
			Subjects: &config.PolicySubjects{Issuers: []string{"https://corp.okta.com"}, Groups: []string{"admins"}},
			Allow:    []config.PolicyAllow{{Server: "*"}},
		}},
	})
	corp := &auth.Principal{Subject: "alice", Issuer: "https://corp.okta.com", Groups: []string{"admins"}}
	// Same group name, different (lower-assurance) issuer must NOT match.
	partner := &auth.Principal{Subject: "mallory", Issuer: "https://partner.example", Groups: []string{"admins"}}

	if !e.Decide(corp, "db", "tools/call", "x").Allowed {
		t.Error("corp admin should be allowed")
	}
	if e.Decide(partner, "db", "tools/call", "x").Allowed {
		t.Error("partner-issuer principal must not match a corp-scoped rule")
	}
}

func TestClaimsMatching(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID: "eng-dept",
			Subjects: &config.PolicySubjects{
				Claims: map[string]any{"dept": "eng", "level": float64(3)},
			},
			Allow: []config.PolicyAllow{{Server: "*"}},
		}},
	})
	allow := func(p *auth.Principal) bool { return e.Decide(p, "x", "tools/call", "t").Allowed }

	// All required claims present and equal → match (ABAC without naming subjects).
	if !allow(&auth.Principal{Subject: "a", Claims: map[string]any{"dept": "eng", "level": float64(3)}}) {
		t.Error("matching claims should allow")
	}
	// Every required claim must match — one wrong value denies.
	if allow(&auth.Principal{Subject: "a", Claims: map[string]any{"dept": "eng", "level": float64(2)}}) {
		t.Error("wrong level should deny")
	}
	// Missing claim denies.
	if allow(&auth.Principal{Subject: "a", Claims: map[string]any{"dept": "eng"}}) {
		t.Error("missing level should deny")
	}
	// An array-valued token claim matches by membership.
	if !allow(&auth.Principal{Subject: "a", Claims: map[string]any{"dept": []any{"sales", "eng"}, "level": float64(3)}}) {
		t.Error("array claim containing the value should allow")
	}
	if allow(&auth.Principal{Subject: "a", Claims: map[string]any{"dept": []any{"sales"}, "level": float64(3)}}) {
		t.Error("array claim without the value should deny")
	}
	// Claim comparison is type-exact: a token asserting the string "3" does
	// not satisfy a rule requiring the number 3. Worth pinning, because the
	// comparison has a scalar fast path in front of reflect.DeepEqual and a
	// lenient one would quietly widen every claim-gated rule in the document.
	if allow(&auth.Principal{Subject: "a", Claims: map[string]any{"dept": "eng", "level": "3"}}) {
		t.Error("string \"3\" should not satisfy a rule requiring the number 3")
	}
	if allow(&auth.Principal{Subject: "a", Claims: map[string]any{"dept": true, "level": float64(3)}}) {
		t.Error("boolean true should not satisfy a rule requiring the string \"eng\"")
	}
	// No claims at all (e.g. a hand-built principal) denies.
	if allow(&auth.Principal{Subject: "a"}) {
		t.Error("principal without claims should deny")
	}
	// Nil principal (auth disabled) never matches a subjects-scoped rule.
	if allow(nil) {
		t.Error("nil principal should deny")
	}
}

func TestClaimsCombineWithGroups(t *testing.T) {
	// Claims gate like issuers: they are an additional requirement on top of
	// the subs/groups match, not an alternative to it.
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID: "eng-admins",
			Subjects: &config.PolicySubjects{
				Groups: []string{"admins"},
				Claims: map[string]any{"mfa": true},
			},
			Allow: []config.PolicyAllow{{Server: "*"}},
		}},
	})
	allow := func(p *auth.Principal) bool { return e.Decide(p, "x", "tools/call", "t").Allowed }

	if !allow(&auth.Principal{Subject: "a", Groups: []string{"admins"}, Claims: map[string]any{"mfa": true}}) {
		t.Error("group + claim should allow")
	}
	if allow(&auth.Principal{Subject: "a", Groups: []string{"admins"}, Claims: map[string]any{"mfa": false}}) {
		t.Error("group without claim gate should deny")
	}
	if allow(&auth.Principal{Subject: "a", Groups: []string{"users"}, Claims: map[string]any{"mfa": true}}) {
		t.Error("claim without group should deny")
	}
}

// TestDenyWinsRegardlessOfOrder is the whole claim of deny rules. The same two
// rules are evaluated in both orders and must reach the same answer — that is
// what "globally" means, and it is why fold does not use first-match here: a
// rule whose correctness depends on where it was pasted will eventually be
// pasted in the wrong place.
func TestDenyWinsRegardlessOfOrder(t *testing.T) {
	broadAllow := config.PolicyRule{
		ID:    "support-billing",
		Allow: []config.PolicyAllow{{Server: "billing"}},
	}
	carveOut := config.PolicyRule{
		ID:   "no-refunds",
		Deny: []config.PolicyAllow{{Server: "billing", Names: []string{"refund_*"}}},
	}

	for _, order := range []struct {
		name  string
		rules []config.PolicyRule
	}{
		{"allow first", []config.PolicyRule{broadAllow, carveOut}},
		{"deny first", []config.PolicyRule{carveOut, broadAllow}},
	} {
		e := New(&config.Policy{DefaultDecision: "deny", Rules: order.rules})
		if d := e.Decide(nil, "billing", "tools/call", "refund_order"); d.Allowed {
			t.Errorf("%s: refund_order allowed; deny must not be overridable", order.name)
		} else if d.RuleID != "no-refunds" {
			t.Errorf("%s: denial rule = %q, want no-refunds — an explicit refusal owes the operator its rule", order.name, d.RuleID)
		}
		if !e.Decide(nil, "billing", "tools/call", "read_invoice").Allowed {
			t.Errorf("%s: the carve-out swallowed the rest of the grant", order.name)
		}
	}
}

// TestDenyIsScopedToItsSubjects keeps deny from becoming a global kill switch:
// it is a rule like any other, and a rule that does not match this principal
// has nothing to say about them.
func TestDenyIsScopedToItsSubjects(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{
			{ID: "everyone-billing", Allow: []config.PolicyAllow{{Server: "billing"}}},
			{
				ID:       "contractors-no-refunds",
				Subjects: &config.PolicySubjects{Groups: []string{"contractors"}},
				Deny:     []config.PolicyAllow{{Server: "billing", Names: []string{"refund_*"}}},
			},
		},
	})
	staff := &auth.Principal{Subject: "alice", Groups: []string{"staff"}}
	contractor := &auth.Principal{Subject: "bob", Groups: []string{"contractors"}}

	if !e.Decide(staff, "billing", "tools/call", "refund_order").Allowed {
		t.Error("a deny scoped to contractors refused staff")
	}
	if e.Decide(contractor, "billing", "tools/call", "refund_order").Allowed {
		t.Error("contractor refund was not denied")
	}
}

// TestDenyRuleNeedsNoAllow: a document may carve out of a grant written
// elsewhere, so deny stands alone.
func TestDenyRuleNeedsNoAllow(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "allow",
		Rules:           []config.PolicyRule{{ID: "nobody-deletes", Deny: []config.PolicyAllow{{Server: "*", Names: []string{"delete_*"}}}}},
	})
	if e.Decide(nil, "any", "tools/call", "delete_everything").Allowed {
		t.Error("deny must override an allow-by-default posture too")
	}
	if !e.Decide(nil, "any", "tools/call", "list_things").Allowed {
		t.Error("unrelated call refused")
	}
}
