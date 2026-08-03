package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold-go/auth"
	"github.com/fold-run/fold-go/config"
	"github.com/fold-run/fold-go/internal/breaker"
	"github.com/fold-run/fold-go/internal/cache"
	"github.com/fold-run/fold-go/internal/ratelimit"
)

// Gateway-minted JSON-RPC error codes, documented in the README.
const (
	codeRateLimited       = -32040 // per-upstream rate limit exceeded
	codeUpstreamDown      = -32041 // circuit open or upstream unreachable
	codeDenied            = -32042 // policy denied the invocation
	codeUnknownNamespace  = -32043 // name does not resolve to an upstream
	defaultCacheTTL       = 30 * time.Second
	defaultConnectTimeout = 5 * time.Second
	defaultRequestTimeout = 60 * time.Second
)

// upstream wraps one configured MCP server: a persistent client session,
// credential injection, and the per-upstream guards (rate limit, breaker).
type upstream struct {
	cfg   config.Upstream
	creds *auth.UpstreamCredentials

	limiter *ratelimit.Limiter
	breaker *breaker.Breaker
	lists   *cache.Cache

	connectTimeout time.Duration
	requestTimeout time.Duration
	cacheTTL       time.Duration

	httpClient *http.Client

	mu      sync.Mutex
	session *mcp.ClientSession
}

func newUpstream(cfg config.Upstream) *upstream {
	u := &upstream{
		cfg:            cfg,
		creds:          auth.NewUpstreamCredentials(cfg.Auth, http.DefaultClient),
		lists:          cache.New(),
		connectTimeout: defaultConnectTimeout,
		requestTimeout: defaultRequestTimeout,
		cacheTTL:       defaultCacheTTL,
	}
	if t := cfg.Timeouts; t != nil {
		if t.ConnectMs > 0 {
			u.connectTimeout = time.Duration(t.ConnectMs) * time.Millisecond
		}
		if t.RequestMs > 0 {
			u.requestTimeout = time.Duration(t.RequestMs) * time.Millisecond
		}
	}
	if cfg.CacheTTLMs > 0 {
		u.cacheTTL = time.Duration(cfg.CacheTTLMs) * time.Millisecond
	} else if cfg.CacheTTLMs < 0 {
		u.cacheTTL = 0
	}
	if rl := cfg.RateLimit; rl != nil {
		u.limiter = ratelimit.New(rl.RequestsPerMinute)
	}
	threshold, halfOpen := 5, 30*time.Second
	if cb := cfg.CircuitBreaker; cb != nil {
		if cb.FailureThreshold > 0 {
			threshold = cb.FailureThreshold
		}
		if cb.HalfOpenAfterMs > 0 {
			halfOpen = time.Duration(cb.HalfOpenAfterMs) * time.Millisecond
		}
	}
	u.breaker = breaker.New(threshold, halfOpen)
	u.httpClient = &http.Client{
		Transport: &credentialTransport{creds: u.creds, base: http.DefaultTransport},
	}
	return u
}

// credentialTransport attaches upstream credentials to every outgoing
// request, resolving caller-derived strategies (passthrough, token-exchange)
// from the request context.
type credentialTransport struct {
	creds *auth.UpstreamCredentials
	base  http.RoundTripper
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	if err := t.creds.Apply(req.Context(), req.Header); err != nil {
		return nil, fmt.Errorf("upstream credentials: %w", err)
	}
	return t.base.RoundTrip(req)
}

// connect (or reuse) the upstream session.
func (u *upstream) connectedSession(ctx context.Context) (*mcp.ClientSession, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.session != nil {
		return u.session, nil
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "fold-gateway", Version: version}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			u.lists.Invalidate("tools")
		},
		PromptListChangedHandler: func(context.Context, *mcp.PromptListChangedRequest) {
			u.lists.Invalidate("prompts")
		},
		ResourceListChangedHandler: func(context.Context, *mcp.ResourceListChangedRequest) {
			u.lists.Invalidate("resources")
		},
	})
	cctx, cancel := context.WithTimeout(ctx, u.connectTimeout)
	defer cancel()
	session, err := client.Connect(cctx, &mcp.StreamableClientTransport{
		Endpoint:   u.cfg.URL,
		HTTPClient: u.httpClient,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect upstream %q: %w", u.cfg.ID, err)
	}
	u.session = session
	return session, nil
}

