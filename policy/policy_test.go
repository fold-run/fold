package policy

import (
	"strings"
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

// TestArgumentConstraints covers the grant that names a condition rather than
// just a tool: "may deploy, but only to staging".
func TestArgumentConstraints(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID: "staging-only",
			Allow: []config.PolicyAllow{{
				Server: "deploy", Names: []string{"deploy"},
				Args: map[string]any{"target.environment": "staging"},
			}},
		}},
	})
	args := func(m map[string]any) ArgSource {
		return func() (map[string]any, bool) { return m, true }
	}

	cases := []struct {
		name    string
		args    ArgSource
		allowed bool
		failed  string
	}{
		{"matching nested value", args(map[string]any{"target": map[string]any{"environment": "staging"}}), true, ""},
		{"wrong value", args(map[string]any{"target": map[string]any{"environment": "production"}}), false, "target.environment"},
		{"path absent", args(map[string]any{"target": map[string]any{}}), false, "target.environment"},
		{"parent not an object", args(map[string]any{"target": "staging"}), false, "target.environment"},
		{"no arguments at all", nil, false, "target.environment"},
	}
	for _, c := range cases {
		d := e.DecideCall(nil, "deploy", "tools/call", "deploy", Evidence{Args: c.args})
		if d.Allowed != c.allowed {
			t.Errorf("%s: allowed = %v, want %v", c.name, d.Allowed, c.allowed)
		}
		if d.FailedArg != c.failed {
			t.Errorf("%s: failed path = %q, want %q", c.name, d.FailedArg, c.failed)
		}
	}
}

// TestArgumentTypesAreExact: "1" and 1 are different values, because a lenient
// comparison silently widens the grant — the same rule subject claims follow.
func TestArgumentTypesAreExact(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID:    "replicas-one",
			Allow: []config.PolicyAllow{{Server: "deploy", Args: map[string]any{"replicas": float64(1)}}},
		}},
	})
	num := func() (map[string]any, bool) { return map[string]any{"replicas": float64(1)}, true }
	str := func() (map[string]any, bool) { return map[string]any{"replicas": "1"}, true }

	if !e.DecideCall(nil, "deploy", "tools/call", "scale", Evidence{Args: num}).Allowed {
		t.Error("numeric 1 did not satisfy a numeric constraint")
	}
	if e.DecideCall(nil, "deploy", "tools/call", "scale", Evidence{Args: str}).Allowed {
		t.Error(`string "1" satisfied a numeric constraint`)
	}
}

// TestArgumentConstrainedToolStaysVisible is the asymmetry the design record
// insists on stating rather than letting someone discover: there are no
// arguments at list time, so a constrained tool is visible and only
// conditionally callable. An operator who needs the stronger guarantee grants
// by name.
func TestArgumentConstrainedToolStaysVisible(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID:    "staging-only",
			Allow: []config.PolicyAllow{{Server: "deploy", Names: []string{"deploy"}, Args: map[string]any{"environment": "staging"}}},
		}},
	})
	if !e.Visible(nil, "deploy", "tools/call", "deploy") {
		t.Error("an argument-constrained tool must still list; there are no arguments to judge at list time")
	}
	prod := func() (map[string]any, bool) { return map[string]any{"environment": "production"}, true }
	if e.DecideCall(nil, "deploy", "tools/call", "deploy", Evidence{Args: prod}).Allowed {
		t.Error("visible must not mean callable with any arguments")
	}
}

// TestUnconstrainedClauseWinsOverConstrained: a later clause granting the tool
// outright must not be skipped because an earlier constrained one missed.
func TestUnconstrainedClauseWinsOverConstrained(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID: "two-ways",
			Allow: []config.PolicyAllow{
				{Server: "deploy", Names: []string{"deploy"}, Args: map[string]any{"environment": "staging"}},
				{Server: "deploy", Names: []string{"deploy"}},
			},
		}},
	})
	prod := func() (map[string]any, bool) { return map[string]any{"environment": "production"}, true }
	d := e.DecideCall(nil, "deploy", "tools/call", "deploy", Evidence{Args: prod})
	if !d.Allowed {
		t.Error("the unconstrained grant was skipped after a constrained near-miss")
	}
	if d.FailedArg != "" {
		t.Errorf("an allowed decision reported a failed constraint %q", d.FailedArg)
	}
}

