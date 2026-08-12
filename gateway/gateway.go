// Package gateway implements the fold MCP gateway engine: one governed
// endpoint federating any number of upstream MCP servers, with namespaced
// tools, enterprise auth, policy, caching, rate limiting, and audit.
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/bounded"
	"github.com/fold-run/fold/internal/breaker"
	"github.com/fold-run/fold/internal/state"
	"github.com/fold-run/fold/policy"
)

// version is stamped at build time via
// -ldflags="-X github.com/fold-run/fold/gateway.ldflagsVersion=v...", and recovered
// from the binary's own module metadata when it was not. See resolveVersion.
var version = resolveVersion(ldflagsVersion, moduleVersion)

// ldflagsVersion is what goreleaser stamps. It is separate from `version` so
// the resolution below has an unambiguous "nothing was stamped" input; -X
// against `version` itself would be overwritten by the initialiser.
var ldflagsVersion = "dev"

// Version reports the gateway build version.
func Version() string { return version }

// moduleVersion returns the version Go recorded for the main module, or "" if
// the binary carries none.
func moduleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	return bi.Main.Version
}

// resolveVersion decides what this build should call itself.
//
// goreleaser stamps every released artifact, but the module proxy is fold's
// other distribution channel and cannot: `go run
// github.com/fold-run/fold/cmd/fold@latest` — the install the README leads
// with — produces a binary whose version reads "dev".
//
// That is not cosmetic. It reaches /api/federation, /health, the startup log
// and the MCP clientInfo, and the console gates its own surfaces on it: an
// unparseable version is treated as current, so every operator installing that
// way silently loses version-skew detection and gets unversioned docs links.
// Go already records the module version in the binary for exactly this case.
//
// The stamp still wins where there is one. "(devel)", which is what a build
// from a working tree records, is not a version and leaves "dev" alone —
// that path is a developer's, and it is honest about being one.
func resolveVersion(stamped string, module func() string) string {
	if stamped != "dev" {
		return stamped
	}
	switch v := module(); v {
	case "", "(devel)":
		return stamped
	default:
		return v
	}
}

// tokenInfoPrincipalKey carries the verified Principal through the SDK's
// TokenInfo.Extra map into per-request MCP metadata.
const tokenInfoPrincipalKey = "run.fold/principal" //nolint:gosec // metadata map key, not a credential

// Gateway is a running fold gateway. Create one with New, mount Handler
// into any http server, and Close it on shutdown.
type Gateway struct {
	cfg      *config.Config
	sep      string
	pageSize int // per-page bound for federated lists; 0 = single page

	// routes is the reloadable routing state — the upstream set, its
	// indexes, and the policy engine — swapped atomically by Reload. Every
	// request loads the pointer once so it sees one consistent world.
	routes   atomic.Pointer[routes]
	reloadMu sync.Mutex // serializes reloads; the swap itself is atomic

	// baseCfg is the operator-supplied configuration; discovered is the
	// discovery-sourced upstream set merged in alongside it. Both guarded
	// by reloadMu — Reload replaces the base keeping discovered, a
	// discovery sync replaces discovered keeping the base.
	baseCfg    *config.Config
	discovered []config.Upstream
	discovery  *discoverer // nil unless cfg.Discovery is set

	audit    *audit.Logger
	verifier *auth.Verifier
	metrics  *metricsSet
	tracer   *gwTracer // nil unless cfg.Tracing is set
	state    state.Provider
	log      *slog.Logger

	// ema is fold's embedded ID-JAG token endpoint (nil unless configured);
	// emaTokenLimit caps the unauthenticated exchange endpoint.
	ema           *auth.EMA
	emaTokenLimit state.Limiter

	globalLimit state.Limiter

	// globalBudget caps consumption across every upstream over a calendar
	// period. Construction-wired like the rest of the server section, so a
	// running gateway's allowance cannot be widened by editing config.
	globalBudget state.Budget

	// perPrincipalRPM caps each authenticated principal on its own bucket;
	// principalLimits memoizes those limiters per hashed identity.
	perPrincipalRPM int
	principalLimits *bounded.Map[state.Limiter]

	// resourceOwner remembers which upstream listed each resource URI
	// (URIs are opaque and never rewritten; ownership is remembered).
	// Per-instance: it is a routing hint, and a miss re-probes.
	resourceOwner *bounded.Map[string]

	// taskOwner remembers which upstream owns each task id (opaque, never
	// rewritten) and which principal it was minted for. Pinned at mint or on
	// first probe; refreshed on use. Unlike resourceOwner it is an
	// authorization record, so it lives in state.Provider — shared across a
	// fleet when Redis is configured.
	taskOwner *taskOwners

	// health caches and single-flights the upstream health fan-out shared by
	// /health and /api/federation.
	health healthCache

	// callCtx tracks the context of each in-flight named invocation per
	// downstream session. Server-initiated traffic from an upstream
	// (sampling, elicitation, logging, progress) is forwarded with that
	// context so the SDK routes it over the originating call's stream —
	// clients that never open a standalone SSE stream still hear it.
	callCtx sync.Map // downstream session ID → *ctxStack

	// subscribers ref-counts resource subscriptions per URI across
	// downstream sessions, so the single shared upstream subscription is
	// established on the first subscriber and dropped only on the last.
	subMu       sync.Mutex
	subscribers map[string]map[string]bool // URI → set of downstream session IDs

	server  *mcp.Server
	handler http.Handler

	stopSweeper chan struct{}
	closeOnce   sync.Once

	// metricsSrv is the separate telemetry listener (nil unless
	// server.metricsAddr is set). The gateway owns it because the config
	// document asks for it: an embedder who sets the field expects the
	// listener, and one who would rather mount it themselves uses
	// MetricsHandler and leaves the field unset.
	metricsSrv *http.Server
}

// routes is one immutable snapshot of the gateway's reloadable state. cfg is
// the config document the snapshot was built from — Reload diffs against it.
type routes struct {
	cfg         *config.Config
	upstreams   []*upstream
	byNamespace map[string]*upstream
	byID        map[string]*upstream
	passthrough bool
	policy      *policy.Engine
	// tenants is the resolved tenant set, reloadable like the rest of the
	// snapshot: tenants change when a customer signs up.
	tenants tenantSet
}

// rt returns the current routing snapshot.
func (g *Gateway) rt() *routes { return g.routes.Load() }

// Bounds on the per-instance affinity stores. Each is keyed by identifiers
// the gateway does not choose and cannot bound (upstream resource URIs,
// verified principal identities), so each needs a ceiling; see
// internal/bounded for why eviction is safe on both.
const (
	maxResourceOwners  = 50_000
	maxPrincipalLimits = 10_000
)

// Option configures a Gateway at construction.
type Option func(*Gateway)

// WithLogger sets the structured logger for operational events. If unset,
// the gateway logs to a discard handler (silent).
func WithLogger(l *slog.Logger) Option {
	return func(g *Gateway) {
		if l != nil {
			g.log = l
		}
	}
}