func (u *upstream) dropSession(s *mcp.ClientSession) {
	u.mu.Lock()
	if u.session == s {
		u.session = nil
	}
	u.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// Close shuts the held session down.
func (u *upstream) Close() {
	u.mu.Lock()
	s := u.session
	u.session = nil
	u.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// do runs one guarded request against the upstream: per-upstream rate limit,
// circuit breaker, request timeout, session reconnect on transport failure.
func (u *upstream) do(ctx context.Context, fn func(context.Context, *mcp.ClientSession) error) error {
	if ok, retry := u.limiter.Allow(); !ok {
		return &jsonrpc.Error{
			Code:    codeRateLimited,
			Message: fmt.Sprintf("upstream %q rate limit exceeded; retry in %s", u.cfg.ID, retry.Round(time.Second)),
		}
	}
	if !u.breaker.Allow() {
		return &jsonrpc.Error{
			Code:    codeUpstreamDown,
			Message: fmt.Sprintf("upstream %q unavailable (circuit open)", u.cfg.ID),
		}
	}
	session, err := u.connectedSession(ctx)
	if err != nil {
		u.breaker.Record(false)
		return &jsonrpc.Error{Code: codeUpstreamDown, Message: err.Error()}
	}
	rctx, cancel := context.WithTimeout(ctx, u.requestTimeout)
	defer cancel()
	err = fn(rctx, session)
	if err == nil {
		u.breaker.Record(true)
		return nil
	}
	// A JSON-RPC error response proves the upstream is alive: pass it
	// through verbatim and don't count it against the breaker.
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		u.breaker.Record(true)
		return wire
	}
	u.breaker.Record(false)
	if isConnectionError(err) {
		u.dropSession(session)
	}
	return &jsonrpc.Error{
		Code:    codeUpstreamDown,
		Message: fmt.Sprintf("upstream %q: %v", u.cfg.ID, err),
	}
}

func isConnectionError(err error) bool {
	if errors.Is(err, mcp.ErrConnectionClosed) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "connection") || strings.Contains(msg, "EOF") ||
		strings.Contains(msg, "broken pipe") || strings.Contains(msg, "refused")
}

// listTools returns the upstream's full (un-namespaced) tool list, cached.
func (u *upstream) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	v, err := u.lists.GetOrFill(ctx, "tools", u.cacheTTL, func(ctx context.Context) (any, error) {
		var tools []*mcp.Tool
		err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
			for t, err := range s.Tools(ctx, nil) {
				if err != nil {
					return err
				}
				tools = append(tools, t)
			}
			return nil
		})
		return tools, err
	})
	if err != nil {
		return nil, err
	}
	return v.([]*mcp.Tool), nil
}

func (u *upstream) listPrompts(ctx context.Context) ([]*mcp.Prompt, error) {
	v, err := u.lists.GetOrFill(ctx, "prompts", u.cacheTTL, func(ctx context.Context) (any, error) {
		var prompts []*mcp.Prompt
		err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
			for p, err := range s.Prompts(ctx, nil) {
				if err != nil {
					return err
				}
				prompts = append(prompts, p)
			}
			return nil
		})
		return prompts, err
	})
	if err != nil {
		return nil, err
	}
	return v.([]*mcp.Prompt), nil
}

func (u *upstream) listResources(ctx context.Context) ([]*mcp.Resource, error) {
	v, err := u.lists.GetOrFill(ctx, "resources", u.cacheTTL, func(ctx context.Context) (any, error) {
		var res []*mcp.Resource
		err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
			for r, err := range s.Resources(ctx, nil) {
				if err != nil {
					return err
				}
				res = append(res, r)
			}
			return nil
		})
		return res, err
	})
	if err != nil {
		return nil, err
	}
	return v.([]*mcp.Resource), nil
}

func (u *upstream) listResourceTemplates(ctx context.Context) ([]*mcp.ResourceTemplate, error) {
	v, err := u.lists.GetOrFill(ctx, "resources/templates", u.cacheTTL, func(ctx context.Context) (any, error) {
		var res []*mcp.ResourceTemplate
		err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
			for r, err := range s.ResourceTemplates(ctx, nil) {
				if err != nil {
					return err
				}
				res = append(res, r)
			}
			return nil
		})
		return res, err
	})
	if err != nil {
		return nil, err
	}
	return v.([]*mcp.ResourceTemplate), nil
}

func (u *upstream) callTool(ctx context.Context, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	var out *mcp.CallToolResult
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		var err error
		out, err = s.CallTool(ctx, params)
		return err
	})
	return out, err
}

func (u *upstream) getPrompt(ctx context.Context, params *mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	var out *mcp.GetPromptResult
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		var err error
		out, err = s.GetPrompt(ctx, params)
		return err
	})
	return out, err
}

func (u *upstream) readResource(ctx context.Context, params *mcp.ReadResourceParams) (*mcp.ReadResourceResult, error) {
	var out *mcp.ReadResourceResult
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		var err error
		out, err = s.ReadResource(ctx, params)
		return err
	})
	return out, err
}

func (u *upstream) ping(ctx context.Context) error {
	return u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		return s.Ping(ctx, nil)
	})
}
