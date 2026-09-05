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
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"runtime/debug"
	"slices"
	"sort"
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

	// iconBase is the absolute origin fold mints icon URLs under, or "" when
	// it has none — server.publicUrl, else auth.resource. Construction-wired
	// with the rest of the server section, which is what lets the minted form
	// be computed once per snapshot and memoized. See icon.go.
	iconBase string
	// iconBytes caches fetched icon bodies across every upstream, so the
	// ceiling is federation-wide rather than per-upstream. Bounded and safe
	// to evict: a miss is a re-fetch.
	iconBytes    *bounded.Map[iconEntry]
	iconInflight *iconFlight
	iconClient   *http.Client

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
	// subCount is how many URIs each downstream session holds, the bound
	// that keeps subscribers finite: the outer map is keyed by a URI the
	// caller chose, and under passthrough any URI resolves to the one
	// upstream, so a single long-lived session could otherwise grow it
	// without limit. A bounded map would be the wrong tool — evicting an
	// entry would orphan the upstream subscription behind it and break the
	// ref-count — so the bound is a refusal at the cap instead.
	subCount map[string]int

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
	// hook is the external decision endpoint, nil when unconfigured. It is
	// snapshot state so a reload swaps it atomically and in-flight requests
	// finish against the configuration they started under.
	hook *decisionHook

	// listScope is the MCP cacheScope fold's list results carry. It depends
	// only on configuration, so it is resolved once per snapshot rather than
	// per list — and it lives here so a reload that adds a policy or a tenant
	// changes it in the same atomic swap that changes the thing it describes.
	listScope string
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
		iconBase:        cfg.PublicURL(),
		iconBytes:       newIconBytesCache(),
		iconInflight:    newIconFlight(),
		iconClient:      newIconClient(cfg.IconTimeout()),
		pageSize:        cfg.PageSize(),
		audit:           nil, // built below, once metrics exist to observe it
		state:           provider,
		subscribers:     map[string]map[string]bool{},
		subCount:        map[string]int{},
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
	// Degraded limiter/breaker decisions (Redis unreachable → per-instance
	// enforcement) count like degraded budget decisions do.
	if r, ok := provider.(*state.Redis); ok {
		r.OnDegraded(g.metrics.observeStateDegraded)
		if err := r.StartupError(); err != nil {
			// Same posture as a mid-run outage, said once at the moment it
			// matters: this replica is enforcing its own copy of every
			// limit, breaker, budget, and replay record until Redis answers.
			g.log.Error("redis unreachable at startup — enforcing per-instance until it answers",
				"err", err, "hint", "the fleet is not one gateway while this lasts; fold_state_degraded_total counts it")
			g.metrics.observeStateDegraded("startup")
		}
	}
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
	// The exception to that, and the only one: an operator who set
	// requireDurable has said a best-effort trail is not acceptable here, and
	// starting anyway would serve traffic against a record they have already
	// refused. config.Validate rejects the document that declares no durable
	// sink; this catches the declared one that would not open.
	if err := g.audit.DurabilityError(); err != nil {
		g.audit.Close()
		return nil, err
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
		// Trust-anchor fetches (issuer JWKS, the EMA IdP's JWKS) get the
		// bounded, redirect-refusing client — http.DefaultClient would hang
		// on a wedged IdP and follow a redirect off the configured URI.
		anchors := auth.TrustAnchorClient()
		g.verifier = auth.NewVerifier(cfg.Auth, anchors)
		g.verifier.SetJWKSObserver(g.metrics.observeJWKSFetch)
		if cfg.Auth.EMA != nil {
			ema, err := auth.NewEMA(cfg.Auth, anchors, provider.Once("emajti"))
			if err != nil {
				return nil, err
			}
			g.ema = ema
			ema.SetJWKSObserver(g.metrics.observeJWKSFetch)
			if len(cfg.Auth.EMA.AllowedAssertionTypes) == 0 && len(cfg.Auth.EMA.AllowedClientIDs) == 0 {
				// The residual gap the allowlists close is documented in
				// auth/ema.go: with neither set, any IdP-signed JWT addressed
				// to the resource is exchangeable — an ID token, an
				// application token for a client whose id happens to be the
				// resource URI. The default is kept for compatibility; the
				// exposure is said out loud, like discovery without its
				// allowlists.
				g.log.Warn("EMA token exchange accepts any IdP-signed JWT addressed to the resource — no assertion type or client id allowlist is set",
					"hint", "set auth.ema.allowedAssertionTypes (SEP-990 requires \"oauth-id-jag+jwt\") and/or auth.ema.allowedClientIds")
			}
			// The token endpoint is an authorization server surface; its
			// terminal responses flow through audit like every other refusal
			// or grant the gateway produces. A replayed ID-JAG is the event
			// the trail most needs to carry — invisible here, an attacker
			// replaying assertions under the rate limit left no record at all.
			ema.OnExchange = func(x auth.TokenExchange) {
				evt := audit.Event{
					Method:    "oauth/token",
					Principal: x.Subject,
					Issuer:    x.Issuer,
					Error:     x.Detail,
					// The exchange outcome verbatim ("minted", "replayed",
					// "invalid_grant", ...): a replay must be alertable on a
					// structured field, not a substring of the error text.
					Decision: x.Outcome,
				}
				switch x.Outcome {
				case "minted":
					evt.Outcome = audit.OutcomeOK
				case "replayed", "invalid_grant":
					// An assertion that does not verify — or verifies but was
					// already redeemed — is a failed authentication.
					evt.Outcome = audit.OutcomeUnauthenticated
				default: // invalid_request, unsupported_grant_type, server_error
					evt.Outcome = audit.OutcomeError
				}
				g.audit.Emit(evt)
			}
			g.emaTokenLimit = provider.Limiter("oauth-token", cfg.Auth.EMA.ResolvedTokenRateLimit())
			// Tokens fold mints are presented back to fold: trust them via
			// the local key, no JWKS round-trip.
			g.verifier.TrustLocal(ema.Issuer(), ema.PublicKey())
		}
	}
	if cfg.IconsEnabled() && g.iconBase == "" && !cfg.Passthrough() {
		// Not an error: this is exactly what fold did before icons existed.
		// But it is invisible from the outside — a client simply sees icons
		// that will not load — so the one thing that fixes it gets named.
		g.log.Info("upstream icons are served as published: fold has no public URL to mint them under",
			"fix", "set server.publicUrl (or auth.resource) to the absolute origin clients reach this gateway at")
	}
	g.log.Info("gateway configured",
		"version", version,
		"upstreams", len(g.rt().upstreams),
		"passthrough", g.rt().passthrough,
		"authRequired", cfg.AuthRequired(),
		"sharedState", redisOn)

	g.server = mcp.NewServer(
		g.implementation(version),
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
			// server.keepAliveMs: ping connected clients so a long-lived
			// stream keeps carrying bytes past a balancer's idle timeout.
			// Zero leaves it off, which is what fold has always done.
			//
			// The threshold is deliberately not the SDK's default of "close
			// on the first failure". A gateway's clients sit behind whatever
			// network the operator has, and one missed ping is a hiccup
			// rather than a dead peer; the specification's own guidance is
			// that *multiple* failed pings may trigger a reset. Three misses
			// is a peer that has genuinely stopped answering.
			KeepAlive:                 g.cfg.KeepAlive(),
			KeepAliveFailureThreshold: 3,
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
	g.warnCleartextCredentials(cfg.Upstreams)
	if cfg.Server != nil && slices.Contains(cfg.Server.AllowedHosts, "*") && cfg.Server.AllowedOrigins == nil {
		// The Host wildcard used to switch off the Origin check with it,
		// and MCP requires Origin validation however Host is handled. The
		// old behaviour is kept so a ["*"] deployment behind a proxy keeps
		// working; the gap is named so it is chosen rather than inherited.
		g.log.Warn("allowedHosts is \"*\" and no allowedOrigins is set — browser Origins are not validated, so DNS-rebinding protection is off",
			"hint", "set server.allowedOrigins to the origins browser clients legitimately use, or [\"*\"] to accept the exposure explicitly")
	}
	if cfg.Discovery != nil {
		// The allowlists are what stop a compromised discovery source from
		// pointing gateway-held secrets (or callers' tokens, via
		// passthrough) at endpoints of its choosing. Absent, that trust is
		// total — say so at startup, like the budget-without-Redis warning.
		if cfg.Discovery.AllowedAuthStrategies == nil &&
			cfg.Discovery.AllowedSecretRefs == nil &&
			cfg.Discovery.AllowedCredentialHosts == nil {
			g.log.Warn("discovery configured without credential allowlists — the source can attach any secretRef and point credentialed upstreams anywhere",
				"hint", "set discovery.allowedAuthStrategies / allowedSecretRefs / allowedCredentialHosts unless the source is operated by the gateway's own operators")
		}
		if cfg.Discovery.AllowedUpstreamHosts == nil {
			// A different exposure from the credential one: no secret has
			// to travel for a source to register an upstream at any host and
			// put its tool definitions in every caller's list.
			g.log.Warn("discovery configured without allowedUpstreamHosts — the source can register an upstream at any host, and its tool definitions reach every caller",
				"hint", "set discovery.allowedUpstreamHosts to the hosts upstreams are allowed to live on")
		}
		g.discovery = newDiscoverer(g, cfg.Discovery)
		go g.discovery.loop()
	}
	return g, nil
}