// TestDenyWithArgumentsDoesNotHide: a deny that cannot be evaluated at list
// time must not hide the tool, or "no deploys to prod" would remove deploy
// entirely — applying the rule far more broadly than it was written.
func TestDenyWithArgumentsDoesNotHide(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{
			{ID: "can-deploy", Allow: []config.PolicyAllow{{Server: "deploy"}}},
			{ID: "not-prod", Deny: []config.PolicyAllow{{Server: "deploy", Args: map[string]any{"environment": "production"}}}},
		},
	})
	if !e.Visible(nil, "deploy", "tools/call", "deploy") {
		t.Error("a conditionally-denied tool was hidden outright")
	}
	prod := func() (map[string]any, bool) { return map[string]any{"environment": "production"}, true }
	staging := func() (map[string]any, bool) { return map[string]any{"environment": "staging"}, true }
	if e.DecideCall(nil, "deploy", "tools/call", "deploy", Evidence{Args: prod}).Allowed {
		t.Error("the prod deploy was not denied")
	}
	if !e.DecideCall(nil, "deploy", "tools/call", "deploy", Evidence{Args: staging}).Allowed {
		t.Error("the staging deploy was refused by a prod-only deny")
	}
}

// anno builds an annotation source for a known tool.
func anno(readOnly, destructive bool) AnnotationSource {
	return func() (ToolAnnotations, bool) {
		return ToolAnnotations{ReadOnly: readOnly, Destructive: destructive}, true
	}
}

// TestToolKindGating covers "read anything, write nothing" without naming a
// tool, and takes the MCP spec's defaults as written: an unannotated tool is
// not read-only and is destructive, so it fails both gates.
func TestToolKindGating(t *testing.T) {
	engine := func(kind string) *Engine {
		return New(&config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "by-kind",
				Allow: []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, ToolKind: kind}},
			}},
		})
	}
	unannotated := anno(false, true) // the spec's defaults

	cases := []struct {
		kind    string
		tool    AnnotationSource
		allowed bool
		what    string
	}{
		{"readOnly", anno(true, false), true, "a read-only tool"},
		{"readOnly", anno(false, false), false, "a non-destructive but writing tool"},
		{"readOnly", unannotated, false, "an unannotated tool"},
		{"nonDestructive", anno(false, false), true, "an explicitly non-destructive tool"},
		{"nonDestructive", anno(true, false), true, "a read-only tool, non-destructive by construction"},
		{"nonDestructive", anno(false, true), false, "a destructive tool"},
		{"nonDestructive", unannotated, false, "an unannotated tool"},
	}
	for _, c := range cases {
		got := engine(c.kind).DecideCall(nil, "u", "tools/call", "x", Evidence{Tool: c.tool}).Allowed
		if got != c.allowed {
			t.Errorf("toolKind %q with %s: allowed = %v, want %v", c.kind, c.what, got, c.allowed)
		}
	}
}

// TestUnknownAnnotationsDeny is the fail-safe the design record insists on:
// a gate that cannot see what it is gating must refuse, so the limitation is
// visible the first time someone hits it rather than silently permissive.
func TestUnknownAnnotationsDeny(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID:    "reads",
			Allow: []config.PolicyAllow{{Server: "*", ToolKind: "readOnly"}},
		}},
	})
	unknown := func() (ToolAnnotations, bool) { return ToolAnnotations{}, false }

	if e.DecideCall(nil, "u", "tools/call", "x", Evidence{Tool: unknown}).Allowed {
		t.Error("a tool whose annotations could not be established was allowed")
	}
	if e.DecideCall(nil, "u", "tools/call", "x", Evidence{}).Allowed {
		t.Error("a missing annotation source was allowed")
	}
	// And it must not accidentally leak through the list path either.
	if e.DecideList(nil, "u", "tools/call", "x", unknown).Allowed {
		t.Error("unknown annotations were visible in a list")
	}
}

// TestToolKindFiltersLists is the difference from argument constraints:
// annotations arrive with the tool, so a kind gate can filter a list rather
// than only refusing a call.
func TestToolKindFiltersLists(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID:    "reads",
			Allow: []config.PolicyAllow{{Server: "*", Methods: []string{"tools/call"}, ToolKind: "readOnly"}},
		}},
	})
	if !e.DecideList(nil, "u", "tools/call", "search", anno(true, false)).Allowed {
		t.Error("a read-only tool was hidden from a read-only grant")
	}
	if e.DecideList(nil, "u", "tools/call", "delete", anno(false, true)).Allowed {
		t.Error("a destructive tool was listed under a read-only grant")
	}
}

