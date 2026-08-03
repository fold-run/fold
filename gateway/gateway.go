// Package gateway implements the fold-go MCP gateway engine: one governed
// endpoint federating any number of upstream MCP servers, with namespaced
// tools, enterprise auth, policy, caching, rate limiting, and audit.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/fold-run/fold-go/audit"
	"github.com/fold-run/fold-go/auth"
	"github.com/fold-run/fold-go/config"
	"github.com/fold-run/fold-go/internal/breaker"
	"github.com/fold-run/fold-go/internal/state"
	"github.com/fold-run/fold-go/policy"
)

// version is stamped at build time via
// -ldflags="-X github.com/fold-run/fold-go/gateway.version=v...".
var version = "dev"

// Version reports the gateway build version.
func Version() string { return version }

// tokenInfoPrincipalKey carries the verified Principal through the SDK's
// TokenInfo.Extra map into per-request MCP metadata.
const tokenInfoPrincipalKey = "run.fold/principal"

// Gateway is a running fold gateway. Create one with New, mount Handler
// into any http server, and Close it on shutdown.
type Gateway struct {
	cfg         *config.Config
	sep         string
	passthrough bool

	upstreams   []*upstream
	byNamespace map[string]*upstream
	byID        map[string]*upstream

	policy   *policy.Engine
	audit    *audit.Logger
	verifier *auth.Verifier
	metrics  *metricsSet
	state    state.Provider

	globalLimit state.Limiter

	// resourceOwner remembers which upstream listed each resource URI
	// (URIs are opaque and never rewritten; ownership is remembered).
	resourceOwner sync.Map

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
}

// New builds a gateway from a validated config.
func New(cfg *config.Config) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	provider, err := buildStateProvider(cfg)
	if err != nil {
		return nil, err
	}
	g := &Gateway{
		cfg:         cfg,
		sep:         cfg.NamespaceSeparator(),
		passthrough: cfg.Passthrough(),
		byNamespace: map[string]*upstream{},
		byID:        map[string]*upstream{},
		policy:      policy.New(cfg.Policy),
		audit:       audit.New(cfg.Audit),
		state:       provider,
		subscribers: map[string]map[string]bool{},
	}
	for _, ucfg := range cfg.Upstreams {
		u := newUpstream(ucfg, provider)
		g.upstreams = append(g.upstreams, u)
		g.byID[ucfg.ID] = u
		if ucfg.Namespace != "" {
			g.byNamespace[ucfg.Namespace] = u
		}
	}
	globalRPM := 0
	if cfg.Server != nil && cfg.Server.RateLimit != nil {
		globalRPM = cfg.Server.RateLimit.RequestsPerMinute
	}
	g.globalLimit = provider.Limiter("global", globalRPM)
	if cfg.AuthRequired() {
		g.verifier = auth.NewVerifier(cfg.Auth, http.DefaultClient)
	}
	g.metrics = newMetricsSet(g.upstreams)
	for _, u := range g.upstreams {
		u.metrics = g.metrics
	}

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
	for _, u := range g.upstreams {
		u.onResourceUpdated = func(ctx context.Context, params *mcp.ResourceUpdatedNotificationParams) {
			g.server.ResourceUpdated(ctx, params)
		}
		u.onListChanged = g.notifyListChanged
	}
	g.stopSweeper = make(chan struct{})
	go g.sweepLoop()
	g.handler = g.buildHandler()
	return g, nil
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

// sweepLoop periodically closes idle per-client upstream sessions.
func (g *Gateway) sweepLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			for _, u := range g.upstreams {
				u.sweepBridged()
			}
		case <-g.stopSweeper:
			return
		}
	}
}

