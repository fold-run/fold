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

	// FailedArg is the dotted path of the argument constraint that refused an
	// otherwise-matching grant, empty when none did. It exists so a denial can
	// tell a caller *which* condition they missed without disclosing what the
	// rule wanted it to be.
	FailedArg string
}

// Engine evaluates policy rules.
type Engine struct {
	defaultAllow bool
	rules        []compiledRule
	enabled      bool

	// hasDeny is whether any rule carries a deny clause, computed once at
	// construction. Deny-wins means evaluation cannot stop at the first
	// matching allow — every rule must be checked for a matching deny first —
	// and this is what keeps that cost off the documents that use none, which
	// is every document written before deny existed.
	hasDeny bool

	// hasArgs is whether any rule constrains arguments. When false the call
	// path never unmarshals a request body, which is what it does today.
	hasArgs bool

	// hasKind is whether any rule gates on tool annotations.
	hasKind bool

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
	deny     []compiledAllow
	maxItems int
}

type compiledAllow struct {
	server  string
	methods []string
	names   []glob
	// args are the argument constraints, with each dotted path pre-split so
	// the walk does not re-split per call.
	args []compiledArg
	// kind gates on the tool's own annotations; kindAny means the clause does
	// not care.
	kind toolKind
}

// toolKind is the compiled form of a clause's toolKind field.
type toolKind int

const (
	kindAny toolKind = iota
	kindReadOnly
	kindNonDestructive
)

// admits reports whether annotations satisfy this gate.
func (k toolKind) admits(a ToolAnnotations) bool {
	switch k {
	case kindReadOnly:
		return a.ReadOnly
	case kindNonDestructive:
		// A read-only tool is non-destructive by construction; otherwise the
		// upstream must have said so explicitly, since the spec's default for
		// destructiveHint is true.
		return a.ReadOnly || !a.Destructive
	}
	return true
}

// compiledArg is one dotted path and the value the argument at it must equal.
type compiledArg struct {
	path  []string
	value any
}

// ToolAnnotations is what a toolKind rule needs to know about a tool. The
// fields carry the MCP spec's defaults for an unannotated tool, which are
// fail-safe as written and which fold does not "improve": readOnlyHint
// defaults to false and destructiveHint to true, so an unannotated tool is
// not read-only and is destructive.
type ToolAnnotations struct {
	ReadOnly    bool
	Destructive bool
}

// AnnotationSource yields the annotations of the tool being decided, or false
// when fold cannot establish them from the snapshot it holds. False denies:
// a gate that cannot see what it is gating must not wave things through, and
// failing here in the direction the rest of the engine fails makes the
// limitation visible the first time someone hits it.
//
// It is a function so that nothing is looked up for a decision that never
// reaches a rule carrying toolKind.
type AnnotationSource func() (ToolAnnotations, bool)

// Evidence is what a decision may consult beyond names — the invocation's
// arguments and the tool's annotations. Both are lazy, and either may be nil
// when the caller has none to offer.
type Evidence struct {
	Args ArgSource
	Tool AnnotationSource
}

// ArgSource yields the invocation's parsed arguments, or false when there are
// none or they could not be parsed. It is a function rather than a map so that
// nothing is unmarshalled for a call that never reaches a rule carrying
// constraints — fold forwards arguments as raw JSON and never parses them
// otherwise, which is part of why the proxy path is allocation-light.
type ArgSource func() (map[string]any, bool)

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
			allow:    compileClauses(r.Allow),
			deny:     compileClauses(r.Deny),
			maxItems: r.MaxItems,
		}
		if len(cr.deny) > 0 {
			e.hasDeny = true
		}
		for _, cs := range [][]compiledAllow{cr.allow, cr.deny} {
			for _, c := range cs {
				if len(c.args) > 0 {
					e.hasArgs = true
				}
				if c.kind != kindAny {
					e.hasKind = true
				}
			}
		}
		e.rules = append(e.rules, cr)
	}
	return e
}

// compileClauses pre-splits the name globs of one rule's allow or deny list.
func compileClauses(in []config.PolicyAllow) []compiledAllow {
	if len(in) == 0 {
		return nil
	}
	out := make([]compiledAllow, 0, len(in))
	for _, a := range in {
		ca := compiledAllow{server: a.Server, methods: a.Methods, kind: compileKind(a.ToolKind)}
		for _, pattern := range a.Names {
			ca.names = append(ca.names, compileGlob(pattern))
		}
		for path, want := range a.Args {
			ca.args = append(ca.args, compiledArg{path: strings.Split(path, "."), value: want})
		}
		// Map iteration is unordered and the failed-path is reported to the
		// caller, so sort for a stable message: the same request must not
		// blame a different constraint on each attempt.
		slices.SortFunc(ca.args, func(x, y compiledArg) int {
			return strings.Compare(strings.Join(x.path, "."), strings.Join(y.path, "."))
		})
		out = append(out, ca)
	}
	return out
}