// ---- scope subjects ----
//
// A scope is not another way to name an identity, which is what makes it the
// odd one out among subject selectors: subs and groups are alternatives, and
// holding any one of them is enough, but a list of scopes is a list of
// requirements. These pin that reading, and the disclosure rules that bound
// what a denial is allowed to say about it.

// scopedEngine builds a deny-by-default document from one scope-gated rule.
func scopedEngine(subjects *config.PolicySubjects, allow ...config.PolicyAllow) *Engine {
	return New(&config.Policy{
		DefaultDecision: "deny",
		Rules:           []config.PolicyRule{{ID: "scoped", Subjects: subjects, Allow: allow}},
	})
}

// held builds a principal carrying scopes and nothing else worth matching.
func held(scopes ...string) *auth.Principal {
	return &auth.Principal{Subject: "alice", Scopes: scopes}
}

// Every named scope must be held: a subset is not a match, and a superset is.
// The subset case is the one that matters — read as alternatives, "read and
// write" would be satisfied by "read", which is the grant an operator was
// trying to withhold.
func TestScopesAreConjunctive(t *testing.T) {
	e := scopedEngine(&config.PolicySubjects{Scopes: []string{"read", "write"}},
		config.PolicyAllow{Server: "docs"})

	cases := []struct {
		name    string
		scopes  []string
		allowed bool
	}{
		{"holds both", []string{"read", "write"}, true},
		{"holds both, in the other order", []string{"write", "read"}, true},
		{"holds a superset", []string{"admin", "write", "read"}, true},
		{"holds a subset", []string{"read"}, false},
		{"holds the other half", []string{"write"}, false},
		{"holds none", nil, false},
		{"holds something else entirely", []string{"admin"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := e.Decide(held(c.scopes...), "docs", "tools/call", "read_doc").Allowed; got != c.allowed {
				t.Errorf("allowed = %v, want %v", got, c.allowed)
			}
		})
	}
}

// A selector naming only scopes is sufficient on its own, the way issuers and
// claims are: it says "whoever was granted this", which is a complete answer
// to who a rule covers. The alternative reading — that a scope only qualifies
// a sub or group match — would make a scopes-only rule match nobody, which is
// the failure mode that silently withdraws a grant.
func TestScopeOnlySelectorIsSufficient(t *testing.T) {
	e := scopedEngine(&config.PolicySubjects{Scopes: []string{"mcp:invoke"}},
		config.PolicyAllow{Server: "docs"})

	if !e.Decide(held("mcp:invoke"), "docs", "tools/call", "read_doc").Allowed {
		t.Error("a scopes-only rule matched nobody; it must stand on its own like issuers and claims")
	}
	if e.Decide(held("other"), "docs", "tools/call", "read_doc").Allowed {
		t.Error("a caller without the scope was allowed")
	}
	// A nil principal (auth disabled) holds no scopes, so it never matches.
	if e.Decide(nil, "docs", "tools/call", "read_doc").Allowed {
		t.Error("a nil principal satisfied a scope requirement")
	}
}

// The denial names only what the caller lacks, never the whole requirement.
// Re-authorizing accumulates: a caller told to obtain "read write" may come
// back with a token that dropped something else they held, so the remedy has
// to be stated as the delta.
func TestMissingScopesNameOnlyWhatIsLacked(t *testing.T) {
	e := scopedEngine(&config.PolicySubjects{Scopes: []string{"read", "write", "admin"}},
		config.PolicyAllow{Server: "docs"})

	d := e.DecideCall(held("read"), "docs", "tools/call", "edit_doc", Evidence{})
	if d.Allowed {
		t.Fatal("a caller holding one of three scopes was allowed")
	}
	if got := strings.Join(d.MissingScopes, ","); got != "write,admin" {
		t.Errorf("missing scopes = %q, want %q — only the shortfall, in the order the rule names it", got, "write,admin")
	}
	// Holding everything means no shortfall to report.
	if d := e.DecideCall(held("read", "write", "admin"), "docs", "tools/call", "edit_doc", Evidence{}); !d.Allowed {
		t.Fatal("a caller holding every scope was denied")
	} else if len(d.MissingScopes) > 0 {
		t.Errorf("an allowed decision reported missing scopes %v", d.MissingScopes)
	}
}

