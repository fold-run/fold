package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold-go/audit"
	"github.com/fold-run/fold-go/auth"
)

// metaPartialFailure marks list results assembled while one or more
// upstreams were unreachable.
const metaPartialFailure = "run.fold/partialFailure"

// metaUpstream tags proxied results with the upstream that served them.
const metaUpstream = "run.fold/upstream"

// federationMiddleware is the gateway's request pipeline at the MCP layer:
// principal extraction → authorize → route (fan-out or named) → per-upstream
// guards → proxy → egress rewriting → audit (single exit door).
func (g *Gateway) federationMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		start := time.Now()
		principal := principalFrom(req)
		ctx = auth.WithPrincipal(ctx, principal)

		evt := audit.Event{Method: method}
		if principal != nil {
			evt.Principal = principal.Subject
			evt.Issuer = principal.Issuer
		}

		res, err := g.route(ctx, method, req, &evt, next)

		evt.LatencyMs = time.Since(start).Milliseconds()
		if err != nil {
			evt.Error = err.Error()
			if evt.Outcome == "" {
				evt.Outcome = classify(err)
			}
		} else if evt.Outcome == "" {
			evt.Outcome = audit.OutcomeOK
		}
		// Notifications and pings are protocol plumbing; keep audit focused
		// on requests, matching fold's "every request including denials".
		if method != "notifications/initialized" && method != "ping" {
			g.audit.Emit(evt)
		}
		return res, err
	}
}

func principalFrom(req mcp.Request) *auth.Principal {
	extra := req.GetExtra()
	if extra == nil || extra.TokenInfo == nil {
		return nil
	}
	p, _ := extra.TokenInfo.Extra[tokenInfoPrincipalKey].(*auth.Principal)
	return p
}

func classify(err error) audit.Outcome {
	var wire *jsonrpc.Error
	if ok := asWireError(err, &wire); ok {
		switch wire.Code {
		case codeRateLimited:
			return audit.OutcomeRateLimited
		case codeUpstreamDown:
			return audit.OutcomeUpstreamDown
		case codeDenied:
			return audit.OutcomeDenied
		}
	}
	return audit.OutcomeError
}

func (g *Gateway) route(ctx context.Context, method string, req mcp.Request, evt *audit.Event, next mcp.MethodHandler) (mcp.Result, error) {
	switch method {
	case "tools/list":
		return g.listTools(ctx, evt)
	case "tools/call":
		return g.callTool(ctx, req, evt)
	case "prompts/list":
		return g.listPrompts(ctx, evt)
	case "prompts/get":
		return g.getPrompt(ctx, req, evt)
	case "resources/list":
		return g.listResources(ctx, evt)
	case "resources/templates/list":
		return g.listResourceTemplates(ctx, evt)
	case "resources/read":
		return g.readResource(ctx, req, evt)
	default:
		// initialize, ping, notifications, logging, completion — handled by
		// the SDK server itself.
		return next(ctx, method, req)
	}
}

// resolve maps a namespaced public name to (upstream, bare name).
func (g *Gateway) resolve(name string) (*upstream, string, error) {
	if g.passthrough {
		return g.upstreams[0], name, nil
	}
	ns, bare, ok := strings.Cut(name, g.sep)
	if ok {
		if u := g.byNamespace[ns]; u != nil {
			return u, bare, nil
		}
	}
	return nil, "", &jsonrpc.Error{
		Code:    codeUnknownNamespace,
		Message: fmt.Sprintf("unknown name %q: no upstream owns this namespace", name),
	}
}

// public returns the namespaced public name for an upstream's bare name.
func (g *Gateway) public(u *upstream, name string) string {
	if u.cfg.Namespace == "" {
		return name
	}
	return u.cfg.Namespace + g.sep + name
}