// New builds a gateway from a validated config.
func New(cfg *config.Config, opts ...Option) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	provider, err := buildStateProvider(cfg)
	if err != nil {
		return nil, err
	}
	g := &Gateway{
		cfg:             cfg,
		sep:             cfg.NamespaceSeparator(),
		pageSize:        cfg.PageSize(),
		audit:           nil, // built below, once metrics exist to observe it
		state:           provider,
		subscribers:     map[string]map[string]bool{},
		resourceOwner:   bounded.New[string](maxResourceOwners),
		taskOwner:       &taskOwners{store: provider.Store("task")},
		principalLimits: bounded.New[state.Limiter](maxPrincipalLimits),
		log:             slog.New(slog.DiscardHandler),
	}
	for _, opt := range opts {
		opt(g)
	}
	g.metrics = newMetricsSet(func() []*upstream {
		if rt := g.routes.Load(); rt != nil {
			return rt.upstreams
		}
		return nil
	})
	// Audit is built after metrics so its delivery outcomes have somewhere to
	// be counted: a sink that drops events is the one failure the audit trail
	// cannot record about itself.
	g.audit = audit.New(cfg.Audit,
		audit.WithObserver(g.metrics.observeAudit),
		// Delivery-worker panics count and log like every other recovered
		// panic (site "audit"), instead of masquerading as sink failures.
		audit.WithPanicHook(func(r any, stack []byte) {
			g.metrics.panicked("audit")
			g.log.Error("panic recovered", "site", "audit", "panic", r, "stack", string(stack))
		}))
	for _, err := range g.audit.StartupErrors() {
		// A sink that would not open is skipped rather than fatal — losing one
		// destination should not take the gateway down — but it is not silent.
		g.log.Error("audit sink not started", "err", err)
	}
	if cfg.Tracing != nil {
		if g.tracer, err = newGwTracer(cfg.Tracing); err != nil {
			return nil, err
		}
	}
	globalRPM := 0
	if cfg.Server != nil && cfg.Server.RateLimit != nil {
		globalRPM = cfg.Server.RateLimit.RequestsPerMinute
		g.perPrincipalRPM = cfg.Server.RateLimit.PerPrincipalPerMinute
	}
	g.globalLimit = provider.Limiter("global", globalRPM)
	redisOn := (cfg.Server != nil && cfg.Server.RedisURL != "") || os.Getenv("REDIS_URL") != ""
	var serverBudget *config.Budget
	if cfg.Server != nil {
		serverBudget = cfg.Server.Budget
	}
	// Built before the first snapshot: every upstream carries a reference to
	// it, so it must exist before buildRoutes constructs them.
	g.globalBudget = provider.Budget("global",
		state.Period(serverBudget.ResolvedPeriod()), serverBudget.Allowance())
	g.routes.Store(g.buildRoutes(cfg, nil))
	// A budget is only a budget across a fleet: without shared state each
	// instance keeps its own counter, so N instances enforce N allowances.
	// Say so rather than let an operator discover it from a bill.
	if !redisOn {
		var scopes []string
		if serverBudget.Allowance() > 0 {
			scopes = append(scopes, "server")
		}
		for i := range cfg.Upstreams {
			if cfg.Upstreams[i].Budget.Allowance() > 0 {
				scopes = append(scopes, "upstream:"+cfg.Upstreams[i].ID)
			}
		}
		for i := range cfg.Tenants {
			if cfg.Tenants[i].Budget.Allowance() > 0 {
				scopes = append(scopes, "tenant:"+cfg.Tenants[i].ID)
			}
		}
		if len(scopes) > 0 {
			g.log.Warn("budget configured without shared state — each instance enforces its own allowance",
				"scopes", scopes, "hint", "set server.redisUrl or REDIS_URL")
		}
	}
	if cfg.AuthRequired() {
		g.verifier = auth.NewVerifier(cfg.Auth, http.DefaultClient)
		if cfg.Auth.EMA != nil {
			ema, err := auth.NewEMA(cfg.Auth, http.DefaultClient, provider.Once("emajti"))
			if err != nil {
				return nil, err
			}
			g.ema = ema
			g.emaTokenLimit = provider.Limiter("oauth-token", cfg.Auth.EMA.ResolvedTokenRateLimit())
			// Tokens fold mints are presented back to fold: trust them via
			// the local key, no JWKS round-trip.
			g.verifier.TrustLocal(ema.Issuer(), ema.PublicKey())
		}
	}
	g.log.Info("gateway configured",
		"version", version,
		"upstreams", len(g.rt().upstreams),
		"passthrough", g.rt().passthrough,
		"authRequired", cfg.AuthRequired(),
		"sharedState", redisOn)

	g.server = mcp.NewServer(
		&mcp.Implementation{Name: "fold", Title: "fold gateway", Version: version},
		&mcp.ServerOptions{
			Instructions: g.instructions(),
			Capabilities: &mcp.ServerCapabilities{
				Tools:       &mcp.ToolCapabilities{ListChanged: true},
				Prompts:     &mcp.PromptCapabilities{ListChanged: true},
				Resources:   &mcp.ResourceCapabilities{ListChanged: true, Subscribe: true},
				Logging:     &mcp.LoggingCapabilities{},
				Completions: &mcp.CompletionCapabilities{},
			},
			SubscribeHandler:   g.handleSubscribe,
			UnsubscribeHandler: g.handleUnsubscribe,
		},
	)
	g.server.AddReceivingMiddleware(g.federationMiddleware)
	if err := g.registerTaskMethods(); err != nil {
		return nil, err
	}
	// Registered here rather than in newMetricsSet because it reads
	// g.server, which only now exists.
	g.metrics.registry.MustRegister(&sessionCollector{
		downstream: func() int {
			n := 0
			for range g.server.Sessions() {
				n++
			}
			return n
		},
		upstreams: func() []*upstream {
			if rt := g.routes.Load(); rt != nil {
				return rt.upstreams
			}
			return nil
		},
	})
	g.stopSweeper = make(chan struct{})
	go g.sweepLoop()
	g.handler = g.buildHandler()
	if err := g.startMetricsListener(); err != nil {
		g.Close()
		return nil, err
	}
	g.baseCfg = cfg
	if cfg.Discovery != nil {
		g.discovery = newDiscoverer(g, cfg.Discovery)
		go g.discovery.loop()
	}
	return g, nil
}

// buildRoutes assembles a routing snapshot for cfg. When prev is non-nil,
// upstreams whose configuration is unchanged are carried over live — their
// sessions, caches, and breaker state survive the reload.
func (g *Gateway) buildRoutes(cfg *config.Config, prev *routes) *routes {
	var prevTenants tenantSet
	if prev != nil {
		prevTenants = prev.tenants
	}
	rt := &routes{
		cfg:         cfg,
		byNamespace: map[string]*upstream{},
		byID:        map[string]*upstream{},
		passthrough: cfg.Passthrough(),
		policy:      policy.New(cfg.Policy),
		tenants:     buildTenants(cfg, g.state, prevTenants),
	}
	for _, ucfg := range cfg.Upstreams {
		var u *upstream
		if prev != nil {
			if ex := prev.byID[ucfg.ID]; ex != nil && reflect.DeepEqual(ex.cfg, ucfg) {
				u = ex
			}
		}
		if u == nil {
			u = g.newWiredUpstream(ucfg)
		}
		rt.upstreams = append(rt.upstreams, u)
		rt.byID[ucfg.ID] = u
		if ucfg.Namespace != "" {
			rt.byNamespace[ucfg.Namespace] = u
		}
	}
	return rt
}