// warnCleartextCredentials names each upstream that will send a credential
// over plaintext http to a host that is not this machine. Validation forces
// https onto every other place a credential travels — token endpoints, the
// hook, JWKS, discovery — and deliberately not onto upstream endpoints,
// because cleartext inside a service mesh is a legitimate topology fold
// cannot distinguish from a mistake. What it can do is say which upstreams
// are in that position. Passthrough counts: it carries the caller's own
// bearer token, which is the one credential fold least owns.
func (g *Gateway) warnCleartextCredentials(ups []config.Upstream) {
	for i := range ups {
		u := &ups[i]
		if u.Auth == nil || u.Auth.Strategy == "" || u.Auth.Strategy == "none" {
			continue
		}
		for _, ep := range u.Endpoints() {
			parsed, err := url.Parse(ep)
			if err != nil || parsed.Scheme != "http" || isLoopbackHost(parsed.Hostname()) {
				continue
			}
			g.log.Warn("credentialed upstream over cleartext http — its credential travels unencrypted to this host",
				"upstream", u.ID, "strategy", u.Auth.Strategy, "host", parsed.Host,
				"hint", "use https, or accept this only inside a mesh that encrypts the hop for you")
			break
		}
	}
}

// isLoopbackHost reports whether an endpoint host is this machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
		// Rebuilt per snapshot rather than reused: the client carries the
		// configured timeout, so a reload that changes the bound must not
		// leave requests running under the old one. Connections are pooled
		// per client, so a reload costs the hook's keep-alives — rare enough
		// to prefer over a second reuse rule to keep in step.
		hook: newDecisionHook(cfg.Hook),
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
	// After the upstreams: the scope depends on whether any of them derives
	// its credential from the caller.
	rt.listScope = listCacheScope(cfg, rt.upstreams)
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
	u.iconBase = g.iconBase
	u.iconIndex = newIconIndex()
	u.iconIndexTTL = g.cfg.IconCacheTTL()
	u.log = g.log.With("upstream", ucfg.ID)
	if u.pins != nil {
		u.pins.report = g.reportDrift(u)
	}
	u.onResourceUpdated = func(ctx context.Context, params *mcp.ResourceUpdatedNotificationParams) {
		// A client subscribed with the minted URI and must be told about that
		// one; the upstream names its own. Copied rather than edited — the
		// notification belongs to the SDK's read loop.
		if minted := u.mintUIURI(params.URI); minted != params.URI {
			p := *params
			p.URI = minted
			params = &p
		}
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
		// Destination first, credentialed or not: a source that can register
		// an uncredentialed upstream at an arbitrary host can put arbitrary
		// tool definitions in front of every model behind the gateway and
		// make the gateway dial that host on every list fan-out. The
		// credential allowlists below bound where a *secret* travels; this
		// bounds where traffic travels.
		if d.AllowedUpstreamHosts != nil {
			for _, raw := range u.Endpoints() {
				parsed, err := url.Parse(raw)
				if err != nil {
					return fmt.Errorf("discovery: upstream %q: unparseable endpoint %q", u.ID, raw)
				}
				if !config.HostAllowed(d.AllowedUpstreamHosts, parsed.Host) {
					return fmt.Errorf("discovery: upstream %q: endpoint host %q is not in allowedUpstreamHosts", u.ID, parsed.Host)
				}
			}
		}
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
				if g.dropSubscriberLocked(uri, id) {
					orphaned = append(orphaned, uri)
				}
			}
		}
	}
	g.subMu.Unlock()
	if len(orphaned) == 0 {
		return
	}

	rt := g.rt()
	for _, uri := range orphaned {
		// A minted ui:// URI says which upstream holds it and under what name,
		// so the sweep releases it exactly rather than by search.
		if u, original, ok := g.uiOwner(rt, uri); ok {
			ctx, cancel := context.WithTimeout(context.Background(), u.requestTimeout)
			if err := u.unsubscribe(ctx, original); err != nil {
				g.log.Debug("sweep: releasing orphaned subscription failed",
					"upstream", u.cfg.ID, "uri", uri, "err", err)
			}
			cancel()
			continue
		}
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
	u, upstreamURI, err := g.subscribeTarget(ctx, rt, req.Params.URI)
	if err != nil {
		return err
	}
	if !rt.policy.Decide(auth.PrincipalFromContext(ctx), u.cfg.ID, "resources/read", upstreamURI).Allowed {
		return &jsonrpc.Error{Code: codeDenied, Message: fmt.Sprintf("policy denied resources/subscribe %q", req.Params.URI)}
	}
	sessionID := ""
	if ss, ok := req.GetSession().(*mcp.ServerSession); ok {
		sessionID = ss.ID()
	}

	g.subMu.Lock()
	subscribers := g.subscribers[req.Params.URI]
	if !subscribers[sessionID] && g.subCount[sessionID] >= maxSubscriptionsPerSession {
		g.subMu.Unlock()
		// Invalid params rather than a fold-minted code: the codes fold
		// mints are frozen, and "this session may not hold another
		// subscription" is a parameter the caller can change by
		// unsubscribing, which is what -32602 already means for cursors.
		return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams,
			Message: fmt.Sprintf("subscription limit reached for this session (%d); unsubscribe before subscribing again", maxSubscriptionsPerSession)}
	}
	first := len(subscribers) == 0
	if subscribers == nil {
		subscribers = map[string]bool{}
		g.subscribers[req.Params.URI] = subscribers
	}
	if !subscribers[sessionID] {
		subscribers[sessionID] = true
		g.subCount[sessionID]++
	}
	g.subMu.Unlock()

	if !first {
		return nil // upstream subscription already held
	}
	if err := u.subscribe(ctx, upstreamURI); err != nil {
		g.subMu.Lock()
		g.dropSubscriberLocked(req.Params.URI, sessionID)
		g.subMu.Unlock()
		return err
	}
	return nil
}