// fanOut runs fn against every upstream concurrently and collects results
// keyed by upstream index, plus the ids of upstreams that failed.
func fanOut[T any](ctx context.Context, ups []*upstream, fn func(context.Context, *upstream) (T, error)) (results []T, failed []string) {
	results = make([]T, len(ups))
	errs := make([]error, len(ups))
	var wg sync.WaitGroup
	for i, u := range ups {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = fn(ctx, u)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			failed = append(failed, ups[i].cfg.ID)
		}
	}
	sort.Strings(failed)
	return results, failed
}

// partialFailureMeta degrades gracefully: some upstreams down → results from
// the healthy ones plus a partial-failure marker; all down → an error.
func partialFailureMeta(failed []string, total int) (mcp.Meta, error) {
	if len(failed) == 0 {
		return nil, nil
	}
	if len(failed) == total {
		return nil, &jsonrpc.Error{
			Code:    codeUpstreamDown,
			Message: fmt.Sprintf("all upstreams unavailable: %s", strings.Join(failed, ", ")),
		}
	}
	return mcp.Meta{metaPartialFailure: map[string]any{"failedUpstreams": failed}}, nil
}

func (g *Gateway) listTools(ctx context.Context, evt *audit.Event) (mcp.Result, error) {
	principal := auth.PrincipalFromContext(ctx)
	lists, failed := fanOut(ctx, g.upstreams, func(ctx context.Context, u *upstream) ([]*mcp.Tool, error) {
		return u.listTools(ctx)
	})
	meta, err := partialFailureMeta(failed, len(g.upstreams))
	if err != nil {
		return nil, err
	}
	out := &mcp.ListToolsResult{Tools: []*mcp.Tool{}, Meta: meta}
	for i, u := range g.upstreams {
		for _, t := range lists[i] {
			if !g.policy.Visible(principal, u.cfg.ID, "tools/call", t.Name) {
				continue
			}
			nt := *t
			nt.Name = g.public(u, t.Name)
			out.Tools = append(out.Tools, &nt)
		}
	}
	return out, nil
}

func (g *Gateway) callTool(ctx context.Context, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok || params == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "missing tool name"}
	}
	evt.Name = params.Name
	u, bare, err := g.resolve(params.Name)
	if err != nil {
		return nil, err
	}
	evt.Upstream = u.cfg.ID
	if res, err := g.authorize(ctx, evt, u, "tools/call", bare); err != nil {
		return res, err
	}
	out, err := u.callTool(ctx, &mcp.CallToolParams{
		Name:      bare,
		Arguments: params.Arguments,
		Meta:      params.Meta,
	})
	if err != nil {
		return nil, err
	}
	tagUpstream(&out.Meta, u)
	return out, nil
}

func (g *Gateway) listPrompts(ctx context.Context, evt *audit.Event) (mcp.Result, error) {
	principal := auth.PrincipalFromContext(ctx)
	lists, failed := fanOut(ctx, g.upstreams, func(ctx context.Context, u *upstream) ([]*mcp.Prompt, error) {
		return u.listPrompts(ctx)
	})
	meta, err := partialFailureMeta(failed, len(g.upstreams))
	if err != nil {
		return nil, err
	}
	out := &mcp.ListPromptsResult{Prompts: []*mcp.Prompt{}, Meta: meta}
	for i, u := range g.upstreams {
		for _, p := range lists[i] {
			if !g.policy.Visible(principal, u.cfg.ID, "prompts/get", p.Name) {
				continue
			}
			np := *p
			np.Name = g.public(u, p.Name)
			out.Prompts = append(out.Prompts, &np)
		}
	}
	return out, nil
}

func (g *Gateway) getPrompt(ctx context.Context, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.GetPromptParams)
	if !ok || params == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "missing prompt name"}
	}
	evt.Name = params.Name
	u, bare, err := g.resolve(params.Name)
	if err != nil {
		return nil, err
	}
	evt.Upstream = u.cfg.ID
	if res, err := g.authorize(ctx, evt, u, "prompts/get", bare); err != nil {
		return res, err
	}
	out, err := u.getPrompt(ctx, &mcp.GetPromptParams{
		Name:      bare,
		Arguments: params.Arguments,
		Meta:      params.Meta,
	})
	if err != nil {
		return nil, err
	}
	tagUpstream(&out.Meta, u)
	return out, nil
}