// newWiredUpstream builds an upstream wired into this gateway's metrics,
// logging, and notification fan-out. The handler closures read g.server at
// notification time, so construction order does not matter.
func (g *Gateway) newWiredUpstream(ucfg config.Upstream) *upstream {
	u := newUpstream(ucfg, g.state)
	u.globalBudget = g.globalBudget
	u.metrics = g.metrics
	u.tracer = g.tracer
	u.sep = g.sep
	u.log = g.log.With("upstream", ucfg.ID)
	u.onResourceUpdated = func(ctx context.Context, params *mcp.ResourceUpdatedNotificationParams) {
		_ = g.server.ResourceUpdated(ctx, params) // best-effort fan-out
	}
	u.onListChanged = g.notifyListChanged
	u.startHealthProbes()
	return u
}

// Reload applies a new configuration without a restart. The upstream set and
// the policy engine swap atomically: in-flight requests finish against the
// snapshot they started on, new requests see the new one. Upstreams whose
// configuration is unchanged keep their live sessions, caches, and breaker
// state; removed or changed upstreams are drained (closed after their
// request timeout, so in-flight calls complete) and a changed upstream's
// resource subscriptions are re-established on its replacement. Clients
// receive list_changed notifications so they refetch. Discovery-sourced
// upstreams (see config.Discovery) survive a base reload unchanged.
//
// The auth, server, routing, audit, tracing, and discovery sections are
// wired in at construction and cannot hot-swap: changing them returns an
// error and leaves the running configuration untouched.
func (g *Gateway) Reload(cfg *config.Config) error {
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()
	return g.applyLocked(cfg, g.discovered)
}

// setDiscovered swaps the discovery-sourced upstream set, keeping the
// operator-supplied base configuration. The document is checked against the
// discovery credential allowlists first — the discovery source chooses both
// an upstream's secretRef names and its destination URL, so without a
// gateway-side gate a compromised source could point gateway-held secrets
// (or callers' tokens, via passthrough) at any endpoint.
func (g *Gateway) setDiscovered(ups []config.Upstream) error {
	if err := checkDiscoveredCredentials(g.cfg.Discovery, ups); err != nil {
		return err
	}
	clampDiscoveredUpstreams(g.cfg.Discovery, ups, g.log)
	g.reloadMu.Lock()
	defer g.reloadMu.Unlock()
	return g.applyLocked(g.baseCfg, ups)
}

// checkDiscoveredCredentials enforces discovery.allowedAuthStrategies and
// allowedSecretRefs on a discovered document. Nil allowlists (absent from
// the config) leave that dimension unrestricted; a violation rejects the
// whole document, matching the discovery path's fail-safe posture.
func checkDiscoveredCredentials(d *config.Discovery, ups []config.Upstream) error {
	if d == nil {
		return nil
	}
	for i := range ups {
		u := &ups[i]
		if u.Auth == nil {
			continue
		}
		strategy := u.Auth.Strategy
		if strategy == "" {
			strategy = "none"
		}
		if d.AllowedAuthStrategies != nil && strategy != "none" && !slices.Contains(d.AllowedAuthStrategies, strategy) {
			return fmt.Errorf("discovery: upstream %q: auth strategy %q is not in allowedAuthStrategies", u.ID, strategy)
		}
		if d.AllowedSecretRefs != nil {
			refs := []string{u.Auth.SecretRef}
			if u.Auth.ClientAuth != nil {
				refs = append(refs, u.Auth.ClientAuth.SecretRef)
			}
			for _, ref := range refs {
				if ref != "" && !slices.Contains(d.AllowedSecretRefs, ref) {
					return fmt.Errorf("discovery: upstream %q: secretRef %q is not in allowedSecretRefs", u.ID, ref)
				}
			}
		}
		// Naming a secret is half the exposure; the destination is the
		// other half. A credentialed upstream sends the gateway's secret
		// (or, under passthrough/token-exchange, the caller's token) to its
		// endpoints and its token endpoint — bound both.
		// An entry naming a secret is credentialed whatever its declared
		// strategy — otherwise a blank strategy would skip the gate.
		carriesSecret := u.Auth.SecretRef != "" || (u.Auth.ClientAuth != nil && u.Auth.ClientAuth.SecretRef != "")
		if d.AllowedCredentialHosts != nil && (strategy != "none" || carriesSecret) {
			targets := u.Endpoints()
			if u.Auth.TokenEndpoint != "" {
				targets = append(targets, u.Auth.TokenEndpoint)
			}
			for _, raw := range targets {
				parsed, err := url.Parse(raw)
				if err != nil {
					return fmt.Errorf("discovery: upstream %q: unparseable endpoint %q", u.ID, raw)
				}
				if !config.HostAllowed(d.AllowedCredentialHosts, parsed.Host) {
					return fmt.Errorf("discovery: upstream %q: credentialed endpoint host %q is not in allowedCredentialHosts",
						u.ID, parsed.Host)
				}
			}
		}
	}
	return nil
}

// clampDiscoveredUpstreams bounds per-upstream knobs a discovery source
// could otherwise weaponize. A 1 ms health-probe interval would make the
// gateway a flood source against a host the source chose; the floor is
// applied rather than rejected so a merely aggressive registration still
// federates.
func clampDiscoveredUpstreams(d *config.Discovery, ups []config.Upstream, log *slog.Logger) {
	if d == nil {
		return
	}
	floor := d.MinHealthCheckIntervalResolved()
	for i := range ups {
		hc := ups[i].HealthCheck
		if hc != nil && hc.IntervalMs < floor {
			log.Warn("discovery: clamping health-probe interval",
				"upstream", ups[i].ID, "requested", hc.IntervalMs, "floor", floor)
			clamped := *hc
			clamped.IntervalMs = floor
			ups[i].HealthCheck = &clamped
		}
	}
}

// applyLocked merges base + discovered into one document, validates it as a
// whole — so a discovered upstream colliding with a static id or namespace
// rejects the discovery set, never corrupts routing — and swaps the snapshot.
// Caller holds reloadMu.
func (g *Gateway) applyLocked(base *config.Config, discovered []config.Upstream) error {
	merged := *base
	if len(discovered) > 0 {
		merged.Upstreams = append(append([]config.Upstream{}, base.Upstreams...), discovered...)
	}
	if err := merged.Validate(); err != nil {
		return err
	}
	old := g.rt()
	for _, section := range []struct {
		name    string
		changed bool
	}{
		{"auth", !reflect.DeepEqual(merged.Auth, old.cfg.Auth)},
		{"server", !reflect.DeepEqual(merged.Server, old.cfg.Server)},
		{"routing", !reflect.DeepEqual(merged.Routing, old.cfg.Routing)},
		{"audit", !reflect.DeepEqual(merged.Audit, old.cfg.Audit)},
		{"tracing", !reflect.DeepEqual(merged.Tracing, old.cfg.Tracing)},
		{"discovery", !reflect.DeepEqual(merged.Discovery, old.cfg.Discovery)},
	} {
		if section.changed {
			return fmt.Errorf("reload: the %s section cannot change without a restart", section.name)
		}
	}

	next := g.buildRoutes(&merged, old)
	kept := map[*upstream]bool{}
	for _, u := range next.upstreams {
		kept[u] = true
	}
	var retired []*upstream
	for _, u := range old.upstreams {
		if !kept[u] {
			retired = append(retired, u)
		}
	}
	g.routes.Store(next)

	for _, r := range retired {
		// A changed upstream keeps its id: move its live resource
		// subscriptions to the replacement so subscribed clients keep
		// hearing resources/updated (best effort, off the reload path).
		if succ := next.byID[r.cfg.ID]; succ != nil {
			for _, uri := range r.subscribedURIs() {
				go func() {
					defer g.rescue("reload")
					ctx, cancel := context.WithTimeout(context.Background(), succ.connectTimeout+succ.requestTimeout)
					defer cancel()
					if err := succ.subscribe(ctx, uri); err != nil {
						g.log.Warn("reload: resubscribe failed", "upstream", succ.cfg.ID, "uri", uri, "err", err)
					}
				}()
			}
		}
		// Drain: no new requests route to a retired upstream, and closing
		// after its request timeout lets in-flight calls finish.
		go func() {
			defer g.rescue("reload")
			timer := time.NewTimer(r.requestTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-g.stopSweeper:
			}
			r.Close()
		}()
	}
	for _, kind := range []string{"tools", "prompts", "resources"} {
		g.notifyListChanged(kind)
	}
	g.baseCfg, g.discovered = base, discovered
	g.log.Info("configuration reloaded",
		"upstreams", len(next.upstreams),
		"discovered", len(discovered),
		"retired", len(retired),
		"passthrough", next.passthrough)
	return nil
}

