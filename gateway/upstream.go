package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold-go/auth"
	"github.com/fold-run/fold-go/config"
	"github.com/fold-run/fold-go/internal/state"
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

	// bridgedIdleTimeout bounds how long a per-client upstream session
	// outlives its last use before the sweeper closes it.
	bridgedIdleTimeout = 5 * time.Minute
)

// upstream wraps one configured MCP server: a shared root session (lists,
// health, completion, subscriptions), per-client bridged sessions (named
// invocations, so server-initiated traffic flows back to the right client),
// credential injection, and the per-upstream guards (rate limit, breaker).
type upstream struct {
	cfg   config.Upstream
	creds *auth.UpstreamCredentials

	limiter state.Limiter
	breaker state.Breaker
	lists   state.ListCache

	connectTimeout time.Duration
	requestTimeout time.Duration
	cacheTTL       time.Duration

	httpClient *http.Client

	// onResourceUpdated forwards resources/updated notifications arriving on
	// the root session (which holds all upstream subscriptions).
	onResourceUpdated func(context.Context, *mcp.ResourceUpdatedNotificationParams)

	// onListChanged re-emits an upstream's list_changed notification to the
	// gateway's connected clients ("tools" | "prompts" | "resources").
	onListChanged func(kind string)

	// metrics is the owning gateway's instrumentation (set by Gateway).
	metrics *metricsSet

	mu         sync.Mutex
	session    *mcp.ClientSession       // root session
	bridged    map[string]*bridgedEntry // downstream session ID → bridged session
	subscribed map[string]bool          // URIs subscribed on the root session
}

type bridgedEntry struct {
	session  *mcp.ClientSession
	lastUsed time.Time
}

