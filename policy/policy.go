// Package policy implements fold's deny-by-default allowlist engine:
// first matching rule allows, otherwise the default decision applies.
// Policy governs named invocations (tools/call, prompts/get) and filters
// list results per principal — callers never see tools they cannot call.
package policy

import (
	"reflect"
	"slices"
	"strings"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
)

// Decision is the outcome of a policy check.
type Decision struct {
	Allowed bool
	RuleID  string // matching rule id, if any

	// MaxItems is the matching rule's list cap, 0 for none. It rides on the
	// decision because the caller filtering a list already has the decision in
	// hand: asking the engine a second question per item, to learn a bound
	// that never changes, would be work on the path this project measures.
	MaxItems int
}

// Engine evaluates policy rules.
type Engine struct {
	defaultAllow bool
	rules        []compiledRule
	enabled      bool

	// serverInitiatedAllow is the posture for the reverse direction — the
	// requests an upstream makes of the caller's client. It is a separate
	// default from defaultAllow, and it defaults the other way, because this
	// traffic flowed ungoverned before the check existed; see
	// config.Policy.ServerInitiatedDecision.
	serverInitiatedAllow bool
}

// compiledRule is one configured rule with its name patterns pre-split.
// Policy filters every item of every list per principal, so a pattern that
// re-parses itself on each evaluation costs one allocation per item per
// request; patterns come from config and never change for the life of an
// engine, and an engine is built once per routing snapshot.
type compiledRule struct {
	id       string
	subjects *config.PolicySubjects
	allow    []compiledAllow
	maxItems int
}

type compiledAllow struct {
	server  string
	methods []string
	names   []glob
}

// New builds an engine from configuration. A nil config yields an allow-all
// engine (policy absent = allow-all, matching fold).
func New(cfg *config.Policy) *Engine {
	if cfg == nil {
		return &Engine{defaultAllow: true}
	}
	e := &Engine{
		defaultAllow:         cfg.DefaultDecision == "allow",
		serverInitiatedAllow: cfg.ServerInitiatedDecision != "deny",
		rules:                make([]compiledRule, 0, len(cfg.Rules)),
		enabled:              true,
	}
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		cr := compiledRule{
			id:       r.ID,
			subjects: r.Subjects,
			allow:    make([]compiledAllow, 0, len(r.Allow)),
			maxItems: r.MaxItems,
		}
		for _, a := range r.Allow {
			ca := compiledAllow{server: a.Server, methods: a.Methods}
			for _, pattern := range a.Names {
				ca.names = append(ca.names, compileGlob(pattern))
			}
			cr.allow = append(cr.allow, ca)
		}
		e.rules = append(e.rules, cr)
	}
	return e
}

// Decide checks whether principal may invoke method (e.g. "tools/call") with
// the un-namespaced name on the given upstream. Protocol plumbing (ping,
// lists themselves) is not policy-gated; list filtering plus call denial is
// the enforcement pair.
func (e *Engine) Decide(p *auth.Principal, upstreamID, method, name string) Decision {
	if !e.enabled {
		return Decision{Allowed: true}
	}
	for i := range e.rules {
		r := &e.rules[i]
		if !subjectsMatch(r.subjects, p) {
			continue
		}
		for j := range r.allow {
			a := &r.allow[j]
			if a.server != "*" && a.server != upstreamID {
				continue
			}
			if len(a.methods) > 0 && !contains(a.methods, method) {
				continue
			}
			if len(a.names) > 0 && !globAny(a.names, name) {
				continue
			}
			return Decision{Allowed: true, RuleID: r.id, MaxItems: r.maxItems}
		}
	}
	return Decision{Allowed: e.defaultAllow}
}

// DecideServerInitiated checks whether upstreamID may make a server-initiated
// request — "sampling/createMessage", "elicitation/create" — of principal's
// client over a bridged session. It is the reverse of Decide: the upstream is
// asking, and the principal is who would pay for the answer.
//
// There is nothing to name in either request, so the rule matched is
// server-and-method only. A rule that grants every method on a server does
// cover these too: an empty name matches the "*" glob, which is the reading a
// blanket grant deserves.
func (e *Engine) DecideServerInitiated(p *auth.Principal, upstreamID, method string) Decision {
	// Unlike the forward path there is no configured-but-silent state to
	// worry about: an operator who has not opted in keeps yesterday's
	// behaviour, which is that this traffic flows.
	if !e.enabled || e.serverInitiatedAllow {
		return Decision{Allowed: true}
	}
	return e.Decide(p, upstreamID, method, "")
}