// Every rule that would have granted contributes, deduplicated. Reporting
// only the first rule's shortfall turns one re-authorization into as many
// round trips as there are rules — the caller obtains what they were told to,
// retries, and is refused by the next.
func TestMissingScopesUnionAcrossRules(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{
			{
				ID:       "writers",
				Subjects: &config.PolicySubjects{Scopes: []string{"read", "write"}},
				Allow:    []config.PolicyAllow{{Server: "docs", Names: []string{"edit_*"}}},
			},
			{
				ID:       "admins",
				Subjects: &config.PolicySubjects{Scopes: []string{"read", "admin"}},
				Allow:    []config.PolicyAllow{{Server: "docs"}},
			},
		},
	})

	d := e.DecideCall(held("read"), "docs", "tools/call", "edit_doc", Evidence{})
	if d.Allowed {
		t.Fatal("allowed with neither rule satisfied")
	}
	if got := strings.Join(d.MissingScopes, ","); got != "write,admin" {
		t.Errorf("missing scopes = %q, want %q — both rules' shortfalls, deduplicated", got, "write,admin")
	}
	// "read" is lacked by neither rule and must appear once at most; a caller
	// holding nothing sees it exactly once even though both rules want it.
	d = e.DecideCall(held(), "docs", "tools/call", "edit_doc", Evidence{})
	seen := map[string]int{}
	for _, s := range d.MissingScopes {
		seen[s]++
	}
	if seen["read"] != 1 {
		t.Errorf("missing scopes = %v, want \"read\" exactly once", d.MissingScopes)
	}
}

// The first disclosure rule: a rule contributes only when it targets this
// exact invocation. A shortfall reported for a rule about another server
// would tell the caller a rule exists for something they cannot reach — and
// would name the scope guarding it.
func TestMissingScopesStayWithTheirTarget(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{
			{
				ID:       "payroll-admins",
				Subjects: &config.PolicySubjects{Scopes: []string{"payroll:admin"}},
				Allow:    []config.PolicyAllow{{Server: "payroll"}},
			},
			{
				ID:       "docs-writers",
				Subjects: &config.PolicySubjects{Scopes: []string{"docs:write"}},
				Allow:    []config.PolicyAllow{{Server: "docs", Methods: []string{"tools/call"}, Names: []string{"edit_*"}}},
			},
		},
	})
	p := held("docs:read")

	// A call on docs learns about the docs rule and nothing about payroll.
	d := e.DecideCall(p, "docs", "tools/call", "edit_doc", Evidence{})
	if got := strings.Join(d.MissingScopes, ","); got != "docs:write" {
		t.Errorf("missing scopes = %q, want only docs:write — the payroll rule is not about this call", got)
	}
	// A call on the other server that no rule targets discloses nothing.
	if d := e.DecideCall(p, "docs", "tools/call", "delete_doc", Evidence{}); len(d.MissingScopes) > 0 {
		t.Errorf("missing scopes = %v for a name no rule grants, want none", d.MissingScopes)
	}
	if d := e.DecideCall(p, "docs", "prompts/get", "edit_doc", Evidence{}); len(d.MissingScopes) > 0 {
		t.Errorf("missing scopes = %v for a method no rule grants, want none", d.MissingScopes)
	}
	// And the payroll rule stays silent for a caller who could never satisfy
	// the rest of it either.
	if d := e.DecideCall(p, "payroll", "tools/call", "run_payroll", Evidence{}); strings.Join(d.MissingScopes, ",") != "payroll:admin" {
		t.Errorf("missing scopes = %v on the payroll call, want payroll:admin", d.MissingScopes)
	}
}