// notifyListChanged re-emits an upstream's list_changed notification to
// every connected client. The SDK server only sends these on feature
// mutations, so we blip a sentinel feature: add + immediate remove is
// debounced into a single notification, and the sentinel is never visible
// because the federation middleware intercepts all list methods.
func (g *Gateway) notifyListChanged(kind string) {
	switch kind {
	case "tools":
		g.server.AddTool(&mcp.Tool{
			Name:        "fold-refresh-sentinel",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, fmt.Errorf("fold-refresh-sentinel is not callable")
		})
		g.server.RemoveTools("fold-refresh-sentinel")
	case "prompts":
		g.server.AddPrompt(&mcp.Prompt{Name: "fold-refresh-sentinel"},
			func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return nil, fmt.Errorf("fold-refresh-sentinel is not gettable")
			})
		g.server.RemovePrompts("fold-refresh-sentinel")
	case "resources":
		g.server.AddResource(&mcp.Resource{URI: "fold:refresh-sentinel", Name: "fold-refresh-sentinel"},
			func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return nil, fmt.Errorf("fold-refresh-sentinel is not readable")
			})
		g.server.RemoveResources("fold:refresh-sentinel")
	}
}

// sweepLoop periodically closes idle per-client upstream sessions and
// releases subscriptions whose downstream client is gone.
func (g *Gateway) sweepLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// One poisoned tick is dropped, not the loop: without the sweep,
			// idle sessions and orphaned subscriptions grow for the life of
			// the process.
			g.safely("sweep", func() {
				for _, u := range g.rt().upstreams {
					u.sweepBridged()
				}
				g.reapSubscribers()
			})
		case <-g.stopSweeper:
			return
		}
	}
}

// reapSubscribers drops subscription ref-counts held by downstream sessions
// that are no longer connected, and releases the shared upstream
// subscription once a URI has no subscriber left.
//
// resources/unsubscribe is the only path that decrements the ref-count, and
// a client is under no obligation to send one before disconnecting: without
// this, every client that subscribed and went away left the gateway holding
// an upstream subscription forever, and a client that reconnects and repeats
// the pattern grows the ref-count table without bound.
func (g *Gateway) reapSubscribers() {
	live := map[string]bool{}
	for ss := range g.server.Sessions() {
		live[ss.ID()] = true
	}

	var orphaned []string
	g.subMu.Lock()
	for uri, subs := range g.subscribers {
		for id := range subs {
			// An empty id belongs to a session the transport does not
			// identify (in-process, stdio); it cannot be matched against the
			// live set, so it is left alone rather than reaped wrongly.
			if id != "" && !live[id] {
				delete(subs, id)
			}
		}
		if len(subs) == 0 {
			delete(g.subscribers, uri)
			orphaned = append(orphaned, uri)
		}
	}
	g.subMu.Unlock()
	if len(orphaned) == 0 {
		return
	}

	rt := g.rt()
	for _, uri := range orphaned {
		// Ask the upstreams that actually hold the subscription — a list
		// fan-out to re-resolve ownership would be a heavy price for a
		// background sweep.
		for _, u := range rt.upstreams {
			if !u.isSubscribed(uri) {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), u.requestTimeout)
			if err := u.unsubscribe(ctx, uri); err != nil {
				g.log.Debug("sweep: releasing orphaned subscription failed",
					"upstream", u.cfg.ID, "uri", uri, "err", err)
			}
			cancel()
		}
	}
}

// handleSubscribe forwards resources/subscribe to the upstream owning the
// URI, gated by policy. The gateway holds one upstream subscription per URI
// shared across all downstream subscribers, ref-counted per session so one
// client's unsubscribe cannot tear down another's.
func (g *Gateway) handleSubscribe(ctx context.Context, req *mcp.SubscribeRequest) error {
	rt := g.rt()
	u, err := g.ownerForURI(ctx, rt, req.Params.URI)
	if err != nil {
		return err
	}
	if !rt.policy.Decide(auth.PrincipalFromContext(ctx), u.cfg.ID, "resources/read", req.Params.URI).Allowed {
		return &jsonrpc.Error{Code: codeDenied, Message: fmt.Sprintf("policy denied resources/subscribe %q", req.Params.URI)}
	}
	sessionID := ""
	if ss, ok := req.GetSession().(*mcp.ServerSession); ok {
		sessionID = ss.ID()
	}

	g.subMu.Lock()
	subscribers := g.subscribers[req.Params.URI]
	first := len(subscribers) == 0
	if subscribers == nil {
		subscribers = map[string]bool{}
		g.subscribers[req.Params.URI] = subscribers
	}
	subscribers[sessionID] = true
	g.subMu.Unlock()

	if !first {
		return nil // upstream subscription already held
	}
	if err := u.subscribe(ctx, req.Params.URI); err != nil {
		g.subMu.Lock()
		delete(g.subscribers[req.Params.URI], sessionID)
		if len(g.subscribers[req.Params.URI]) == 0 {
			delete(g.subscribers, req.Params.URI)
		}
		g.subMu.Unlock()
		return err
	}
	return nil
}

func (g *Gateway) handleUnsubscribe(ctx context.Context, req *mcp.UnsubscribeRequest) error {
	u, err := g.ownerForURI(ctx, g.rt(), req.Params.URI)
	if err != nil {
		return err
	}
	sessionID := ""
	if ss, ok := req.GetSession().(*mcp.ServerSession); ok {
		sessionID = ss.ID()
	}

	g.subMu.Lock()
	subscribers := g.subscribers[req.Params.URI]
	if !subscribers[sessionID] {
		// This session never subscribed to this URI — do not touch the
		// shared upstream subscription other clients depend on.
		g.subMu.Unlock()
		return nil
	}
	delete(subscribers, sessionID)
	last := len(subscribers) == 0
	if last {
		delete(g.subscribers, req.Params.URI)
	}
	g.subMu.Unlock()

	if last {
		return u.unsubscribe(ctx, req.Params.URI)
	}
	return nil
}