// maxSubscriptionsPerSession bounds the URIs one downstream session may hold.
// Generous for any real client and small enough that a session cannot make
// the subscription table a memory problem; a var so tests can lower it.
var maxSubscriptionsPerSession = 1024

// dropSubscriberLocked removes one session from one URI's subscriber set,
// keeping the per-session count in step. Caller holds subMu. Returns whether
// the URI has no subscribers left.
func (g *Gateway) dropSubscriberLocked(uri, sessionID string) (last bool) {
	subs := g.subscribers[uri]
	if !subs[sessionID] {
		return len(subs) == 0
	}
	delete(subs, sessionID)
	if g.subCount[sessionID] <= 1 {
		delete(g.subCount, sessionID)
	} else {
		g.subCount[sessionID]--
	}
	if len(subs) == 0 {
		delete(g.subscribers, uri)
		return true
	}
	return false
}

func (g *Gateway) handleUnsubscribe(ctx context.Context, req *mcp.UnsubscribeRequest) error {
	u, upstreamURI, err := g.subscribeTarget(ctx, g.rt(), req.Params.URI)
	if err != nil {
		return err
	}
	sessionID := ""
	if ss, ok := req.GetSession().(*mcp.ServerSession); ok {
		sessionID = ss.ID()
	}

	g.subMu.Lock()
	if !g.subscribers[req.Params.URI][sessionID] {
		// This session never subscribed to this URI — do not touch the
		// shared upstream subscription other clients depend on.
		g.subMu.Unlock()
		return nil
	}
	last := g.dropSubscriberLocked(req.Params.URI, sessionID)
	g.subMu.Unlock()

	if last {
		return u.unsubscribe(ctx, upstreamURI)
	}
	return nil
}