// The second disclosure rule: scopes must have been the *sole* obstacle. A
// caller in the wrong group learns nothing, because a shortfall named for a
// rule they failed on other grounds discloses a requirement guarding
// something they were never going to reach.
func TestMissingScopesRequireEveryOtherConditionMet(t *testing.T) {
	e := scopedEngine(
		&config.PolicySubjects{Groups: []string{"finance"}, Scopes: []string{"payroll:admin"}},
		config.PolicyAllow{Server: "payroll"},
	)
	call := func(p *auth.Principal) Decision {
		return e.DecideCall(p, "payroll", "tools/call", "run_payroll", Evidence{})
	}

	// Wrong group and missing scope: nothing is disclosed.
	wrong := &auth.Principal{Subject: "mallory", Groups: []string{"sales"}}
	if d := call(wrong); d.Allowed || len(d.MissingScopes) > 0 {
		t.Errorf("decision = %+v, want a denial disclosing nothing to an out-of-group caller", d)
	}
	// Wrong group but holding the scope: still nothing, because the group is
	// what refused them.
	if d := call(&auth.Principal{Subject: "mallory", Groups: []string{"sales"}, Scopes: []string{"payroll:admin"}}); d.Allowed || len(d.MissingScopes) > 0 {
		t.Errorf("decision = %+v, want a denial disclosing nothing", d)
	}
	// Right group, missing scope: this is the caller the shortfall is for.
	d := call(&auth.Principal{Subject: "alice", Groups: []string{"finance"}})
	if d.Allowed {
		t.Fatal("an in-group caller without the scope was allowed")
	}
	if got := strings.Join(d.MissingScopes, ","); got != "payroll:admin" {
		t.Errorf("missing scopes = %q, want payroll:admin", got)
	}
	// The same asymmetry for issuers, the other gate that runs before scopes.
	issuerGated := scopedEngine(
		&config.PolicySubjects{Issuers: []string{"https://corp.idp"}, Scopes: []string{"payroll:admin"}},
		config.PolicyAllow{Server: "payroll"},
	)
	partner := &auth.Principal{Subject: "mallory", Issuer: "https://partner.example"}
	if d := issuerGated.DecideCall(partner, "payroll", "tools/call", "run_payroll", Evidence{}); len(d.MissingScopes) > 0 {
		t.Errorf("missing scopes = %v for a principal from another issuer, want none", d.MissingScopes)
	}
}

// Lists never disclose. There is no denial message for a filtered item — the
// item simply is not there — so computing a shortfall for one would be work
// nobody reads, and a Decision carrying it could only leak by being logged.
func TestListDecisionsDiscloseNoScopes(t *testing.T) {
	e := scopedEngine(&config.PolicySubjects{Scopes: []string{"docs:write"}},
		config.PolicyAllow{Server: "docs", Methods: []string{"tools/call"}, Names: []string{"edit_doc"}})
	p := held("docs:read")

	if d := e.Decide(p, "docs", "tools/call", "edit_doc"); d.Allowed || len(d.MissingScopes) > 0 {
		t.Errorf("Decide = %+v, want a denial with no shortfall", d)
	}
	if d := e.DecideList(p, "docs", "tools/call", "edit_doc", nil); d.Allowed || len(d.MissingScopes) > 0 {
		t.Errorf("DecideList = %+v, want a denial with no shortfall", d)
	}
	if e.Visible(p, "docs", "tools/call", "edit_doc") {
		t.Error("a tool the caller lacks the scope for was visible")
	}
	// The enforcement pair: invisible and denied, with the remedy only on
	// the call.
	d := e.DecideCall(p, "docs", "tools/call", "edit_doc", Evidence{})
	if d.Allowed || len(d.MissingScopes) != 1 {
		t.Errorf("DecideCall = %+v, want a denial naming the one missing scope", d)
	}
}

// A document naming no scope pays nothing and says nothing: the accounting is
// gated on the engine having seen one, so every denial in every existing
// document keeps the shape it had.
func TestDocumentWithoutScopeRulesReportsNoShortfall(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID:       "eng",
			Subjects: &config.PolicySubjects{Groups: []string{"engineering"}},
			Allow:    []config.PolicyAllow{{Server: "docs"}},
		}},
	})
	if e.hasScopes {
		t.Error("a document naming no scope reported hasScopes")
	}
	for _, p := range []*auth.Principal{
		nil,
		held("read", "write"),
		{Subject: "bob", Groups: []string{"sales"}},
	} {
		if d := e.DecideCall(p, "docs", "tools/call", "edit_doc", Evidence{}); len(d.MissingScopes) > 0 {
			t.Errorf("principal %v: missing scopes = %v, want none", p, d.MissingScopes)
		}
	}
	// And a scope-gated document sets it, so the gate is not simply always
	// false.
	if !scopedEngine(&config.PolicySubjects{Scopes: []string{"read"}}, config.PolicyAllow{Server: "docs"}).hasScopes {
		t.Error("a scope-gated document did not report hasScopes")
	}
}