// Visible reports whether a listed item (a tool for "tools/call", a prompt
// for "prompts/get") should appear in list results for principal.
func (e *Engine) Visible(p *auth.Principal, upstreamID, invokeMethod, name string) bool {
	return e.Decide(p, upstreamID, invokeMethod, name).Allowed
}

// MatchSubjects reports whether a principal satisfies a subject selector.
//
// Exported so tenancy can say "these callers" the same way policy does. There
// is deliberately one definition of that: a second matcher would drift, and
// the two would eventually disagree about who a rule covers — which for
// tenancy would mean assigning someone another tenant's allowance.
func MatchSubjects(s *config.PolicySubjects, p *auth.Principal) bool {
	return subjectsMatch(s, p)
}

func subjectsMatch(s *config.PolicySubjects, p *auth.Principal) bool {
	if s == nil {
		return true
	}
	if p == nil {
		return false
	}
	// When a rule scopes to issuers, the principal's token issuer must be one
	// of them. Subjects and groups are only unique within an issuer, so a
	// rule that names them without an issuer is honored across every trusted
	// issuer — pin the issuer to keep a lower-assurance IdP from minting a
	// principal that matches a rule written for another.
	if len(s.Issuers) > 0 && !contains(s.Issuers, p.Issuer) {
		return false
	}
	// Claims gate like issuers: every required claim must match (ABAC).
	if !claimsMatch(s.Claims, p.Claims) {
		return false
	}
	// If only gates (issuers, claims) are named, passing them is sufficient.
	if len(s.Subs) == 0 && len(s.Groups) == 0 {
		return len(s.Issuers) > 0 || len(s.Claims) > 0
	}
	if len(s.Subs) > 0 && contains(s.Subs, p.Subject) {
		return true
	}
	if len(s.Groups) > 0 {
		for _, g := range s.Groups {
			if contains(p.Groups, g) {
				return true
			}
		}
	}
	return false
}

func contains(list []string, v string) bool {
	return slices.Contains(list, v)
}

// claimsMatch reports whether the verified token claims satisfy every
// required entry: the claim equals the required value, or — when the token
// claim is an array — contains it. Both sides come from JSON decoding, so
// scalars compare as string/float64/bool and DeepEqual is exact.
func claimsMatch(required map[string]any, claims map[string]any) bool {
	for key, want := range required {
		got, ok := claims[key]
		if !ok {
			return false
		}
		if arr, isArr := got.([]any); isArr {
			if !slices.ContainsFunc(arr, func(v any) bool { return claimEqual(v, want) }) {
				return false
			}
			continue
		}
		if !claimEqual(got, want) {
			return false
		}
	}
	return true
}

// claimEqual is reflect.DeepEqual specialized for the shapes claims actually
// take. Both sides come from JSON, so nearly every comparison is two strings —
// and DeepEqual reaches for reflection to decide that, once per required claim
// per rule, on a path that runs for every item of every list. The switch is
// exact rather than lenient: for two values of the same scalar type DeepEqual
// is ==, and a type mismatch is false either way, so this changes cost and not
// meaning. Anything else falls through to DeepEqual unchanged.
func claimEqual(got, want any) bool {
	switch w := want.(type) {
	case string:
		g, ok := got.(string)
		return ok && g == w
	case float64:
		g, ok := got.(float64)
		return ok && g == w
	case bool:
		g, ok := got.(bool)
		return ok && g == w
	}
	return reflect.DeepEqual(got, want)
}

// glob is a name pattern with its wildcard split already done. Patterns
// support "*" wildcards (any position); everything else matches literally.
type glob struct {
	// parts is the pattern split on "*"; a single part means a literal
	// pattern with no wildcard at all.
	parts []string
}

// compileGlob splits a pattern once, at engine-construction time.
func compileGlob(pattern string) glob {
	return glob{parts: strings.Split(pattern, "*")}
}

func (g glob) match(name string) bool {
	if len(g.parts) == 1 {
		return g.parts[0] == name
	}
	if !strings.HasPrefix(name, g.parts[0]) {
		return false
	}
	name = name[len(g.parts[0]):]
	for i := 1; i < len(g.parts)-1; i++ {
		idx := strings.Index(name, g.parts[i])
		if idx < 0 {
			return false
		}
		name = name[idx+len(g.parts[i]):]
	}
	return strings.HasSuffix(name, g.parts[len(g.parts)-1])
}

// globAny reports whether name matches any of the compiled patterns.
func globAny(globs []glob, name string) bool {
	for _, g := range globs {
		if g.match(name) {
			return true
		}
	}
	return false
}

// globMatch compiles and matches in one step. Kept for the pattern-semantics
// tests; the request path uses pre-compiled globs.
func globMatch(pattern, name string) bool {
	return compileGlob(pattern).match(name)
}