func newUpstream(cfg config.Upstream, provider state.Provider) *upstream {
	u := &upstream{
		cfg:            cfg,
		creds:          auth.NewUpstreamCredentials(cfg.Auth, http.DefaultClient),
		lists:          provider.ListCache("up:" + cfg.ID),
		connectTimeout: defaultConnectTimeout,
		requestTimeout: defaultRequestTimeout,
		cacheTTL:       defaultCacheTTL,
		bridged:        map[string]*bridgedEntry{},
		subscribed:     map[string]bool{},
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
	rpm := 0
	if rl := cfg.RateLimit; rl != nil {
		rpm = rl.RequestsPerMinute
	}
	u.limiter = provider.Limiter("up:"+cfg.ID, rpm)
	threshold, halfOpen := 5, 30*time.Second
	if cb := cfg.CircuitBreaker; cb != nil {
		if cb.FailureThreshold > 0 {
			threshold = cb.FailureThreshold
		}
		if cb.HalfOpenAfterMs > 0 {
			halfOpen = time.Duration(cb.HalfOpenAfterMs) * time.Millisecond
		}
	}
	u.breaker = provider.Breaker("up:"+cfg.ID, threshold, halfOpen)
	sessionEra := cfg.Protocol == "" || cfg.Protocol == "session"
	u.httpClient = &http.Client{
		Transport: &credentialTransport{creds: u.creds, base: http.DefaultTransport, sessionEra: sessionEra},
	}
	return u
}

// credentialTransport attaches upstream credentials to every outgoing
// request, resolving caller-derived strategies (passthrough, token-exchange)
// from the request context. In session mode it also answers server/discover
// with 405, steering the SDK client onto the sessionful legacy handshake —
// the only era whose connections can carry server-initiated requests
// (sampling, elicitation) back through the gateway.
type credentialTransport struct {
	creds      *auth.UpstreamCredentials
	base       http.RoundTripper
	sessionEra bool
}

func (t *credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.sessionEra && req.Method == http.MethodPost && req.Body != nil {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if bytes.Contains(body, []byte(`"server/discover"`)) {
			return &http.Response{
				StatusCode: http.StatusMethodNotAllowed,
				Status:     "405 Method Not Allowed",
				Proto:      req.Proto,
				ProtoMajor: req.ProtoMajor,
				ProtoMinor: req.ProtoMinor,
				Header:     http.Header{},
				Body:       io.NopCloser(bytes.NewReader(nil)),
				Request:    req,
			}, nil
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	req = req.Clone(req.Context())
	if err := t.creds.Apply(req.Context(), req.Header); err != nil {
		return nil, fmt.Errorf("upstream credentials: %w", err)
	}
	injectTraceContext(req.Context(), req.Header)
	return t.base.RoundTrip(req)
}

func (u *upstream) connect(ctx context.Context, opts *mcp.ClientOptions) (*mcp.ClientSession, error) {
	client := mcp.NewClient(&mcp.Implementation{Name: "fold-gateway", Version: version}, opts)
	cctx, cancel := context.WithTimeout(ctx, u.connectTimeout)
	defer cancel()
	session, err := client.Connect(cctx, &mcp.StreamableClientTransport{
		Endpoint:   u.cfg.URL,
		HTTPClient: u.httpClient,
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect upstream %q: %w", u.cfg.ID, err)
	}
	return session, nil
}

// rootSession connects (or reuses) the shared upstream session. List-changed
// notifications invalidate the list caches; resource-updated notifications
// are forwarded so subscribed clients hear them.
func (u *upstream) rootSession(ctx context.Context) (*mcp.ClientSession, error) {
	u.mu.Lock()
	if u.session != nil {
		s := u.session
		u.mu.Unlock()
		return s, nil
	}
	resub := make([]string, 0, len(u.subscribed))
	for uri := range u.subscribed {
		resub = append(resub, uri)
	}
	u.mu.Unlock()

	notifyDownstream := func(kind string) {
		if u.onListChanged != nil {
			u.onListChanged(kind)
		}
	}
	opts := &mcp.ClientOptions{
		ToolListChangedHandler: func(ctx context.Context, _ *mcp.ToolListChangedRequest) {
			u.lists.Invalidate(ctx, "tools")
			notifyDownstream("tools")
		},
		PromptListChangedHandler: func(ctx context.Context, _ *mcp.PromptListChangedRequest) {
			u.lists.Invalidate(ctx, "prompts")
			notifyDownstream("prompts")
		},
		ResourceListChangedHandler: func(ctx context.Context, _ *mcp.ResourceListChangedRequest) {
			u.lists.Invalidate(ctx, "resources")
			notifyDownstream("resources")
		},
		ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			if u.onResourceUpdated != nil {
				u.onResourceUpdated(ctx, req.Params)
			}
		},
	}
	session, err := u.connect(ctx, opts)
	if err != nil {
		return nil, err
	}

	u.mu.Lock()
	if u.session != nil {
		// Lost the race; use the winner.
		s := u.session
		u.mu.Unlock()
		session.Close()
		return s, nil
	}
	u.session = session
	u.mu.Unlock()

	// Restore upstream subscriptions lost with the previous session.
	for _, uri := range resub {
		session.Subscribe(ctx, &mcp.SubscribeParams{URI: uri})
	}
	return session, nil
}

// bridgedSession connects (or reuses) the per-client session for downstream
// session key. opts carries the handlers that forward server-initiated
// traffic (sampling, elicitation, logging, progress) back to that client.
func (u *upstream) bridgedSession(ctx context.Context, key string, opts *mcp.ClientOptions) (*mcp.ClientSession, error) {
	u.mu.Lock()
	if e := u.bridged[key]; e != nil {
		e.lastUsed = time.Now()
		s := e.session
		u.mu.Unlock()
		return s, nil
	}
	u.mu.Unlock()

	session, err := u.connect(ctx, opts)
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	if e := u.bridged[key]; e != nil {
		e.lastUsed = time.Now()
		s := e.session
		u.mu.Unlock()
		session.Close()
		return s, nil
	}
	u.bridged[key] = &bridgedEntry{session: session, lastUsed: time.Now()}
	u.mu.Unlock()
	return session, nil
}

func (u *upstream) dropSession(s *mcp.ClientSession) {
	u.mu.Lock()
	if u.session == s {
		u.session = nil
	}
	for key, e := range u.bridged {
		if e.session == s {
			delete(u.bridged, key)
		}
	}
	u.mu.Unlock()
	if s != nil {
		s.Close()
	}
}

// sweepBridged closes bridged sessions idle longer than bridgedIdleTimeout.
func (u *upstream) sweepBridged() {
	u.mu.Lock()
	var stale []*mcp.ClientSession
	for key, e := range u.bridged {
		if time.Since(e.lastUsed) > bridgedIdleTimeout {
			stale = append(stale, e.session)
			delete(u.bridged, key)
		}
	}
	u.mu.Unlock()
	for _, s := range stale {
		s.Close()
	}
}

// Close shuts all held sessions down.
func (u *upstream) Close() {
	u.mu.Lock()
	sessions := []*mcp.ClientSession{u.session}
	u.session = nil
	for _, e := range u.bridged {
		sessions = append(sessions, e.session)
	}
	u.bridged = map[string]*bridgedEntry{}
	u.mu.Unlock()
	for _, s := range sessions {
		if s != nil {
			s.Close()
		}
	}
}

// do runs one guarded request on the root session.
func (u *upstream) do(ctx context.Context, fn func(context.Context, *mcp.ClientSession) error) error {
	return u.guardedDo(ctx, u.rootSession, fn)
}

// doBridged runs one guarded request on the per-client session for key.
func (u *upstream) doBridged(ctx context.Context, key string, opts *mcp.ClientOptions, fn func(context.Context, *mcp.ClientSession) error) error {
	if key == "" {
		return u.do(ctx, fn)
	}
	return u.guardedDo(ctx, func(ctx context.Context) (*mcp.ClientSession, error) {
		return u.bridgedSession(ctx, key, opts)
	}, fn)
}

// guardedDo applies the per-upstream guards around fn: rate limit, circuit
// breaker, request timeout, session drop on transport failure.
func (u *upstream) guardedDo(ctx context.Context, acquire func(context.Context) (*mcp.ClientSession, error), fn func(context.Context, *mcp.ClientSession) error) error {
	start := time.Now()
	observe := func(outcome string) {
		if u.metrics != nil {
			u.metrics.observeUpstream(u.cfg.ID, outcome, time.Since(start))
		}
	}
	if ok, retry := u.limiter.Allow(ctx); !ok {
		observe("rate_limited")
		return &jsonrpc.Error{
			Code:    codeRateLimited,
			Message: fmt.Sprintf("upstream %q rate limit exceeded; retry in %s", u.cfg.ID, retry.Round(time.Second)),
		}
	}
	if !u.breaker.Allow(ctx) {
		observe("circuit_open")
		return &jsonrpc.Error{
			Code:    codeUpstreamDown,
			Message: fmt.Sprintf("upstream %q unavailable (circuit open)", u.cfg.ID),
		}
	}
	session, err := acquire(ctx)
	if err != nil {
		u.breaker.Record(ctx, false)
		observe("connect_error")
		return &jsonrpc.Error{Code: codeUpstreamDown, Message: err.Error()}
	}
	rctx, cancel := context.WithTimeout(ctx, u.requestTimeout)
	defer cancel()
	err = fn(rctx, session)
	if err == nil {
		u.breaker.Record(ctx, true)
		observe("ok")
		return nil
	}
	// A JSON-RPC error response proves the upstream is alive: pass it
	// through verbatim and don't count it against the breaker.
	var wire *jsonrpc.Error
	if errors.As(err, &wire) {
		u.breaker.Record(ctx, true)
		observe("ok")
		return wire
	}
	u.breaker.Record(ctx, false)
	if isConnectionError(err) {
		u.dropSession(session)
	}
	observe("error")
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

// cachedList fetches a list through the upstream's shared cache, which
// stores serialized JSON so entries can live in Redis and be shared across
// gateway instances.
func cachedList[T any](ctx context.Context, u *upstream, key string, fetch func(context.Context, *mcp.ClientSession) ([]T, error)) ([]T, error) {
	data, err := u.lists.GetOrFill(ctx, key, u.cacheTTL, func(ctx context.Context) ([]byte, error) {
		var items []T
		err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
			var err error
			items, err = fetch(ctx, s)
			return err
		})
		if err != nil {
			return nil, err
		}
		return json.Marshal(items)
	})
	if err != nil {
		return nil, err
	}
	var items []T
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, fmt.Errorf("decode cached %s for upstream %q: %w", key, u.cfg.ID, err)
	}
	return items, nil
}

// listTools returns the upstream's full (un-namespaced) tool list, cached.
func (u *upstream) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	return cachedList(ctx, u, "tools", func(ctx context.Context, s *mcp.ClientSession) ([]*mcp.Tool, error) {
		var tools []*mcp.Tool
		for t, err := range s.Tools(ctx, nil) {
			if err != nil {
				return nil, err
			}
			tools = append(tools, t)
		}
		return tools, nil
	})
}