// subscribeTarget resolves a client-facing URI to the upstream that serves it
// and the URI that upstream knows it by. The ref-count table stays keyed by
// the *client-facing* URI: two upstreams may legitimately publish the same
// ui:// URI, and keying by the upstream's form would merge two unrelated
// subscriptions into one ref-count.
func (g *Gateway) subscribeTarget(ctx context.Context, rt *routes, uri string) (*upstream, string, error) {
	if u, original, ok := g.uiOwner(rt, uri); ok {
		return u, original, nil
	}
	u, err := g.ownerForURI(ctx, rt, uri)
	return u, uri, err
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
	// /health is here too so a scraper or a probe on this network can reach
	// liveness without the Host dance. Host validation is optional: when
	// server.metricsAllowedHosts is set, requests whose Host does not match
	// are refused with 403; when it is empty the listener remains unguarded,
	// relying on network scope for protection.
	var handler http.Handler = mux
	if allowed := g.cfg.Server.MetricsAllowedHosts; len(allowed) > 0 {
		handler = hostAllowedHandler(allowed, handler)
	}
	mux.Handle("/metrics", g.metrics.handler())
	mux.HandleFunc("/health", g.handleHealth)
	g.metricsSrv = &http.Server{
		Handler:           handler,
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
func (g *Gateway) bridgeOptions(ss *mcp.ServerSession, u *upstream) *mcp.ClientOptions {
	key := ss.ID()
	// All four handlers run on SDK client goroutines with no recovery above
	// them (see rescue.go): notifications swallow a panic, request handlers
	// convert it into an error answer to the upstream.
	opts := &mcp.ClientOptions{
		// Advertise no capabilities by default; handlers below add theirs.
		// The exception is what the client itself declared about rendering:
		// this session carries that client's own invocations, so the upstream
		// should hear it the way it would on a direct connection.
		Capabilities: profileOfSession(ss).uiCapabilities(&mcp.ClientCapabilities{}),
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
	// Sampling and elicitation are the two requests an upstream can make that
	// spend something of the caller's — model budget, or a human's attention
	// and trust. Both are policy-gated against the in-flight invocation's
	// principal; logging and progress above are notifications that cost the
	// caller nothing to receive and carry no answer to authorize.
	//
	// The invisibility half of the pair: setting a handler is what advertises
	// the capability to the upstream (SDK semantics), so an upstream policy
	// will never let ask is not told the client can answer, and does not ask.
	// The handler check below remains the boundary — this decision is made
	// once, at connect, and a bridged session outlives the call that opened
	// it. The staleness runs one way only: a grant added by a later reload
	// waits for a new session, while a denial added by one takes effect on the
	// next request, because the handler asks again every time.
	mayAsk := func(method string) bool {
		ictx := g.invocationCtx(context.Background(), key)
		return g.rt().policy.DecideServerInitiated(auth.PrincipalFromContext(ictx), u.cfg.ID, method).Allowed
	}
	if caps != nil && caps.Sampling != nil && mayAsk("sampling/createMessage") {
		opts.CreateMessageHandler = func(ctx context.Context, req *mcp.CreateMessageRequest) (res *mcp.CreateMessageResult, err error) {
			defer func() {
				if r := recover(); r != nil {
					notePanic(g.log, g.metrics, "bridge", r)
					res, err = nil, fmt.Errorf("internal gateway error")
				}
			}()
			g.bumpBridgeActivity(key)
			ictx := g.invocationCtx(ctx, key)
			done, err := g.authorizeServerInitiated(ictx, u, "sampling/createMessage", req.Params)
			if err != nil {
				return nil, err
			}
			res, err = ss.CreateMessage(ictx, req.Params)
			done(err)
			return res, err
		}
	}
	if caps != nil && caps.Elicitation != nil && mayAsk("elicitation/create") {
		opts.ElicitationHandler = func(ctx context.Context, req *mcp.ElicitRequest) (res *mcp.ElicitResult, err error) {
			defer func() {
				if r := recover(); r != nil {
					notePanic(g.log, g.metrics, "bridge", r)
					res, err = nil, fmt.Errorf("internal gateway error")
				}
			}()
			g.bumpBridgeActivity(key)
			ictx := g.invocationCtx(ctx, key)
			done, err := g.authorizeServerInitiated(ictx, u, "elicitation/create", req.Params)
			if err != nil {
				return nil, err
			}
			res, err = ss.Elicit(ictx, req.Params)
			done(err)
			return res, err
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
	if g.iconsServed() {
		// Deliberately not behind authMiddleware: the specification has
		// clients fetch icons without credentials, and a browser <img> tag
		// carries no bearer token, so an authenticated endpoint would serve
		// nothing to the clients this exists for. See iconproxy.go for what
		// that discloses and why it is acceptable.
		mux.HandleFunc(iconPathPrefix, g.handleIcon)
	}
	if g.cfg.AuthRequired() {
		mux.Handle("/.well-known/oauth-protected-resource", g.protectedResourceHandler())
		if g.ema != nil {
			mux.HandleFunc("/.well-known/jwks.json", g.ema.ServeJWKS)
			// RFC 8414. fold names itself in `authorization_servers` above
			// whenever EMA is on, so a client that follows that pointer must
			// find a document rather than a 404 — at the root path, and also
			// at the path-scoped form when auth.resource carries a path,
			// since that is where §3.1 sends the client.
			for _, path := range g.ema.AuthorizationServerMetadataPaths() {
				mux.HandleFunc(path, g.ema.ServeAuthorizationServerMetadata)
			}
			mux.Handle("/oauth/token", g.tokenRateLimit(http.HandlerFunc(g.ema.ServeToken)))
		}
	}
	// recoverHTTP sits inside the body cap and outside the mux: it counts
	// and audits a panic in any HTTP handler net/http would otherwise recover
	// silently (see rescue.go). The MCP path's own boundary is routeSafe,
	// below the SDK's dispatch; this catches what happens above it.
	return g.hostValidation(g.bodyCapMiddleware(g.recoverHTTP(mux)))
}

// bodyReadTimeout bounds how long a request body may take to arrive. It is a
// deadline on the connection's read side, set when a body-bearing request
// enters and cleared the moment the body reaches EOF (or the handler
// returns), so it bounds the *request* and never the response: an SSE stream
// on a POST — the ordinary MCP shape — is written long after its body was
// consumed. ReadHeaderTimeout bounds only the headers, and a WriteTimeout is
// off the table for the same streaming reason, so without this a client that
// sent its headers and then dribbled one byte a minute held a connection and
// a goroutine indefinitely.
var bodyReadTimeout = 30 * time.Second

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
			// A chunked body carries no honest Content-Length, so the cap
			// only trips mid-read inside the handler. The tripwire makes
			// that refusal exit through the same metric and audit event as
			// the declared-length branch — the single exit door does not
			// have a chunked-encoding side gate.
			//
			// The same wrapper carries the read deadline: armed here and
			// cleared when the body reaches EOF — and only then. Every
			// handler fold mounts reads a body to EOF before it answers
			// (the SDK uses io.ReadAll; the token endpoint parses a form),
			// which is what makes EOF sufficient for the streaming case. It
			// is deliberately not cleared when the handler returns: a
			// handler that answered without reading the body (a 406, a 403)
			// leaves net/http to drain what the client still owes, and that
			// drain is exactly the stalled read this deadline exists for.
			// net/http re-arms its own deadlines before the next request on
			// the connection, so a deadline left armed here does not leak
			// into the idle wait.
			rc := http.NewResponseController(w)
			_ = rc.SetReadDeadline(time.Now().Add(bodyReadTimeout)) // ErrNotSupported on HTTP/2: no deadline, as before
			tripwire := &bodyCapTripwire{
				ReadCloser: http.MaxBytesReader(w, r.Body, limit),
				clear:      func() { _ = rc.SetReadDeadline(time.Time{}) },
			}
			r.Body = tripwire
			defer func() {
				if tripwire.tripped {
					g.metrics.reject("body_too_large")
					g.audit.Emit(audit.Event{Method: "http", Outcome: audit.OutcomeError, Error: "request body exceeds maxBodyBytes"})
				}
			}()
		}
		next.ServeHTTP(w, r)
	})
}

// bodyCapTripwire notes when MaxBytesReader cut the body off, so the
// middleware can account for refusals the handler discovers mid-read, and
// lifts the body read deadline once the body has fully arrived.
type bodyCapTripwire struct {
	io.ReadCloser
	tripped bool
	clear   func() // lifts the read deadline; nil-safe, runs once
	cleared bool
}

func (b *bodyCapTripwire) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		b.tripped = true
	}
	if err == io.EOF {
		b.clearDeadline()
	}
	return n, err
}

