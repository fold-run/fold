// Package policy implements fold's deny-by-default allowlist engine:
// first matching rule allows, otherwise the default decision applies.
// Policy governs named invocations (tools/call, prompts/get) and filters
// list results per principal — callers never see tools they cannot call.
package policy

import (
	"strings"

	"github.com/fold-run/fold-go/auth"
	"github.com/fold-run/fold-go/config"
)

// Decision is the outcome of a policy check.
type Decision struct {
	Allowed bool
	RuleID  string // matching rule id, if any
}

// Engine evaluates policy rules.
type Engine struct {
	defaultAllow bool
	rules        []config.PolicyRule
	enabled      bool
}

// New builds an engine from configuration. A nil config yields an allow-all
// engine (policy absent = allow-all, matching fold).
func New(cfg *config.Policy) *Engine {
	if cfg == nil {
		return &Engine{defaultAllow: true}
	}
	return &Engine{
		defaultAllow: cfg.DefaultDecision == "allow",
		rules:        cfg.Rules,
		enabled:      true,
	}
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
		if !subjectsMatch(r.Subjects, p) {
			continue
		}
		for _, a := range r.Allow {
			if a.Server != "*" && a.Server != upstreamID {
				continue
			}
			if len(a.Methods) > 0 && !contains(a.Methods, method) {
				continue
			}
			if len(a.Names) > 0 && !globAny(a.Names, name) {
				continue
			}
			return Decision{Allowed: true, RuleID: r.ID}
		}
	}
	return Decision{Allowed: e.defaultAllow}
}

// Visible reports whether a listed item (a tool for "tools/call", a prompt
// for "prompts/get") should appear in list results for principal.
func (e *Engine) Visible(p *auth.Principal, upstreamID, invokeMethod, name string) bool {
	return e.Decide(p, upstreamID, invokeMethod, name).Allowed
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
	// If only issuers are named, matching the issuer is sufficient.
	if len(s.Subs) == 0 && len(s.Groups) == 0 {
		return len(s.Issuers) > 0
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
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// globAny reports whether name matches any pattern. Patterns support "*"
// wildcards (any position); everything else matches literally.
func globAny(patterns []string, name string) bool {
	for _, pat := range patterns {
		if globMatch(pat, name) {
			return true
		}
	}
	return false
}

func globMatch(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name
	}
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(name, parts[i])
		if idx < 0 {
			return false
		}
		name = name[idx+len(parts[i]):]
	}
	return strings.HasSuffix(name, parts[len(parts)-1])
}