// ownerForURI resolves which upstream serves a resource URI, refreshing the
// resource index once when the URI is unknown.
func (g *Gateway) ownerForURI(ctx context.Context, rt *routes, uri string) (*upstream, error) {
	if rt.passthrough {
		return rt.upstreams[0], nil
	}
	// An owner outside the caller's tenant subset is no owner as far as this
	// request is concerned: the affinity index is shared across principals,
	// so without this check a URI another tenant listed would resolve here.
	tn := tenantFrom(ctx)
	if id, ok := g.resourceOwner.Load(uri); ok {
		if u := rt.byID[id]; u != nil && tn.sees(id) {
			return u, nil
		}
	}
	// Refresh the index via a list fan-out, then retry.
	ups := visibleUpstreams(ctx, rt.upstreams)
	lists, _ := fanOut(ctx, ups, func(ctx context.Context, u *upstream) ([]*mcp.Resource, error) {
		return u.listResources(ctx)
	})
	for i, u := range ups {
		for _, r := range lists[i] {
			g.resourceOwner.Store(r.URI, u.cfg.ID, 0)
		}
	}
	if id, ok := g.resourceOwner.Load(uri); ok {
		if u := rt.byID[id]; u != nil && tn.sees(id) {
			return u, nil
		}
	}
	return nil, mcp.ResourceNotFoundError(uri)
}

func (g *Gateway) instructions() string {
	if g.cfg.Passthrough() {
		return fmt.Sprintf("fold gateway in passthrough mode for upstream %q.", g.cfg.Upstreams[0].ID)
	}
	var ns []string
	for _, u := range g.cfg.Upstreams {
		ns = append(ns, u.Namespace)
	}
	return fmt.Sprintf(
		"fold gateway federating %d upstream MCP servers. Tools and prompts are namespaced as {namespace}%s{name}; namespaces: %s.",
		len(g.cfg.Upstreams), g.sep, strings.Join(ns, ", "))
}

// Handler returns the gateway's HTTP handler: the MCP endpoint plus
// /.well-known/oauth-protected-resource and /health.
func (g *Gateway) Handler() http.Handler { return g.handler }

// buildStateProvider selects shared (Redis) or in-process state.
func buildStateProvider(cfg *config.Config) (state.Provider, error) {
	url := ""
	if cfg.Server != nil {
		url = cfg.Server.RedisURL
	}
	if url == "" {
		url = os.Getenv("REDIS_URL")
	}
	if url == "" {
		return state.NewMemory(), nil
	}
	return state.NewRedis(url)
}

// metricsAddr returns the configured telemetry listener address, or "".
func (g *Gateway) metricsAddr() string {
	if g.cfg.Server == nil {
		return ""
	}
	return g.cfg.Server.MetricsAddr
}

// MetricsHandler serves the Prometheus exposition for this gateway.
//
// Handler() serves it at /metrics too, unless server.metricsAddr moved it to
// its own listener — which is the arrangement to prefer when anything but the
// gateway's own host scrapes it. A scrape names upstream ids, namespaces,
// tenant ids, and multi-endpoint upstreams' endpoint URLs; on the public mux
// that is what the Host allowlist is protecting, and the same check is what
// answers a pod-IP scrape with 403. Mount this on a listener your network
// scopes instead, and nothing has to be exempted.
func (g *Gateway) MetricsHandler() http.Handler { return g.metrics.handler() }

// startMetricsListener binds the telemetry listener when one is configured.
// It binds eagerly (rather than in a goroutine) so an address already in use
// fails New, where an operator sees it, instead of disappearing into a log
// line while the gateway serves on without metrics.
func (g *Gateway) startMetricsListener() error {
	addr := g.metricsAddr()
	if addr == "" {
		return nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("server.metricsAddr: %w", err)
	}
	mux := http.NewServeMux()
	// No host validation, no body cap, no auth: this listener is not an
	// origin a browser can be steered to, and what guards it is that you did
	// not put it on the public network. /health is here too so a scraper or
	// a probe on this network can reach liveness without the Host dance.
	mux.Handle("/metrics", g.metrics.handler())
	mux.HandleFunc("/health", g.handleHealth)
	g.metricsSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	g.log.Info("telemetry listener", "addr", ln.Addr().String(), "paths", "/metrics, /health")
	go func() {
		defer g.rescue("telemetry")
		if err := g.metricsSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			g.log.Error("telemetry listener stopped", "err", err)
		}
	}()
	return nil
}

// Close shuts down all upstream sessions, stops the telemetry listener,
// flushes buffered trace spans, and releases the state provider. Safe to call
// more than once.
func (g *Gateway) Close() {
	g.closeOnce.Do(func() {
		g.log.Info("gateway shutting down")
		if g.discovery != nil {
			close(g.discovery.stop)
		}
		close(g.stopSweeper)
		for _, u := range g.rt().upstreams {
			u.Close()
		}
		if g.metricsSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			_ = g.metricsSrv.Shutdown(ctx)
			cancel()
		}
		g.tracer.shutdown()
		g.audit.Close()
		_ = g.state.Close()
	})
}

// ctxStack tracks in-flight invocation contexts for one downstream session,
// and counts server-initiated forwards so results can wait for trailing
// notifications (see drainBridge).
type ctxStack struct {
	mu       sync.Mutex
	stack    []context.Context
	activity atomic.Int64
}

// bridgeActivity returns the forward count for a downstream session.
func (g *Gateway) bridgeActivity(key string) int64 {
	if v, ok := g.callCtx.Load(key); ok {
		return v.(*ctxStack).activity.Load()
	}
	return 0
}

func (g *Gateway) bumpBridgeActivity(key string) {
	if v, ok := g.callCtx.Load(key); ok {
		v.(*ctxStack).activity.Add(1)
	}
}

// drainBridge briefly waits out trailing server-initiated traffic after an
// invocation completes. Upstream notifications sent just before a result
// (e.g. a final log message) are handled asynchronously by the SDK client
// and can lose the race against the result; when this call saw any bridged
// traffic, wait until the stream has been quiet for a beat (bounded) so
// those notifications reach the client before the result closes its stream.
// Calls with no bridged traffic return immediately.
func (g *Gateway) drainBridge(key string, before int64) {
	if key == "" {
		return
	}
	v, ok := g.callCtx.Load(key)
	if !ok {
		return
	}
	s := v.(*ctxStack)
	last := s.activity.Load()
	if last == before {
		return
	}
	deadline := time.Now().Add(150 * time.Millisecond)
	lastChange := time.Now()
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		cur := s.activity.Load()
		if cur != last {
			last, lastChange = cur, time.Now()
			continue
		}
		if time.Since(lastChange) >= 30*time.Millisecond {
			return
		}
	}
}

// pushCallCtx records ctx as the current invocation context for the
// downstream session key, returning a func that pops it. A no-op for
// unbridged requests (empty key).
func (g *Gateway) pushCallCtx(ctx context.Context, key string) func() {
	if key == "" {
		return func() {}
	}
	v, _ := g.callCtx.LoadOrStore(key, &ctxStack{})
	s := v.(*ctxStack)
	s.mu.Lock()
	s.stack = append(s.stack, ctx)
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		for i, v := range slices.Backward(s.stack) {
			if v == ctx {
				s.stack = append(s.stack[:i], s.stack[i+1:]...)
				break
			}
		}
		empty := len(s.stack) == 0
		s.mu.Unlock()
		if empty {
			g.callCtx.Delete(key)
		}
	}
}

