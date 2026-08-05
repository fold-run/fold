package gateway

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// The fold console (docs/design-console.md): a read-only observability
// dashboard plus an MCP test console, served at /console when
// server.console.enabled is set. The test console is a plain MCP client
// against the gateway's own /mcp endpoint, so console traffic runs the full
// pipeline — policy, rate limits, and audit apply exactly as they do for
// any other client, and there is no privileged path to bypass them.
//
// The assets are hand-written HTML/CSS/JS embedded at build time: no
// frameworks, no build step, no external fetches (the CSP enforces it).

//go:embed console
var consoleFS embed.FS

// consoleState is the /console/api/state response. It carries no secret
// material ever: secretRef *names* are config, not values, and raw connect
// errors follow the same redaction discipline as /healthz.
type consoleState struct {
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

	// ViewerGroups is the console's own allowlist (server.console.groups);
	// empty means any valid principal may view. Group names are config
	// shape — a caller reading this has already passed the gate.
	ViewerGroups []string `json:"viewerGroups,omitempty"`

	NamespaceSeparator string `json:"namespaceSeparator"`
	PageSize           int    `json:"pageSize"` // 0 = pagination disabled

	StaticUpstreams     int `json:"staticUpstreams"`
	DiscoveredUpstreams int `json:"discoveredUpstreams"`

	Upstreams []upstreamHealth        `json:"upstreams"`
	Discovery *consoleDiscoveryStatus `json:"discovery,omitempty"`
}

// consoleDiscoveryStatus reports the discovery poller's last sync.
type consoleDiscoveryStatus struct {
	URL         string `json:"url"`
	IntervalMs  int    `json:"intervalMs,omitempty"`
	LastOutcome string `json:"lastOutcome,omitempty"` // empty before the first sync
	LastSyncAt  string `json:"lastSyncAt,omitempty"`  // RFC 3339
}

// handleConsoleState serves the dashboard's data. When gateway auth is on,
// buildHandler wraps this in authMiddleware — reaching here means the
// caller presented a valid token, so the detailed view is intentional (the
// console exists to show the federation). With auth off, the deployment is
// trusted by the same reasoning as /healthz's detailed mode.
func (g *Gateway) handleConsoleState(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	rt := g.rt()
	statuses, _ := g.collectUpstreamHealth(ctx, rt, true)
	if g.cfg.AuthRequired() {
		// Any valid principal may view the console — including tenants with
		// zero policy grants — so raw connect errors, which can name secret
		// env vars or internal hosts, are reduced to a category. The full
		// text stays in gateway logs. Topology (URLs, owners, labels) is
		// deliberately shown to authenticated viewers; that decision is on
		// record in docs/security-model.md.
		for i := range statuses {
			if statuses[i].Error != "" {
				statuses[i].Error = "unreachable — details in gateway logs"
			}
		}
	}

	st := consoleState{
		Version:               version,
		AuthRequired:          g.cfg.AuthRequired(),
		EMAEnabled:            g.ema != nil,
		Passthrough:           rt.passthrough,
		MCPPath:               g.cfg.MCPPath(),
		PolicyDefaultDecision: policyDefaultDecision(rt.cfg),
		SharedState:           g.cfg.Server != nil && g.cfg.Server.RedisURL != "" || os.Getenv("REDIS_URL") != "",
		AuditSinks:            []string{},
		TracingEnabled:        g.tracer != nil,
		NamespaceSeparator:    g.sep,
		PageSize:              g.pageSize,
		Upstreams:             statuses,
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
	if g.cfg.Server != nil && g.cfg.Server.Console != nil {
		st.ViewerGroups = g.cfg.Server.Console.Groups
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

	if g.discovery != nil {
		ds := &consoleDiscoveryStatus{
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
// present one defaults to deny unless it says "allow". The console must
// never label an allow-all gateway "deny by default".
func policyDefaultDecision(cfg *config.Config) string {
	if cfg.Policy == nil {
		return "allow (no policy configured)"
	}
	if cfg.Policy.DefaultDecision == "allow" {
		return "allow"
	}
	return "deny"
}

// consoleViewerGate enforces server.console.groups: the authenticated
// principal must carry at least one allowlisted group or the state API
// answers 403. Runs inside authMiddleware (config validation guarantees
// auth is required whenever groups are set), so a nil principal here is a
// wiring bug, not an anonymous caller — it fails closed regardless. The
// denial is audited: the console is an authorization surface, and audit is
// the single exit door for authz decisions.
func (g *Gateway) consoleViewerGate(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, gr := range g.cfg.Server.Console.Groups {
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
			g.metrics.reject("console_viewer")
			ev := audit.Event{
				Method:   "http",
				Decision: "deny",
				Outcome:  audit.OutcomeDenied,
				Error:    "principal not in console viewer allowlist",
			}
			if p != nil {
				ev.Principal, ev.Issuer = p.Subject, p.Issuer
			}
			g.audit.Emit(ev)
			http.Error(w, "not in the console viewer allowlist", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// consoleAssetHandler serves the embedded console page under /console/.
// Assets are static and identical for every caller — no data, so no auth —
// and the CSP pins every fetch the page makes to this origin.
func consoleAssetHandler() http.Handler {
	sub, err := fs.Sub(consoleFS, "console")
	if err != nil {
		// The embed is part of the binary; a missing subdirectory is a
		// build defect, not a runtime condition.
		panic(err)
	}
	files := http.StripPrefix("/console/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		files.ServeHTTP(w, r)
	})
}