func (u *upstream) listPrompts(ctx context.Context) ([]*mcp.Prompt, error) {
	return cachedList(ctx, u, "prompts", func(ctx context.Context, s *mcp.ClientSession) ([]*mcp.Prompt, error) {
		var prompts []*mcp.Prompt
		for p, err := range s.Prompts(ctx, nil) {
			if err != nil {
				return nil, err
			}
			prompts = append(prompts, p)
		}
		return prompts, nil
	})
}

func (u *upstream) listResources(ctx context.Context) ([]*mcp.Resource, error) {
	return cachedList(ctx, u, "resources", func(ctx context.Context, s *mcp.ClientSession) ([]*mcp.Resource, error) {
		var res []*mcp.Resource
		for r, err := range s.Resources(ctx, nil) {
			if err != nil {
				return nil, err
			}
			res = append(res, r)
		}
		return res, nil
	})
}

func (u *upstream) listResourceTemplates(ctx context.Context) ([]*mcp.ResourceTemplate, error) {
	return cachedList(ctx, u, "resources/templates", func(ctx context.Context, s *mcp.ClientSession) ([]*mcp.ResourceTemplate, error) {
		var res []*mcp.ResourceTemplate
		for r, err := range s.ResourceTemplates(ctx, nil) {
			if err != nil {
				return nil, err
			}
			res = append(res, r)
		}
		return res, nil
	})
}

