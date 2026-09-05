package gateway

import (
	"encoding/json"
	"net/http"
	"os"
	"time"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// The gateway's read-only HTTP APIs, served at /api/federation and
// /api/auth-hint when server.introspection.enabled is set.
//
// These were /console/api/state and /console/api/auth through v1.8: named
// after their only consumer, back when the console shipped from this repo
// and could never disagree with them. The console now lives in
// fold-run/fold-console and is vendored at a pinned commit, so it can skew
// against the gateway — which makes this a contract rather than an internal
// detail, and gives it a name of its own. Nothing here is console-specific:
// an operator scripting against the federation snapshot is a first-class
// caller.

// federationState is the /api/federation response. It carries no secret
// material ever: secretRef *names* are config, not values, and raw connect
// errors follow the same redaction discipline as /health.
type federationState struct {
	Version      string `json:"version"`
	AuthRequired bool   `json:"authRequired"`
	EMAEnabled   bool   `json:"emaEnabled"`
	Passthrough  bool   `json:"passthrough"`
	MCPPath      string `json:"mcpPath"`

	PolicyDefaultDecision string `json:"policyDefaultDecision"`
	PolicyRules           int    `json:"policyRules"`

	GlobalRequestsPerMinute       int `json:"globalRequestsPerMinute,omitempty"`
	PerPrincipalRequestsPerMinute int `json:"perPrincipalRequestsPerMinute,omitempty"`

	// SharedState reports whether cross-instance state (cache, rate
	// limits, breakers) is Redis-backed; the URL itself is never exposed
	// (it can embed credentials).
	SharedState bool `json:"sharedState"`

	AuditSinks     []string `json:"auditSinks"` // sink types only
	TracingEnabled bool     `json:"tracingEnabled"`

	// ViewerGroups is the read API's own allowlist
	// (server.introspection.groups); empty means any valid principal may
	// read. Group names are config shape — a caller reading this has
	// already passed the gate. The JSON name predates the config rename
	// and stays: it describes the field, not the block that feeds it.
	ViewerGroups []string `json:"viewerGroups,omitempty"`

	NamespaceSeparator string `json:"namespaceSeparator"`
	PageSize           int    `json:"pageSize"` // 0 = pagination disabled

	StaticUpstreams     int `json:"staticUpstreams"`
	DiscoveredUpstreams int `json:"discoveredUpstreams"`

	// ConsoleSource is the fold-console commit the served page was vendored
	// from, so "which console am I looking at" has an answer without
	// unpacking the binary — the question the split created, since the page
	// is now versioned separately. Reported only when a console is actually
	// served: the assets are embedded unconditionally, so on an
	// introspection-only deployment this would otherwise name a commit for
	// a page that 404s.
	ConsoleSource string `json:"consoleSource,omitempty"`

	// Tenant is the tenant the viewer resolved to, empty for a viewer in
	// none. When it has a visibility subset, everything above that counts
	// upstreams — and the list below — is that tenant's view of the
	// federation rather than the whole one, so the API cannot show a
	// customer the topology of upstreams their own traffic is refused.
	Tenant string `json:"tenant,omitempty"`
	// TenantRequestsPerMinute and TenantUpstreamCalls report the governance
	// the viewer's tenant carries, so a customer-facing view can answer
	// "what am I allowed" without an operator relaying it.
	TenantRequestsPerMinute int    `json:"tenantRequestsPerMinute,omitempty"`
	TenantUpstreamCalls     int64  `json:"tenantUpstreamCalls,omitempty"`
	TenantBudgetPeriod      string `json:"tenantBudgetPeriod,omitempty"`

	Upstreams []upstreamHealth `json:"upstreams"`
	Discovery *discoveryStatus `json:"discovery,omitempty"`
}

// discoveryStatus reports the discovery poller's last sync.
type discoveryStatus struct {
	URL         string `json:"url"`
	IntervalMs  int    `json:"intervalMs,omitempty"`
	LastOutcome string `json:"lastOutcome,omitempty"` // empty before the first sync
	LastSyncAt  string `json:"lastSyncAt,omitempty"`  // RFC 3339
}

// handleFederationState serves the federation snapshot. When gateway auth is
// on, buildHandler wraps this in authMiddleware — reaching here means the
// caller presented a valid token, so the detailed view is intentional (the
// API exists to show the federation). With auth off, the endpoint is as open
// as the rest of the gateway — which is why the introspection section is
// opt-in, and why /health, which cannot be turned off, carries none of this.
func (g *Gateway) handleFederationState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rt := g.rt()
	statuses, _, _ := g.upstreamHealthFor(r.Context(), rt)
	if g.cfg.AuthRequired() {
		// Any valid principal may read this — including tenants with zero
		// policy grants — so raw connect errors, which can name secret env
		// vars or internal hosts, are reduced to a category. The full text
		// stays in gateway logs. Topology (URLs, owners, labels) is
		// deliberately shown to authenticated viewers; that decision is on
		// record in docs/security-model.md.
		for i := range statuses {
			if statuses[i].Error != "" {
				statuses[i].Error = "unreachable — details in gateway logs"
			}
		}
	}

	st := federationState{
		Version:               version,
		AuthRequired:          g.cfg.AuthRequired(),
		EMAEnabled:            g.ema != nil,
		Passthrough:           rt.passthrough,
		MCPPath:               g.cfg.MCPPath(),
		PolicyDefaultDecision: policyDefaultDecision(rt.cfg),
		SharedState:           g.cfg.Server != nil && g.cfg.Server.RedisURL != "" || os.Getenv("REDIS_URL") != "",
		AuditSinks:            []string{},
		TracingEnabled:        g.tracer != nil,
		ViewerGroups:          g.cfg.IntrospectionGroups(),
		NamespaceSeparator:    g.sep,
		PageSize:              g.pageSize,
		Upstreams:             statuses,
	}
	if g.cfg.ConsoleEnabled() {
		st.ConsoleSource = consoleSource
	}
	if rt.cfg.Policy != nil {
		st.PolicyRules = len(rt.cfg.Policy.Rules)
	}
	if g.cfg.Server != nil && g.cfg.Server.RateLimit != nil {
		st.GlobalRequestsPerMinute = g.cfg.Server.RateLimit.RequestsPerMinute
		st.PerPrincipalRequestsPerMinute = g.cfg.Server.RateLimit.PerPrincipalPerMinute
	}
	if g.cfg.Audit != nil {
		for _, s := range g.cfg.Audit.Sinks {
			st.AuditSinks = append(st.AuditSinks, s.Type)
		}
	}

	// Annotate source and credential-strategy name per upstream. statuses
	// is index-aligned with rt.upstreams (collectUpstreamHealth preserves
	// order); discovered membership comes from the reload-guarded set.
	discovered := map[string]bool{}
	g.reloadMu.Lock()
	st.StaticUpstreams = len(g.baseCfg.Upstreams)
	st.DiscoveredUpstreams = len(g.discovered)
	for _, u := range g.discovered {
		discovered[u.ID] = true
	}
	g.reloadMu.Unlock()
	for i, u := range rt.upstreams {
		st.Upstreams[i].Source = map[bool]string{true: "discovered", false: "static"}[discovered[u.cfg.ID]]
		st.Upstreams[i].AuthStrategy = "none"
		if u.cfg.Auth != nil && u.cfg.Auth.Strategy != "" {
			st.Upstreams[i].AuthStrategy = u.cfg.Auth.Strategy
		}
	}

	// The federation view is the viewer's, not the operator's. This API has
	// no privileged access to anything else — the console's MCP test client
	// is an ordinary client against /mcp, so it already sees only what the
	// caller's tenant may reach — and a topology listing that ignored the
	// subset would be the one place a tenant could read another's upstream
	// URLs and owner metadata. Annotation happens first, above, so the
	// filter cannot skew the index alignment it depends on.
	//
	// Resolution errors are ignored: an ambiguous tenant is refused on the
	// MCP path, and the answer to "which tenant are you" for a caller who
	// matches two is to claim none.
	if tn, _ := rt.resolveTenant(principalFromRequest(r)); tn != nil {
		st.Tenant = tn.id()
		if rl := tn.cfg.RateLimit; rl != nil {
			st.TenantRequestsPerMinute = rl.RequestsPerMinute
		}
		if b := tn.cfg.Budget; b != nil {
			st.TenantUpstreamCalls, st.TenantBudgetPeriod = b.Allowance(), b.ResolvedPeriod()
		}
		if len(tn.upstreams) > 0 {
			visible := make([]upstreamHealth, 0, len(tn.upstreams))
			static, disc := 0, 0
			for i, u := range rt.upstreams {
				if !tn.sees(u.cfg.ID) {
					continue
				}
				visible = append(visible, st.Upstreams[i])
				if discovered[u.cfg.ID] {
					disc++
				} else {
					static++
				}
			}
			st.Upstreams = visible
			st.StaticUpstreams, st.DiscoveredUpstreams = static, disc
		}
	}

	if g.discovery != nil {
		ds := &discoveryStatus{
			URL:        g.discovery.cfg.URL,
			IntervalMs: g.discovery.cfg.IntervalMs,
		}
		if outcome, at := g.discovery.status(); outcome != "" {
			ds.LastOutcome = outcome
			ds.LastSyncAt = at.UTC().Format(time.RFC3339)
		}
		st.Discovery = ds
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(st)
}

// policyDefaultDecision resolves the effective default the engine actually
// applies: an absent policy section is allow-all (policy.New(nil)); a
// present one defaults to deny unless it says "allow". The API must never
// label an allow-all gateway "deny by default".
func policyDefaultDecision(cfg *config.Config) string {
	if cfg.Policy == nil {
		return "allow (no policy configured)"
	}
	if cfg.Policy.DefaultDecision == "allow" {
		return "allow"
	}
	return "deny"
}

// authHint is the /api/auth-hint response: how to sign in. It is
// deliberately unauthenticated — a browser client needs it *before* it has a
// token (the chicken-and-egg the pasted-token flow never had). Everything in
// it is public SPA configuration: a client id ships in every browser app, the
// issuer is advertised in the RFC 9728 metadata, and the resource IS that
// metadata's subject.
type authHint struct {
	AuthRequired bool       `json:"authRequired"`
	Resource     string     `json:"resource,omitempty"`
	OAuth        *oauthHint `json:"oauth,omitempty"`
}

type oauthHint struct {
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"clientId"`
	Scopes   []string `json:"scopes,omitempty"`
}

func (g *Gateway) handleAuthHint(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	hint := authHint{AuthRequired: g.cfg.AuthRequired()}
	if g.cfg.Auth != nil {
		hint.Resource = g.cfg.Auth.Resource
	}
	if iss, err := g.cfg.ConsoleOAuthIssuer(); err == nil {
		hint.OAuth = &oauthHint{
			Issuer:   iss.Issuer,
			ClientID: g.cfg.Server.Console.OAuth.ClientID,
			Scopes:   g.cfg.Server.Console.OAuth.Scopes,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(hint)
}

// introspectionViewerGate enforces server.introspection.groups: the
// authenticated principal must carry at least one allowlisted group or
// /api/federation answers 403. Runs inside authMiddleware (config validation
// guarantees auth is required whenever groups are set), so a nil principal
// here is a wiring bug, not an anonymous caller — it fails closed regardless.
// The denial is audited: this is an authorization surface, and audit is the
// single exit door for authz decisions.
func (g *Gateway) introspectionViewerGate(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, gr := range g.cfg.IntrospectionGroups() {
		allowed[gr] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFromRequest(r)
		ok := false
		if p != nil {
			for _, gr := range p.Groups {
				if allowed[gr] {
					ok = true
					break
				}
			}
		}
		if !ok {
			g.metrics.reject("introspection_viewer")
			ev := audit.Event{
				Method:   "http",
				Decision: "deny",
				Outcome:  audit.OutcomeDenied,
				Error:    "principal not in introspection viewer allowlist",
			}
			if p != nil {
				ev.Principal, ev.Issuer = p.Subject, p.Issuer
			}
			g.audit.Emit(ev)
			http.Error(w, "not in the introspection viewer allowlist", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