// invocationCtx returns the most recent live invocation context for the
// downstream session key, or fallback when none is in flight.
func (g *Gateway) invocationCtx(fallback context.Context, key string) context.Context {
	v, ok := g.callCtx.Load(key)
	if !ok {
		return fallback
	}
	s := v.(*ctxStack)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range slices.Backward(s.stack) {
		if v.Err() == nil {
			return v
		}
	}
	return fallback
}

// bridgeOptions builds the per-client upstream session options: handlers
// that forward server-initiated traffic (sampling, elicitation, logging,
// progress) back to the downstream client session, mirroring the
// capabilities that client declared. Forwarded traffic uses the in-flight
// invocation's context so it rides that call's stream.
func (g *Gateway) bridgeOptions(ss *mcp.ServerSession) *mcp.ClientOptions {
	key := ss.ID()
	// All four handlers run on SDK client goroutines with no recovery above
	// them (see rescue.go): notifications swallow a panic, request handlers
	// convert it into an error answer to the upstream.
	opts := &mcp.ClientOptions{
		// Advertise no capabilities by default; handlers below add theirs.
		Capabilities: &mcp.ClientCapabilities{},
		LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) {
			defer g.rescue("bridge")
			g.bumpBridgeActivity(key)
			_ = ss.Log(g.invocationCtx(ctx, key), req.Params) // notification; client gone is fine
		},
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			defer g.rescue("bridge")
			g.bumpBridgeActivity(key)
			_ = ss.NotifyProgress(g.invocationCtx(ctx, key), req.Params) // notification; client gone is fine
		},
	}
	var caps *mcp.ClientCapabilities
	if init := ss.InitializeParams(); init != nil {
		caps = init.Capabilities
	}
	if caps != nil && caps.Sampling != nil {
		opts.CreateMessageHandler = func(ctx context.Context, req *mcp.CreateMessageRequest) (res *mcp.CreateMessageResult, err error) {
			defer func() {
				if r := recover(); r != nil {
					notePanic(g.log, g.metrics, "bridge", r)
					res, err = nil, fmt.Errorf("internal gateway error")
				}
			}()
			g.bumpBridgeActivity(key)
			return ss.CreateMessage(g.invocationCtx(ctx, key), req.Params)
		}
	}
	if caps != nil && caps.Elicitation != nil {
		opts.ElicitationHandler = func(ctx context.Context, req *mcp.ElicitRequest) (res *mcp.ElicitResult, err error) {
			defer func() {
				if r := recover(); r != nil {
					notePanic(g.log, g.metrics, "bridge", r)
					res, err = nil, fmt.Errorf("internal gateway error")
				}
			}()
			g.bumpBridgeActivity(key)
			return ss.Elicit(g.invocationCtx(ctx, key), req.Params)
		}
	}
	return opts
}

func (g *Gateway) buildHandler() http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return g.server },
		&mcp.StreamableHTTPOptions{
			// Zero would mean sessions never expire (SDK semantics). Clients
			// routinely reconnect without DELETE — each abandoned session
			// then holds gateway state forever, and reapSubscribers reads it
			// as live, so the upstream subscriptions it pins never release
			// either. Expiry is the backstop that turns both into bounded
			// garbage; fold_downstream_sessions watches it work.
			SessionTimeout: time.Duration(g.cfg.SessionIdleTimeoutMs()) * time.Millisecond,
		},
	)

	// Pipeline (outermost first): host validation → auth → global rate
	// limit → MCP.
	var mcpChain http.Handler = streamable
	mcpChain = g.rateLimitMiddleware(mcpChain)
	if g.verifier != nil {
		mcpChain = g.authMiddleware(mcpChain)
	}

	mux := http.NewServeMux()
	mux.Handle(g.cfg.MCPPath(), mcpChain)
	mux.HandleFunc("/health", g.handleHealth)
	// With a separate telemetry listener configured, /metrics leaves the
	// public mux entirely rather than being served in both places: the point
	// of moving it is that the browser-reachable origin stops exposing the
	// federation's shape, and a second copy here would undo that.
	if g.metricsAddr() == "" {
		mux.Handle("/metrics", g.metrics.handler())
	}
	if g.cfg.IntrospectionEnabled() {
		// The federation snapshot is data, so it authenticates like /mcp. It
		// also shares /mcp's rate budgets (global + per-principal): each
		// request pings every upstream, so an unbudgeted poll loop would be
		// a load amplifier against all backends. The auth hint is
		// deliberately open — a client needs it before it has a token.
		state := g.rateLimitMiddleware(http.HandlerFunc(g.handleFederationState))
		if len(g.cfg.IntrospectionGroups()) > 0 {
			state = g.introspectionViewerGate(state)
		}
		if g.verifier != nil {
			state = g.authMiddleware(state)
		}
		mux.Handle("/api/federation", state)
		mux.HandleFunc("/api/auth-hint", g.handleAuthHint)
	}
	if g.cfg.ConsoleEnabled() {
		// The page only. Static assets are the same bytes for everyone and
		// stay open; the data they render comes from /api/federation, which
		// config validation guarantees is served whenever this is.
		mux.Handle("/console/", consoleAssetHandler(consoleCSP(g.cfg)))
		mux.Handle("/console", http.RedirectHandler("/console/", http.StatusMovedPermanently))
	}
	if g.cfg.AuthRequired() {
		mux.Handle("/.well-known/oauth-protected-resource", g.protectedResourceHandler())
		if g.ema != nil {
			mux.HandleFunc("/.well-known/jwks.json", g.ema.ServeJWKS)
			mux.Handle("/oauth/token", g.tokenRateLimit(http.HandlerFunc(g.ema.ServeToken)))
		}
	}
	return g.hostValidation(g.bodyCapMiddleware(mux))
}