func (b *bodyCapTripwire) clearDeadline() {
	if b.clear != nil && !b.cleared {
		b.cleared = true
		b.clear()
	}
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
		// RFC 9728 §2 recommends naming the scopes a caller needs. fold can
		// answer that from the policy it is already enforcing, and until it
		// did, a client had no way to learn the answer except by being
		// refused: authorize with whatever scopes it guessed, connect, call a
		// tool, and read the requirement out of the denial. The scopes are
		// read from the live snapshot rather than the construction-time
		// config because policy hot-reloads and this document must not go on
		// describing the rules fold started with.
		ScopesSupported: requiredScopes(g.rt()),
	}
}

// requiredScopes is every scope the current policy names, deduplicated and
// ordered so the document is stable between fetches.
//
// It is a hint, not a contract: holding all of these does not entitle a caller
// to anything, because scopes are one gate among several and a rule can also
// require an identity, an issuer, or a claim. What it does is let a client ask
// its authorization server for the right things up front, which is the
// difference between a first call that works and a first call that is denied
// with a remedy attached.
//
// Tenant selectors are deliberately excluded, though they are also scopes a
// caller needs — the first version of this published them and was wrong twice
// over. This endpoint is unauthenticated, and a tenant scope is usually a
// customer's name, so a deployment selecting tenants by scope would have
// published its customer roster to anyone who fetched the well-known path.
// That is a different class of secret from a capability name like
// "docs:write", and fold does not otherwise disclose one caller's governance
// to another — a denial refuses to name a requirement the caller failed on
// other grounds for exactly this reason. The second half: a tenant scope is an
// identity assertion rather than a permission, so an authorization server
// asked for "tenant:acme" will not mint it on request anyway. Naming it would
// leak something real in exchange for advice a client cannot act on.
func requiredScopes(rt *routes) []string {
	if rt == nil || rt.cfg == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(s *config.PolicySubjects) {
		if s == nil {
			return
		}
		for _, sc := range s.Scopes {
			if !seen[sc] {
				seen[sc] = true
				out = append(out, sc)
			}
		}
	}
	if rt.cfg.Policy != nil {
		for i := range rt.cfg.Policy.Rules {
			add(rt.cfg.Policy.Rules[i].Subjects)
		}
	}
	sort.Strings(out)
	return out
}

