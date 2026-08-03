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
	"strconv"
	"strings"
	"sync"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/fold-run/fold-go/audit"
	"github.com/fold-run/fold-go/auth"
	"github.com/fold-run/fold-go/config"
	"github.com/fold-run/fold-go/internal/breaker"
	"github.com/fold-run/fold-go/internal/ratelimit"
	"github.com/fold-run/fold-go/policy"
)

const version = "0.1.0"

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

	globalLimit *ratelimit.Limiter

	// resourceOwner remembers which upstream listed each resource URI
	// (URIs are opaque and never rewritten; ownership is remembered).
	resourceOwner sync.Map

	server  *mcp.Server
	handler http.Handler
}

// New builds a gateway from a validated config.
func New(cfg *config.Config) (*Gateway, error) {
	if err := cfg.Validate(); err != nil {
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
	}
	for _, ucfg := range cfg.Upstreams {
		u := newUpstream(ucfg)
		g.upstreams = append(g.upstreams, u)
		g.byID[ucfg.ID] = u
		if ucfg.Namespace != "" {
			g.byNamespace[ucfg.Namespace] = u
		}
	}
	if cfg.Server != nil && cfg.Server.RateLimit != nil {
		g.globalLimit = ratelimit.New(cfg.Server.RateLimit.RequestsPerMinute)
	}
	if cfg.AuthRequired() {
		g.verifier = auth.NewVerifier(cfg.Auth, http.DefaultClient)
	}

	g.server = mcp.NewServer(
		&mcp.Implementation{Name: "fold", Title: "fold gateway", Version: version},
		&mcp.ServerOptions{
			Instructions: g.instructions(),
			Capabilities: &mcp.ServerCapabilities{
				Tools:     &mcp.ToolCapabilities{ListChanged: true},
				Prompts:   &mcp.PromptCapabilities{ListChanged: true},
				Resources: &mcp.ResourceCapabilities{ListChanged: true},
			},
		},
	)
	g.server.AddReceivingMiddleware(g.federationMiddleware)
	g.handler = g.buildHandler()
	return g, nil
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

// Close shuts down all upstream sessions.
func (g *Gateway) Close() {
	for _, u := range g.upstreams {
		u.Close()
	}
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
	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	wildcard := false
	if g.cfg.Server != nil {
		for _, h := range g.cfg.Server.AllowedHosts {
			if h == "*" {
				wildcard = true
			}
			allowed[strings.ToLower(h)] = true
		}
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
				http.Error(w, "forbidden host", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				if i := strings.Index(origin, "://"); i >= 0 {
					if !allowed[hostname(origin[i+3:])] {
						http.Error(w, "forbidden origin", http.StatusForbidden)
						return
					}
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
		if ok, retry := g.globalLimit.Allow(); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
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

// upstreamHealth is one upstream's health snapshot.
type upstreamHealth struct {
	ID        string            `json:"id"`
	Namespace string            `json:"namespace,omitempty"`
	URL       string            `json:"url"`
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
	statuses := make([]upstreamHealth, len(g.upstreams))
	var wg sync.WaitGroup
	for i, u := range g.upstreams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h := upstreamHealth{
				ID:        u.cfg.ID,
				Namespace: u.cfg.Namespace,
				URL:       u.cfg.URL,
				Owner:     u.cfg.Owner,
				Labels:    u.cfg.Labels,
				Breaker:   u.breaker.State(),
			}
			start := time.Now()
			if err := u.ping(ctx); err != nil {
				h.Error = err.Error()
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