// handleSubscribe forwards resources/subscribe to the upstream owning the
// URI, gated by policy. The gateway holds one upstream subscription per URI
// shared across all downstream subscribers, ref-counted per session so one
// client's unsubscribe cannot tear down another's.
func (g *Gateway) handleSubscribe(ctx context.Context, req *mcp.SubscribeRequest) error {
	u, err := g.ownerForURI(ctx, req.Params.URI)
	if err != nil {
		return err
	}
	if !g.policy.Decide(auth.PrincipalFromContext(ctx), u.cfg.ID, "resources/read", req.Params.URI).Allowed {
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
	u, err := g.ownerForURI(ctx, req.Params.URI)
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
func (g *Gateway) ownerForURI(ctx context.Context, uri string) (*upstream, error) {
	if g.passthrough {
		return g.upstreams[0], nil
	}
	if id, ok := g.resourceOwner.Load(uri); ok {
		if u := g.byID[id.(string)]; u != nil {
			return u, nil
		}
	}
	// Refresh the index via a list fan-out, then retry.
	lists, _ := fanOut(ctx, g.upstreams, func(ctx context.Context, u *upstream) ([]*mcp.Resource, error) {
		return u.listResources(ctx)
	})
	for i, u := range g.upstreams {
		for _, r := range lists[i] {
			g.resourceOwner.Store(r.URI, u.cfg.ID)
		}
	}
	if id, ok := g.resourceOwner.Load(uri); ok {
		if u := g.byID[id.(string)]; u != nil {
			return u, nil
		}
	}
	return nil, mcp.ResourceNotFoundError(uri)
}

func (g *Gateway) instructions() string {
	if g.passthrough {
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
// /.well-known/oauth-protected-resource and /healthz.
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

// Close shuts down all upstream sessions.
func (g *Gateway) Close() {
	close(g.stopSweeper)
	for _, u := range g.upstreams {
		u.Close()
	}
	g.state.Close()
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
func (g *Gateway) pushCallCtx(key string, ctx context.Context) func() {
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
		for i := len(s.stack) - 1; i >= 0; i-- {
			if s.stack[i] == ctx {
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
func (g *Gateway) invocationCtx(key string, fallback context.Context) context.Context {
	v, ok := g.callCtx.Load(key)
	if !ok {
		return fallback
	}
	s := v.(*ctxStack)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.stack) - 1; i >= 0; i-- {
		if s.stack[i].Err() == nil {
			return s.stack[i]
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
	opts := &mcp.ClientOptions{
		// Advertise no capabilities by default; handlers below add theirs.
		Capabilities: &mcp.ClientCapabilities{},
		LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) {
			g.bumpBridgeActivity(key)
			ss.Log(g.invocationCtx(key, ctx), req.Params)
		},
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			g.bumpBridgeActivity(key)
			ss.NotifyProgress(g.invocationCtx(key, ctx), req.Params)
		},
	}
	var caps *mcp.ClientCapabilities
	if init := ss.InitializeParams(); init != nil {
		caps = init.Capabilities
	}
	if caps != nil && caps.Sampling != nil {
		opts.CreateMessageHandler = func(ctx context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			g.bumpBridgeActivity(key)
			return ss.CreateMessage(g.invocationCtx(key, ctx), req.Params)
		}
	}
	if caps != nil && caps.Elicitation != nil {
		opts.ElicitationHandler = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			g.bumpBridgeActivity(key)
			return ss.Elicit(g.invocationCtx(key, ctx), req.Params)
		}
	}
	return opts
}

func (g *Gateway) buildHandler() http.Handler {
	streamable := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return g.server },
		&mcp.StreamableHTTPOptions{},
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
	mux.HandleFunc("/healthz", g.handleHealth)
	mux.Handle("/metrics", g.metrics.handler())
	if g.cfg.AuthRequired() {
		mux.Handle("/.well-known/oauth-protected-resource", sdkauth.ProtectedResourceMetadataHandler(g.protectedResourceMetadata()))
	}
	return g.hostValidation(mux)
}

func (g *Gateway) protectedResourceMetadata() *oauthex.ProtectedResourceMetadata {
	var issuers []string
	for _, iss := range g.cfg.Auth.Issuers {
		issuers = append(issuers, iss.Issuer)
	}
	return &oauthex.ProtectedResourceMetadata{
		Resource:               g.cfg.Auth.Resource,
		AuthorizationServers:   issuers,
		BearerMethodsSupported: []string{"header"},
	}
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
	hostname := func(hostport string) string {
		if h, _, err := net.SplitHostPort(hostport); err == nil {
			return strings.ToLower(h)
		}
		return strings.ToLower(strings.Trim(hostport, "[]"))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !wildcard {
			if !allowed[hostname(r.Host)] {
				g.metrics.reject("forbidden_host")
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				// A present Origin must resolve to an allowed host. A
				// schemeless or opaque origin (e.g. "null" from a sandboxed
				// document) has no allowed host and fails closed.
				host := ""
				if i := strings.Index(origin, "://"); i >= 0 {
					host = hostname(origin[i+3:])
				}
				if !allowed[host] {
					g.metrics.reject("forbidden_origin")
					http.Error(w, "forbidden origin", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
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

// rateLimitMiddleware enforces the global request budget: 429 + Retry-After.
func (g *Gateway) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, retry := g.globalLimit.Allow(r.Context()); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			g.metrics.reject("rate_limited")
			g.audit.Emit(audit.Event{Method: "http", Outcome: audit.OutcomeRateLimited})
			return
		}
		next.ServeHTTP(w, r)
	})
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

// upstreamHealth is one upstream's health snapshot. Sensitive fields (URL,
// owner, labels, detailed error) are populated only for trusted deployments
// (auth disabled); a public deployment's /healthz stays minimal so an
// unauthenticated caller cannot enumerate the federation or learn secret
// env-var names from connect errors.
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
}

func (g *Gateway) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	detailed := !g.cfg.AuthRequired()
	statuses := make([]upstreamHealth, len(g.upstreams))
	var wg sync.WaitGroup
	for i, u := range g.upstreams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := upstreamHealth{
				ID:        u.cfg.ID,
				Namespace: u.cfg.Namespace,
				Breaker:   u.breaker.State(ctx),
			}
			if detailed {
				h.URL = u.cfg.URL
				h.Owner = u.cfg.Owner
				h.Labels = u.cfg.Labels
			}
			start := time.Now()
			if err := u.ping(ctx); err != nil {
				// Never echo the raw error to callers — it can name secret
				// env vars or internal hosts. Detailed deployments get a
				// short category; public ones get nothing.
				if detailed {
					h.Error = err.Error()
				}
			} else {
				h.Connected = true
				h.LatencyMs = time.Since(start).Milliseconds()
			}
			statuses[i] = h
		}()
	}
	wg.Wait()
	healthy := 0
	for _, s := range statuses {
		if s.Connected {
			healthy++
		}
	}
	code := http.StatusOK
	if healthy == 0 {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"status":    map[bool]string{true: "ok", false: "degraded"}[healthy == len(statuses)],
		"version":   version,
		"upstreams": statuses,
	})
}