// protectedResourceHandler serves the RFC 9728 metadata, announcing the EMA
// extension when the embedded token endpoint is enabled.
//
// Built per request rather than once at construction, because the document now
// reports the scopes policy requires and policy hot-reloads. The rest of it —
// resource, issuers, bearer methods — is construction-wired and cannot move,
// so this costs one small marshal on an endpoint a client fetches when it
// cannot authenticate. It does no I/O and no crypto, which is what makes that
// acceptable on an unauthenticated path.
func (g *Gateway) protectedResourceHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meta := g.protectedResourceMetadata()
		if g.ema == nil {
			sdkauth.ProtectedResourceMetadataHandler(meta).ServeHTTP(w, r)
			return
		}
		// The EMA branch hand-writes the response because it has to add a key
		// the SDK's type does not carry — but everything *around* the body is
		// the SDK handler's contract and has to match it, which it did not.
		// With EMA on, the document went out with no CORS headers (so a
		// browser-based client's cross-origin discovery fetch failed the
		// check), answered a preflight with the document instead of 204, and
		// answered POST. Discovery metadata is public by definition, which is
		// why allowing any origin is safe here and in the SDK.
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		doc, err := json.Marshal(meta)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		extended := map[string]any{}
		_ = json.Unmarshal(doc, &extended) // doc was marshaled just above
		extended["io.modelcontextprotocol/enterprise-managed-authorization"] = map[string]string{"version": "stable"}
		body, err := json.Marshal(extended)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
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
			// Method matches the exchange events ServeToken emits, so one
			// SIEM query over method="oauth/token" sees this endpoint's
			// whole story — the floods included.
			g.audit.Emit(audit.Event{Method: "oauth/token", Outcome: audit.OutcomeRateLimited, Error: "oauth token endpoint rate limit"})
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "slow_down", "error_description": "too many token requests"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// hostAllowedHandler wraps next so that only hosts matching the configured
// allowlist reach it. It is the separate-listener form of hostValidation:
// metrics/health are not on the main mux, so they need their own guard.
func hostAllowedHandler(allowed []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !config.HostAllowed(allowed, r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
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
	// The Origin rule is server.allowedOrigins when set; otherwise it is
	// derived from the host allowlist, which under a Host wildcard means no
	// rule at all. That gap is announced at startup (see New) rather than
	// closed here: closing it by requiring same-origin would refuse the
	// browser clients — Inspector on localhost:6274 against a ["*"] gateway
	// — that work today, and the additive field is the non-breaking shape.
	var originRule []string
	originWildcard := false
	if g.cfg.Server != nil && g.cfg.Server.AllowedOrigins != nil {
		originRule = g.cfg.Server.AllowedOrigins
		originWildcard = slices.Contains(originRule, "*")
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
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			// A present Origin must satisfy the rule. An origin that is not
			// a well-formed absolute URL — "null" from a sandboxed document,
			// or an authority whose port is not numeric — has no host and
			// fails closed under either rule.
			var ok bool
			switch {
			case originRule != nil:
				ok = originWildcard || originAllowedBy(originRule, origin)
			case wildcard:
				ok = true // no rule to apply; announced at startup
			default:
				ok = originAllowed(allowed, origin)
			}
			if !ok {
				g.metrics.reject("forbidden_origin")
				g.audit.Emit(audit.Event{Method: "http", Outcome: audit.OutcomeForbidden, Error: fmt.Sprintf("forbidden origin %q", origin)})
				http.Error(w, "forbidden origin", http.StatusForbidden)
				return
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

// originAllowedBy is originAllowed against server.allowedOrigins patterns
// (exact hostnames or "*.suffix"), with the same parsing discipline.
func originAllowedBy(patterns []string, origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	host, ok := authorityHost(u.Host)
	return ok && config.HostAllowed(patterns, host)
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

	// Unprobed marks an upstream the gateway cannot check on its own: one
	// whose credential is derived from the caller (passthrough,
	// token-exchange) has none outside a request, so there is no honest
	// liveness check to run. Such an upstream is reported unprobed rather
	// than down, and counts as neither healthy nor unhealthy — pinging it
	// would fail on every poll, charge the upstream's budget, and trip its
	// breaker for the clients it serves correctly. Omitted when false, so a
	// federation without caller-derived credentials keeps its exact shape.
	Unprobed bool `json:"unprobed,omitempty"`

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
func (g *Gateway) collectUpstreamHealth(ctx context.Context, rt *routes) (statuses []upstreamHealth, healthy, probeable int) {
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
			// A ping carries no principal, so a caller-derived credential
			// cannot be resolved for it. Pinging anyway would fail at Apply,
			// and that failure is not free: it consumes the upstream's rate
			// budget, records a breaker failure — five polls open the circuit
			// — and, once a real session exists, charges the upstream and
			// server budgets for traffic no caller asked for.
			if u.callerDerived() {
				h.Unprobed = true
				statuses[i] = h
				return
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
		if s.Unprobed {
			continue
		}
		probeable++
		if s.Connected {
			healthy++
		}
	}
	return statuses, healthy, probeable
}

// redactUpstreamHealth strips the fields /health must not publish: URLs,
// owners, labels, and raw connect errors, which can name secret env vars or
// internal hosts. Every /health caller is untrusted — the endpoint has no
// authentication to make one otherwise. It rewrites the copy in place,
// allocating a fresh endpoint slice so it never reaches back into the cached
// collection.
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
	mu        sync.Mutex
	rt        *routes
	at        time.Time
	statuses  []upstreamHealth
	healthy   int
	probeable int
}

// upstreamHealthFor returns the (possibly cached) full health view for rt.
// The returned slice is the caller's own copy: redaction and console
// annotation mutate it freely.
func (g *Gateway) upstreamHealthFor(ctx context.Context, rt *routes) (statuses []upstreamHealth, healthy, probeable int) {
	c := &g.health
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rt != rt || time.Since(c.at) >= healthCacheTTL {
		// Detached from the calling request: the fan-out is shared, so one
		// client hanging up must not cancel the pings every other waiter is
		// about to read.
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthFanoutTimeout)
		c.statuses, c.healthy, c.probeable = g.collectUpstreamHealth(fctx, rt)
		cancel()
		c.rt, c.at = rt, time.Now()
	}
	return append([]upstreamHealth(nil), c.statuses...), c.healthy, c.probeable
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	rt := g.rt()
	statuses, healthy, probeable := g.upstreamHealthFor(r.Context(), rt)
	// Always redacted. This endpoint is unauthenticated by design — probes
	// and load balancers carry no token — and the detailed view used to be
	// gated on auth being disabled, on the reasoning that an auth-off
	// gateway is loopback-private. --host 0.0.0.0 breaks that pairing with
	// one flag, and nothing checked it, so a quick-start config exposed on
	// a network published every upstream URL, owner, and raw connect error
	// (which can name secret env vars) to anyone who could reach the port.
	// The detailed view lives on /api/federation, which authenticates.
	redactUpstreamHealth(statuses)
	// Unreachable-when-nothing-is-reachable, judged against what the gateway
	// can actually reach: a federation made entirely of caller-derived
	// upstreams has nothing to probe, and reporting it down would leave the
	// process permanently unready while it serves every authenticated caller
	// correctly.
	code := http.StatusOK
	ok := healthy == probeable
	if probeable > 0 && healthy == 0 {
		code = http.StatusServiceUnavailable
	}
	body := map[string]any{
		"version":   version,
		"upstreams": statuses,
	}
	if g.discovery != nil {
		// A discovery-driven federation with nothing in it is unready until
		// its source has answered once. Before that, an empty upstream set
		// is not a federation that happens to be empty — it is a registry
		// that was unreachable when the pod started, and a pod that passes
		// readiness in that state joins the Service and serves every client
		// an empty tools/list with no error. After a first apply the usual
		// fail-safe holds: a source that goes away leaves the last good set
		// serving, and a source that applies an empty document is believed.
		// The object is deliberately URL-free: /health is unauthenticated.
		outcome, at := g.discovery.status()
		applied := g.discovery.everApplied()
		disc := map[string]any{"applied": applied}
		if outcome != "" {
			disc["lastOutcome"] = outcome
			disc["lastSyncAt"] = at.UTC().Format(time.RFC3339)
		}
		body["discovery"] = disc
		if len(rt.upstreams) == 0 && !applied {
			code = http.StatusServiceUnavailable
			ok = false
		}
	}
	body["status"] = map[bool]string{true: "ok", false: "degraded"}[ok]
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// iconsServed reports whether the /icons endpoint is mounted: icons enabled,
// a public URL to mint under, and a federation to mint for. Passthrough mints
// nothing, so it serves nothing.
func (g *Gateway) iconsServed() bool {
	return g.cfg.IconsEnabled() && g.iconBase != "" && !g.cfg.Passthrough()
}

// implementation is what fold says about itself at initialize. The name and
// title are fold's own and not configurable — a federation of N upstreams has
// one server info, and fold already strips an upstream's out of a relayed
// result rather than letting a caller believe it reached a server it never
// connected to. What an operator may add is where to read about *this*
// deployment and what it looks like.
//
// Identity icons are not proxied: an operator sets a URL they own. A client
// enforcing the specification's same-origin rule will render only a data:
// icon or one hosted at the gateway's own origin, which is why the
// documentation recommends data: — it needs no fetch, no cache, and no
// machinery.
func (g *Gateway) implementation(version string) *mcp.Implementation {
	impl := &mcp.Implementation{Name: "fold", Title: "fold gateway", Version: version}
	id := g.cfg.Server
	if id == nil || id.Identity == nil {
		return impl
	}
	impl.WebsiteURL = id.Identity.WebsiteURL
	for _, icon := range id.Identity.Icons {
		impl.Icons = append(impl.Icons, mcp.Icon{
			Source:   icon.Src,
			MIMEType: icon.MIMEType,
			Sizes:    icon.Sizes,
			Theme:    mcp.IconTheme(icon.Theme),
		})
	}
	return impl
}