func (u *upstream) callTool(ctx context.Context, key string, opts *mcp.ClientOptions, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	var out *mcp.CallToolResult
	err := u.doBridged(ctx, key, opts, func(ctx context.Context, s *mcp.ClientSession) error {
		var err error
		out, err = s.CallTool(ctx, params)
		return err
	})
	return out, err
}

func (u *upstream) getPrompt(ctx context.Context, key string, opts *mcp.ClientOptions, params *mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	var out *mcp.GetPromptResult
	err := u.doBridged(ctx, key, opts, func(ctx context.Context, s *mcp.ClientSession) error {
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

func (u *upstream) complete(ctx context.Context, params *mcp.CompleteParams) (*mcp.CompleteResult, error) {
	var out *mcp.CompleteResult
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		var err error
		out, err = s.Complete(ctx, params)
		return err
	})
	return out, err
}

// subscribe subscribes the root session to uri, remembering it for
// resubscription after a reconnect.
func (u *upstream) subscribe(ctx context.Context, uri string) error {
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		return s.Subscribe(ctx, &mcp.SubscribeParams{URI: uri})
	})
	if err == nil {
		u.mu.Lock()
		u.subscribed[uri] = true
		u.mu.Unlock()
	}
	return err
}

func (u *upstream) unsubscribe(ctx context.Context, uri string) error {
	err := u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		return s.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: uri})
	})
	if err == nil {
		u.mu.Lock()
		delete(u.subscribed, uri)
		u.mu.Unlock()
	}
	return err
}

// setLoggingLevel propagates a client's logging level to its bridged
// session (logging notifications flow back per client).
func (u *upstream) setLoggingLevel(ctx context.Context, key string, opts *mcp.ClientOptions, level mcp.LoggingLevel) error {
	return u.doBridged(ctx, key, opts, func(ctx context.Context, s *mcp.ClientSession) error {
		return s.SetLoggingLevel(ctx, &mcp.SetLoggingLevelParams{Level: level})
	})
}

func (u *upstream) ping(ctx context.Context) error {
	return u.do(ctx, func(ctx context.Context, s *mcp.ClientSession) error {
		return s.Ping(ctx, nil)
	})
}