// bodyCapMiddleware bounds request bodies so one large POST cannot exhaust
// gateway memory: a declared Content-Length over the cap is answered 413
// before any read, and a chunked body with no honest length is bounded by
// MaxBytesReader so it cannot slip past the cap.
func (g *Gateway) bodyCapMiddleware(next http.Handler) http.Handler {
	limit := g.cfg.MaxBodyBytes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			g.metrics.reject("body_too_large")
			g.audit.Emit(audit.Event{Method: "http", Outcome: audit.OutcomeError, Error: "request body exceeds maxBodyBytes"})
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		if r.Body != nil && r.Body != http.NoBody {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) protectedResourceMetadata() *oauthex.ProtectedResourceMetadata {
	// Only direct issuers are authorization servers a client can present
	// tokens from; an exchange issuer's ID-JAGs enter via /oauth/token, and
	// with EMA enabled fold itself is the authorization server for those.
	var issuers []string
	for _, iss := range g.cfg.Auth.Issuers {
		if iss.Mode == "exchange" {
			continue
		}
		issuers = append(issuers, iss.Issuer)
	}
	if g.ema != nil {
		issuers = append(issuers, g.ema.Issuer())
	}
	return &oauthex.ProtectedResourceMetadata{
		Resource:               g.cfg.Auth.Resource,
		AuthorizationServers:   issuers,
		BearerMethodsSupported: []string{"header"},
	}
}

// protectedResourceHandler serves the RFC 9728 metadata, announcing the EMA
// extension when the embedded token endpoint is enabled.
func (g *Gateway) protectedResourceHandler() http.Handler {
	meta := g.protectedResourceMetadata()
	if g.ema == nil {
		return sdkauth.ProtectedResourceMetadataHandler(meta)
	}
	doc, _ := json.Marshal(meta)
	extended := map[string]any{}
	_ = json.Unmarshal(doc, &extended) // doc was marshaled just above
	extended["io.modelcontextprotocol/enterprise-managed-authorization"] = map[string]string{"version": "stable"}
	body, _ := json.Marshal(extended)
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
}

// tokenRateLimit caps the EMA token endpoint: it is unauthenticated by
// design (the ID-JAG is the credential) and does a JWKS resolve, an
// asymmetric verify, and a state write per call — unbounded, that is an
// unauthenticated CPU/memory amplification vector.
func (g *Gateway) tokenRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, retry := g.emaTokenLimit.Allow(r.Context()); !ok {
			g.metrics.reject("oauth_token_rate_limited")
			g.audit.Emit(audit.Event{Method: "http", Outcome: audit.OutcomeRateLimited, Error: "oauth token endpoint rate limit"})
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "slow_down", "error_description": "too many token requests"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostValidation is DNS-rebinding protection: requests must carry an
// allowed Host (and Origin, when present).
func (g *Gateway) hostValidation(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	wildcard := false
	if g.cfg.Server != nil {
		for _, h := range g.cfg.Server.AllowedHosts {
			if h == "*" {
				wildcard = true
			}
			allowed[strings.ToLower(h)] = true
		}
	}
	// Seed the localhost defaults only when the operator did NOT pin an
	// explicit allowlist. A production deployment that sets allowedHosts to
	// its public hostname must not silently keep accepting Host: localhost.
	if len(allowed) == 0 {
		allowed["localhost"] = true
		allowed["127.0.0.1"] = true
		allowed["::1"] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Rejections flow through audit like any other terminal response —
		// audit is the single exit door, and a DNS-rebinding attempt is
		// exactly the kind of event an operator wants recorded.
		if !wildcard {
			host, ok := authorityHost(r.Host)
			if !ok || !allowed[host] {
				g.metrics.reject("forbidden_host")
				g.audit.Emit(audit.Event{Method: "http", Outcome: audit.OutcomeForbidden, Error: fmt.Sprintf("forbidden host %q", r.Host)})
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				// A present Origin must resolve to an allowed host. An origin
				// that is not a well-formed absolute URL — "null" from a
				// sandboxed document, or an authority whose port is not
				// numeric — has no allowed host and fails closed.
				if !originAllowed(allowed, origin) {
					g.metrics.reject("forbidden_origin")
					g.audit.Emit(audit.Event{Method: "http", Outcome: audit.OutcomeForbidden, Error: fmt.Sprintf("forbidden origin %q", origin)})
					http.Error(w, "forbidden origin", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// authorityHost extracts the lowercase hostname from an HTTP authority.
//
// The port is validated rather than merely discarded: net.SplitHostPort
// splits at the last colon without inspecting what follows, so
// "localhost:8080.evil.com" would otherwise read as the allowed host
// "localhost" — a rebinding bypass for any caller that can choose its own
// Host or Origin header. A malformed authority is rejected outright.
func authorityHost(hostport string) (string, bool) {
	if hostport == "" {
		return "", false
	}
	if h, port, err := net.SplitHostPort(hostport); err == nil {
		for i := range len(port) {
			if port[i] < '0' || port[i] > '9' {
				return "", false
			}
		}
		return strings.ToLower(h), true
	}
	// No port to split: a bare hostname, or a bracketed IPv6 literal.
	return strings.ToLower(strings.Trim(hostport, "[]")), true
}

// originAllowed reports whether an Origin header names an allowed host. The
// origin is parsed as a URL rather than string-split, so a malformed
// authority is rejected instead of being silently truncated to its prefix.
func originAllowed(allowed map[string]bool, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host, ok := authorityHost(u.Host)
	return ok && allowed[host]
}

// authMiddleware enforces Bearer auth via the SDK's resource-server
// middleware, wired to fold's JWKS verifier. 401s are audited.
func (g *Gateway) authMiddleware(next http.Handler) http.Handler {
	verify := func(ctx context.Context, token string, _ *http.Request) (*sdkauth.TokenInfo, error) {
		p, err := g.verifier.Verify(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", sdkauth.ErrInvalidToken, err)
		}
		return &sdkauth.TokenInfo{
			Expiration: p.Expiry,
			UserID:     p.Subject,
			Extra:      map[string]any{tokenInfoPrincipalKey: p},
		}, nil
	}
	require := sdkauth.RequireBearerToken(verify, &sdkauth.RequireBearerTokenOptions{
		ResourceMetadataURL: g.cfg.Auth.Resource + "/.well-known/oauth-protected-resource",
	})
	inner := require(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		inner.ServeHTTP(rec, r)
		if rec.status == http.StatusUnauthorized {
			g.metrics.reject("unauthenticated")
			g.audit.Emit(audit.Event{
				Method:  "http",
				Outcome: audit.OutcomeUnauthenticated,
				Error:   "invalid or missing bearer token",
			})
		}
	})
}

// rateLimitMiddleware enforces the request buckets, widest first: the global
// one, then — for authenticated callers — the caller's tenant, then the
// per-principal bucket. The tenant bucket is what "team A cannot flood team B"
// actually means: perPrincipalPerMinute gives each *person* an allowance, so
// ten agents on one team hold ten of them, while a tenant shares one. Runs
// after auth, so the principal is verified. 429 + Retry-After.
//
// The tenant is resolved here as well as at routing, rather than handed from
// one layer to the other: the two layers read their principal from different
// places (the HTTP request here, the MCP request object there), and coupling
// them through a context value would make this depend on how the SDK carries
// a request's context into a handler. Resolution is a map lookup and flat in
// the number of tenants (docs/benchmarks.md), which is what makes paying for
// it twice a non-question.
func (g *Gateway) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, retry := g.globalLimit.Allow(r.Context()); !ok {
			g.rejectRateLimited(w, retry, nil, "")
			return
		}
		p := principalFromRequest(r)
		if p != nil {
			// An ambiguous tenant is not limited here: the request is refused
			// at routing, where that refusal has one home and one message.
			if t, err := g.rt().resolveTenant(p); err == nil && t != nil && t.limiter != nil {
				if ok, retry := t.limiter.Allow(r.Context()); !ok {
					g.rejectRateLimited(w, retry, p, t.id())
					return
				}
			}
			if g.perPrincipalRPM > 0 {
				if ok, retry := g.principalLimiter(p).Allow(r.Context()); !ok {
					g.rejectRateLimited(w, retry, p, "")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// rejectRateLimited answers 429 with Retry-After and records the rejection.
func (g *Gateway) rejectRateLimited(w http.ResponseWriter, retry time.Duration, p *auth.Principal, tenantID string) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
	g.metrics.reject("rate_limited")
	evt := audit.Event{Method: "http", Outcome: audit.OutcomeRateLimited, Tenant: tenantID}
	if p != nil {
		evt.Principal, evt.Issuer = p.Subject, p.Issuer
	}
	g.audit.Emit(evt)
}

// principalFromRequest reads the verified principal the auth middleware
// stored in the request context (nil when auth is disabled).
func principalFromRequest(r *http.Request) *auth.Principal {
	ti := sdkauth.TokenInfoFromContext(r.Context())
	if ti == nil {
		return nil
	}
	p, _ := ti.Extra[tokenInfoPrincipalKey].(*auth.Principal)
	return p
}

// principalLimiter returns the per-principal limiter, memoized per identity.
// The scope hashes the client-influenced issuer+subject so raw values never
// become shared-state keys.
func (g *Gateway) principalLimiter(p *auth.Principal) state.Limiter {
	sum := sha256.Sum256([]byte(p.Issuer + "\x00" + p.Subject))
	scope := "sub:" + hex.EncodeToString(sum[:])
	if l, ok := g.principalLimits.Load(scope); ok {
		return l
	}
	// The memo is bounded (see internal/bounded): rebuilding an evicted one is
	// free against Redis, where the window lives in shared state, and costs
	// at most one fresh in-process window for a principal that has been idle
	// for a full generation.
	l, _ := g.principalLimits.LoadOrStore(scope, g.state.Limiter(scope, g.perPrincipalRPM), 0)
	return l
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush keeps SSE streaming working through the recorder.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// upstreamHealth is one upstream's health snapshot, shared by /health and
// /api/federation. Sensitive fields (URL, owner, labels, detailed
// error) are populated only when the caller is trusted: for /health that
// means auth disabled — a public deployment's /health stays minimal so an
// unauthenticated caller cannot enumerate the federation or learn secret
// env-var names from connect errors; the console serves topology to any
// authenticated principal but reduces raw connect errors to a category
// (see handleFederationState).
type upstreamHealth struct {
	ID        string            `json:"id"`
	Namespace string            `json:"namespace,omitempty"`
	URL       string            `json:"url,omitempty"`
	Owner     *config.Owner     `json:"owner,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
	Breaker   breaker.State     `json:"breaker"`
	Connected bool              `json:"connected"`
	LatencyMs int64             `json:"latencyMs,omitempty"`
	Error     string            `json:"error,omitempty"`

	// Endpoints reports the balancer's per-replica view for multi-endpoint
	// upstreams (URLs only on trusted deployments, like URL above).
	Endpoints []endpointStatus `json:"endpoints,omitempty"`

	// Source ("static" | "discovered") and AuthStrategy (the strategy
	// *name*, never its material) are /api/federation annotations, set by
	// handleFederationState after collection; /health leaves them empty so
	// its response shape is unchanged.
	Source       string `json:"source,omitempty"`
	AuthStrategy string `json:"authStrategy,omitempty"`
}

// collectUpstreamHealth pings every upstream in rt concurrently and reports
// per-upstream status, always in full. Redaction happens at the edge
// (redactUpstreamHealth) rather than here, so one collection can serve both
// a trusted and an untrusted caller. Callers pass the snapshot they are
// already answering from, so one request never mixes two worlds across a
// reload.
func (g *Gateway) collectUpstreamHealth(ctx context.Context, rt *routes) (statuses []upstreamHealth, healthy int) {
	upstreams := rt.upstreams
	statuses = make([]upstreamHealth, len(upstreams))
	var wg sync.WaitGroup
	for i, u := range upstreams {
		wg.Go(func() {
			// Own goroutine — recover here or the process dies (see rescue.go).
			defer func() {
				if r := recover(); r != nil {
					notePanic(g.log, g.metrics, "health", r)
					statuses[i] = upstreamHealth{ID: u.cfg.ID, Namespace: u.cfg.Namespace, Error: "internal error"}
				}
			}()
			h := upstreamHealth{
				ID:        u.cfg.ID,
				Namespace: u.cfg.Namespace,
				Breaker:   u.breaker.State(ctx),
				URL:       u.cfg.URL,
				Owner:     u.cfg.Owner,
				Labels:    u.cfg.Labels,
			}
			if len(u.cfg.URLs) > 0 {
				h.Endpoints = u.endpoints.snapshot(true)
			}
			start := time.Now()
			if err := u.ping(ctx); err != nil {
				h.Error = err.Error()
			} else {
				h.Connected = true
				h.LatencyMs = time.Since(start).Milliseconds()
			}
			statuses[i] = h
		})
	}
	wg.Wait()
	for _, s := range statuses {
		if s.Connected {
			healthy++
		}
	}
	return statuses, healthy
}

// redactUpstreamHealth strips the fields an untrusted caller must not see:
// URLs, owners, labels, and raw connect errors, which can name secret env
// vars or internal hosts. It rewrites the copy in place, allocating a fresh
// endpoint slice so it never reaches back into the cached collection.
func redactUpstreamHealth(statuses []upstreamHealth) {
	for i := range statuses {
		s := &statuses[i]
		s.URL, s.Owner, s.Labels, s.Error = "", nil, nil, ""
		if s.Endpoints != nil {
			bare := make([]endpointStatus, len(s.Endpoints))
			for j, ep := range s.Endpoints {
				bare[j] = endpointStatus{Healthy: ep.Healthy}
			}
			s.Endpoints = bare
		}
	}
}

// healthFanout bounds one collection; healthCacheTTL bounds how long its
// result is reused.
const (
	healthFanoutTimeout = 5 * time.Second
	healthCacheTTL      = time.Second
)

// healthCache single-flights and briefly caches the upstream health
// fan-out. Every /health and console-state call otherwise sends a real MCP
// ping to every upstream: the console API is rate limited for exactly that
// reason, but /health cannot be — it is what orchestrators and load
// balancers probe — which left an unauthenticated endpoint able to multiply
// one request into a ping against every backend, as fast as it could be
// polled. Concurrent callers now share one fan-out, and a poll loop is
// bounded to one fan-out per TTL. A snapshot swap (reload, discovery sync)
// invalidates immediately, so a probe never answers from a retired world.
type healthCache struct {
	mu       sync.Mutex
	rt       *routes
	at       time.Time
	statuses []upstreamHealth
	healthy  int
}

// upstreamHealthFor returns the (possibly cached) full health view for rt.
// The returned slice is the caller's own copy: redaction and console
// annotation mutate it freely.
func (g *Gateway) upstreamHealthFor(ctx context.Context, rt *routes) (statuses []upstreamHealth, healthy int) {
	c := &g.health
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rt != rt || time.Since(c.at) >= healthCacheTTL {
		// Detached from the calling request: the fan-out is shared, so one
		// client hanging up must not cancel the pings every other waiter is
		// about to read.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthFanoutTimeout)
		c.statuses, c.healthy = g.collectUpstreamHealth(fctx, rt)
		cancel()
		c.rt, c.at = rt, time.Now()
	}
	return append([]upstreamHealth(nil), c.statuses...), c.healthy
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	statuses, healthy := g.upstreamHealthFor(r.Context(), g.rt())
	if g.cfg.AuthRequired() {
		redactUpstreamHealth(statuses)
	}
	code := http.StatusOK
	if healthy == 0 {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    map[bool]string{true: "ok", false: "degraded"}[healthy == len(statuses)],
		"version":   version,
		"upstreams": statuses,
	})
}