// Deny is unaffected: it is a rule like any other, so a scope selector scopes
// it to the callers who hold those scopes, and it still wins over an allow.
// A denial by an explicit deny discloses nothing — there is no shortfall that
// would fix it.
func TestDenyRulesWithScopeSubjects(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{
			{
				ID:       "everyone-reads",
				Subjects: &config.PolicySubjects{Scopes: []string{"docs:read"}},
				Allow:    []config.PolicyAllow{{Server: "docs"}},
			},
			{
				ID:       "no-deletes-for-interns",
				Subjects: &config.PolicySubjects{Scopes: []string{"docs:read", "intern"}},
				Deny:     []config.PolicyAllow{{Server: "docs", Names: []string{"delete_*"}}},
			},
		},
	})
	intern := held("docs:read", "intern")
	staff := held("docs:read")

	d := e.DecideCall(intern, "docs", "tools/call", "delete_doc", Evidence{})
	if d.Allowed {
		t.Fatal("the scope-gated deny did not apply to the caller holding its scopes")
	}
	if d.RuleID != "no-deletes-for-interns" {
		t.Errorf("denying rule = %q, want no-deletes-for-interns", d.RuleID)
	}
	if len(d.MissingScopes) > 0 {
		t.Errorf("an explicit deny disclosed missing scopes %v; there is nothing the caller could obtain", d.MissingScopes)
	}
	if !e.DecideCall(intern, "docs", "tools/call", "read_doc", Evidence{}).Allowed {
		t.Error("the carve-out swallowed the rest of the grant")
	}
	// The deny's own scope selector scopes it: staff hold "docs:read" but not
	// "intern", so it has nothing to say about them.
	if !e.DecideCall(staff, "docs", "tools/call", "delete_doc", Evidence{}).Allowed {
		t.Error("a deny scoped to interns refused staff")
	}
}

// TestSubjectMatchedBySubStillGatesOnScopes closes the arm the scope tests
// otherwise miss: subs and groups are alternatives, so a principal can satisfy
// the identity half by subject while holding no groups — and the scope gate
// must still apply to them, and still report its shortfall. A gate that only
// ran on the groups branch would let anyone named in `subs` past it.
func TestSubjectMatchedBySubStillGatesOnScopes(t *testing.T) {
	e := New(&config.Policy{
		DefaultDecision: "deny",
		Rules: []config.PolicyRule{{
			ID: "named-writers",
			Subjects: &config.PolicySubjects{
				Subs:   []string{"u-named"},
				Scopes: []string{"things:write"},
			},
			Allow: []config.PolicyAllow{{Server: "things", Names: []string{"*"}}},
		}},
	})

	named := func(scopes ...string) *auth.Principal {
		return &auth.Principal{Subject: "u-named", Issuer: "https://idp", Scopes: scopes}
	}

	// Named, but without the scope: refused, and told exactly what is missing.
	d := e.DecideCall(named(), "things", "tools/call", "t", Evidence{})
	if d.Allowed {
		t.Fatal("a named subject without the required scope must not be allowed")
	}
	if len(d.MissingScopes) != 1 || d.MissingScopes[0] != "things:write" {
		t.Fatalf("MissingScopes = %v, want [things:write]", d.MissingScopes)
	}

	// Named and holding it: allowed.
	if d := e.DecideCall(named("things:write"), "things", "tools/call", "t", Evidence{}); !d.Allowed {
		t.Fatal("a named subject holding the scope should be allowed")
	}

	// Not named, but holding the scope: refused on identity, and told nothing —
	// the scope was never the obstacle.
	other := &auth.Principal{Subject: "u-other", Issuer: "https://idp", Scopes: []string{"things:write"}}
	d = e.DecideCall(other, "things", "tools/call", "t", Evidence{})
	if d.Allowed {
		t.Fatal("an unnamed subject must not be allowed by holding the scope")
	}
	if len(d.MissingScopes) != 0 {
		t.Fatalf("identity denial disclosed scopes: %v", d.MissingScopes)
	}
}
