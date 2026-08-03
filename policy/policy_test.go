package policy

import (
	"testing"

	"github.com/fold-run/fold-go/auth"
	"github.com/fold-run/fold-go/config"
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