// compileKind maps the config spelling to the compiled gate. Validation has
// already rejected anything else.
func compileKind(s string) toolKind {
	switch s {
	case "readOnly":
		return kindReadOnly
	case "nonDestructive":
		return kindNonDestructive
	}
	return kindAny
}

// argsSatisfied reports whether every constraint holds, and names the first
// that does not. The name is the path, never the expected value: the value in
// the request came from the caller, but the value in the rule is the
// operator's configuration, and a denial message is a fine place to leak a
// policy document one field at a time.
func argsSatisfied(constraints []compiledArg, args ArgSource) (bool, string) {
	if len(constraints) == 0 {
		return true, ""
	}
	if args == nil {
		return false, strings.Join(constraints[0].path, ".")
	}
	got, ok := args()
	if !ok {
		// No arguments, or arguments that are not a JSON object: a constraint
		// on a path that cannot exist is a constraint that is not satisfied.
		return false, strings.Join(constraints[0].path, ".")
	}
	for _, c := range constraints {
		v, found := lookupPath(got, c.path)
		if !found || !claimEqual(v, c.value) {
			return false, strings.Join(c.path, ".")
		}
	}
	return true, ""
}

// lookupPath walks a dotted path through nested JSON objects. No wildcards and
// no array indexing: if that turns out to be too little it can widen later,
// whereas a matcher that starts expressive cannot be narrowed.
func lookupPath(m map[string]any, path []string) (any, bool) {
	var cur any = m
	for _, seg := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// argMode says what a clause's argument constraints mean in this evaluation,
// which differs by direction and by whether there is a request to inspect.
type argMode int

const (
	// argEnforce: an actual invocation. Constraints must hold.
	argEnforce argMode = iota
	// argIgnore: an allow clause at list time. There are no arguments to
	// judge, so the clause matches on its other fields — which is what makes
	// a constrained tool visible-but-conditionally-callable.
	argIgnore
	// argSkip: a deny clause at list time. The opposite choice, for the same
	// missing information: an unevaluable deny does not hide, because
	// "no deploys to production" would otherwise remove deploy entirely and
	// apply the rule far more broadly than it was written.
	argSkip
)

// matches is the matcher for documents with no argument constraints anywhere
// — which is every document written before `args` existed, and the one the
// per-item benchmark measures. It is kept separate from the arg-aware matcher
// below rather than folded into it with a mode flag: the flag version cost
// ~4% on this path, and policy is evaluated once per item of every list it
// filters.
func matches(clauses []compiledAllow, upstreamID, method, name string) bool {
	for j := range clauses {
		a := &clauses[j]
		if a.server != "*" && a.server != upstreamID {
			continue
		}
		if len(a.methods) > 0 && !contains(a.methods, method) {
			continue
		}
		if len(a.names) > 0 && !globAny(a.names, name) {
			continue
		}
		return true
	}
	return false
}

// matchesArgs is matches for documents that constrain arguments. It also
// reports — when a clause matched on everything but its constraints — the path
// that refused it, for the denial message.
func matchesArgs(clauses []compiledAllow, upstreamID, method, name string, ev Evidence, mode argMode) (bool, string) {
	var failed string
	for j := range clauses {
		a := &clauses[j]
		if a.server != "*" && a.server != upstreamID {
			continue
		}
		if len(a.methods) > 0 && !contains(a.methods, method) {
			continue
		}
		if len(a.names) > 0 && !globAny(a.names, name) {
			continue
		}
		// Unlike arguments, annotations exist at list time as well as at call
		// time — they arrive with the tool — so a toolKind gate filters lists
		// and denies invocations alike. That is what lets one rule say "read
		// anything, write nothing" without naming a tool.
		if a.kind != kindAny && !kindAdmits(a.kind, ev.Tool) {
			continue
		}
		switch {
		case len(a.args) == 0:
			// Unconstrained: nothing to evaluate in any mode.
		case mode == argSkip:
			continue
		case mode == argEnforce:
			if ok, path := argsSatisfied(a.args, ev.Args); !ok {
				// Keep looking: another clause may grant this call outright.
				// Remember the first near-miss so the denial can say which
				// constraint stood in the way.
				if failed == "" {
					failed = path
				}
				continue
			}
		}
		return true, ""
	}
	return false, failed
}

// kindAdmits resolves the annotations only when a gate actually asks, and
// denies when they cannot be established.
func kindAdmits(k toolKind, src AnnotationSource) bool {
	if src == nil {
		return false
	}
	anno, ok := src()
	return ok && k.admits(anno)
}

// Decide checks whether principal may invoke method (e.g. "tools/call") with
// the un-namespaced name on the given upstream. Protocol plumbing (ping,
// lists themselves) is not policy-gated; list filtering plus call denial is
// the enforcement pair.
func (e *Engine) Decide(p *auth.Principal, upstreamID, method, name string) Decision {
	return e.decide(p, upstreamID, method, name, Evidence{}, argIgnore)
}

// DecideList is Decide for one item of a list, where the tool is in hand and
// its annotations can therefore be judged, but there are no arguments to
// judge.
func (e *Engine) DecideList(p *auth.Principal, upstreamID, method, name string, tool AnnotationSource) Decision {
	return e.decide(p, upstreamID, method, name, Evidence{Tool: tool}, argIgnore)
}

// DecideCall is Decide for an actual invocation: argument constraints must
// hold, and annotations must be establishable if a rule gates on them.
// Neither source is consulted unless a rule reaches for it, so a document
// using neither parses and looks up nothing.
func (e *Engine) DecideCall(p *auth.Principal, upstreamID, method, name string, ev Evidence) Decision {
	return e.decide(p, upstreamID, method, name, ev, argEnforce)
}

// NeedsArguments reports whether any rule constrains arguments. The gateway
// asks before building an ArgSource at all.
func (e *Engine) NeedsArguments() bool { return e.hasArgs }

// NeedsAnnotations reports whether any rule gates on tool annotations, so the
// gateway can skip building an AnnotationSource — and skip warming a list to
// answer it — for the documents that do not.
func (e *Engine) NeedsAnnotations() bool { return e.hasKind }

func (e *Engine) decide(p *auth.Principal, upstreamID, method, name string, ev Evidence, mode argMode) Decision {
	if !e.enabled {
		return Decision{Allowed: true}
	}
	// Deny wins, and it wins globally rather than by document order: a rule
	// whose correctness depends on where it was pasted will eventually be
	// pasted in the wrong place. So every rule is checked for a matching deny
	// before any allow is honoured — but only when the document contains one,
	// which is what keeps this free for the documents that do not.
	if !e.hasArgs && !e.hasKind {
		// The path every document took before argument constraints existed,
		// preserved exactly rather than approximately.
		if e.hasDeny {
			for i := range e.rules {
				r := &e.rules[i]
				if len(r.deny) == 0 || !subjectsMatch(r.subjects, p) {
					continue
				}
				if matches(r.deny, upstreamID, method, name) {
					// The denying rule is named so the audit event says which
					// one, which an explicit refusal owes an operator in a way
					// the default decision does not.
					return Decision{Allowed: false, RuleID: r.id}
				}
			}
		}
		for i := range e.rules {
			r := &e.rules[i]
			if !subjectsMatch(r.subjects, p) {
				continue
			}
			if matches(r.allow, upstreamID, method, name) {
				return Decision{Allowed: true, RuleID: r.id, MaxItems: r.maxItems}
			}
		}
		return Decision{Allowed: e.defaultAllow}
	}
	if e.hasDeny {
		for i := range e.rules {
			r := &e.rules[i]
			if len(r.deny) == 0 || !subjectsMatch(r.subjects, p) {
				continue
			}
			denyMode := mode
			if denyMode == argIgnore {
				denyMode = argSkip // see argSkip: an unevaluable deny must not hide
			}
			if ok, _ := matchesArgs(r.deny, upstreamID, method, name, ev, denyMode); ok {
				return Decision{Allowed: false, RuleID: r.id}
			}
		}
	}
	var blockedBy string
	for i := range e.rules {
		r := &e.rules[i]
		if !subjectsMatch(r.subjects, p) {
			continue
		}
		ok, failed := matchesArgs(r.allow, upstreamID, method, name, ev, mode)
		if ok {
			return Decision{Allowed: true, RuleID: r.id, MaxItems: r.maxItems}
		}
		if blockedBy == "" {
			blockedBy = failed
		}
	}
	return Decision{Allowed: e.defaultAllow, FailedArg: blockedBy}
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