func (g *Gateway) listResources(ctx context.Context, evt *audit.Event) (mcp.Result, error) {
	lists, failed := fanOut(ctx, g.upstreams, func(ctx context.Context, u *upstream) ([]*mcp.Resource, error) {
		return u.listResources(ctx)
	})
	meta, err := partialFailureMeta(failed, len(g.upstreams))
	if err != nil {
		return nil, err
	}
	out := &mcp.ListResourcesResult{Resources: []*mcp.Resource{}, Meta: meta}
	for i, u := range g.upstreams {
		for _, r := range lists[i] {
			// Resource URIs are opaque identifiers clients persist; fold
			// never rewrites them. Ownership is remembered instead.
			g.resourceOwner.Store(r.URI, u.cfg.ID)
			out.Resources = append(out.Resources, r)
		}
	}
	return out, nil
}

func (g *Gateway) listResourceTemplates(ctx context.Context, evt *audit.Event) (mcp.Result, error) {
	lists, failed := fanOut(ctx, g.upstreams, func(ctx context.Context, u *upstream) ([]*mcp.ResourceTemplate, error) {
		return u.listResourceTemplates(ctx)
	})
	meta, err := partialFailureMeta(failed, len(g.upstreams))
	if err != nil {
		return nil, err
	}
	out := &mcp.ListResourceTemplatesResult{ResourceTemplates: []*mcp.ResourceTemplate{}, Meta: meta}
	for i := range g.upstreams {
		out.ResourceTemplates = append(out.ResourceTemplates, lists[i]...)
	}
	return out, nil
}

func (g *Gateway) readResource(ctx context.Context, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.ReadResourceParams)
	if !ok || params == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "missing resource uri"}
	}
	evt.Name = params.URI

	// Affinity first: route to the upstream the URI was listed from.
	if id, ok := g.resourceOwner.Load(params.URI); ok {
		if u := g.byID[id.(string)]; u != nil {
			evt.Upstream = u.cfg.ID
			out, err := u.readResource(ctx, params)
			if err == nil {
				tagUpstream(&out.Meta, u)
				return out, nil
			}
		}
	}
	// Probe fallback: the owner answers, everyone else is a healthy "no".
	for _, u := range g.upstreams {
		out, err := u.readResource(ctx, params)
		if err == nil {
			g.resourceOwner.Store(params.URI, u.cfg.ID)
			evt.Upstream = u.cfg.ID
			tagUpstream(&out.Meta, u)
			return out, nil
		}
	}
	return nil, mcp.ResourceNotFoundError(params.URI)
}

// authorize applies the policy engine to a named invocation. Denials return
// a policy error; the audit event records the decision either way.
func (g *Gateway) authorize(ctx context.Context, evt *audit.Event, u *upstream, method, bare string) (mcp.Result, error) {
	d := g.policy.Decide(auth.PrincipalFromContext(ctx), u.cfg.ID, method, bare)
	evt.RuleID = d.RuleID
	if d.Allowed {
		evt.Decision = "allow"
		return nil, nil
	}
	evt.Decision = "deny"
	evt.Outcome = audit.OutcomeDenied
	return nil, &jsonrpc.Error{
		Code:    codeDenied,
		Message: fmt.Sprintf("policy denied %s %q", method, evt.Name),
	}
}

func tagUpstream(meta *mcp.Meta, u *upstream) {
	if *meta == nil {
		*meta = mcp.Meta{}
	}
	(*meta)[metaUpstream] = u.cfg.ID
}

func asWireError(err error, target **jsonrpc.Error) bool {
	for e := err; e != nil; {
		if we, ok := e.(*jsonrpc.Error); ok {
			*target = we
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}
