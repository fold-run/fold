package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/trace"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/policy"
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
		var hdr http.Header
		if extra := req.GetExtra(); extra != nil {
			hdr = extra.Header
			ctx = withTraceContext(ctx, hdr)
		}
		// Notifications and pings are protocol plumbing; keep audit, metrics,
		// and spans focused on requests, matching fold's "every request
		// including denials".
		plumbing := method == "notifications/initialized" || method == "ping"

		var span trace.Span
		if !plumbing {
			ctx, span = g.tracer.startServer(ctx, method, hdr)
		}

		ctx, m := withMeter(ctx)

		evt := audit.Event{Method: method}
		if principal != nil {
			evt.Principal = principal.Subject
			evt.Issuer = principal.Issuer
		}

		res, err := g.routeSafe(ctx, method, req, &evt, next)

		evt.UpstreamCalls = int(m.upstreamCalls.Load())
		if u := m.usage.Load(); u != nil {
			evt.Usage = *u
		}
		evt.LatencyMs = time.Since(start).Milliseconds()
		if err != nil {
			evt.Error = err.Error()
			if evt.Outcome == "" {
				evt.Outcome = classify(err)
			}
		} else if evt.Outcome == "" {
			evt.Outcome = audit.OutcomeOK
		}
		if !plumbing {
			g.metrics.observeRequest(method, string(evt.Outcome), time.Since(start))
			if evt.UpstreamCalls > 0 {
				g.metrics.observeFanOut(evt.UpstreamCalls)
			}
			// Read from the event rather than the context: the tenant is
			// recorded once, on the same value audit is about to carry, so
			// the two records cannot disagree about who this was.
			g.metrics.observeTenant(evt.Tenant, string(evt.Outcome), evt.UpstreamCalls)
			g.audit.Emit(evt)
			g.tracer.endServer(span, &evt, err)
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
		case codeBudgetExhausted:
			return audit.OutcomeBudgetExhausted
		}
	}
	return audit.OutcomeError
}

// routeSafe runs route, converting a panic into a JSON-RPC internal error.
// The SDK dispatches each request on its own goroutine with no recovery
// above this middleware, so without the conversion one nil-field edge case
// in a handler would end the process for every session. The wire message is
// generic: a panic value can carry internal details (endpoints, state) the
// error paths already refuse to leak. Falling through to the normal return
// keeps audit the single exit door — the event is still emitted, outcome
// "error".
func (g *Gateway) routeSafe(ctx context.Context, method string, req mcp.Request, evt *audit.Event, next mcp.MethodHandler) (res mcp.Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			notePanic(g.log, g.metrics, "route", r)
			res, err = nil, &jsonrpc.Error{Code: jsonrpc.CodeInternalError, Message: "internal gateway error"}
		}
	}()
	return g.route(ctx, method, req, evt, next)
}

func (g *Gateway) route(ctx context.Context, method string, req mcp.Request, evt *audit.Event, next mcp.MethodHandler) (mcp.Result, error) {
	// One snapshot per request: a concurrent Reload swaps the pointer, and
	// this request keeps routing against the world it started in.
	rt := g.rt()
	// Resolve the tenant once per request, alongside the principal. An
	// ambiguous match is refused rather than guessed: picking would hand some
	// caller another tenant's allowance and visibility.
	t, terr := rt.resolveTenant(auth.PrincipalFromContext(ctx))
	if terr != nil {
		g.log.Error("ambiguous tenant configuration", "err", terr.Message)
		return nil, terr
	}
	if t != nil {
		evt.Tenant = t.id()
	}
	ctx = withTenant(ctx, t)
	// What this client declared it can render decides which upstream session
	// serves it, because an upstream may register a different tool set for a
	// client that supports MCP Apps. See uicapability.go.
	ctx = withCapProfile(ctx, profileOf(req))
	switch method {
	case "tools/list":
		return g.listTools(ctx, rt, req, evt)
	case "tools/call":
		return g.callTool(ctx, rt, req, evt)
	case "prompts/list":
		return g.listPrompts(ctx, rt, req, evt)
	case "prompts/get":
		return g.getPrompt(ctx, rt, req, evt)
	case "resources/list":
		return g.listResources(ctx, rt, req, evt)
	case "resources/templates/list":
		return g.listResourceTemplates(ctx, rt, req, evt)
	case "resources/read":
		return g.readResource(ctx, rt, req, evt)
	case "completion/complete":
		return g.complete(ctx, rt, req, evt)
	case "logging/setLevel":
		return g.setLevel(ctx, rt, req, next)
	default:
		// initialize, ping, notifications, logging, completion — handled by
		// the SDK server itself.
		return next(ctx, method, req)
	}
}

// resolve maps a namespaced public name to (upstream, bare name).
func (g *Gateway) resolve(rt *routes, name string) (*upstream, string, error) {
	if rt.passthrough {
		return rt.upstreams[0], name, nil
	}
	ns, bare, ok := strings.Cut(name, g.sep)
	if ok {
		if u := rt.byNamespace[ns]; u != nil {
			return u, bare, nil
		}
	}
	return nil, "", &jsonrpc.Error{
		Code:    codeUnknownNamespace,
		Message: fmt.Sprintf("unknown name %q: no upstream owns this namespace", name),
	}
}

// public returns the namespaced public name for an upstream's bare name —
// the inverse of resolve. The rewrite itself lives on the upstream, which
// carries the same separator (newWiredUpstream), so the per-request path and
// the memoized list view cannot drift apart.
func (g *Gateway) public(u *upstream, name string) string {
	return u.publicName(name)
}

// fanOut runs fn against every upstream concurrently and collects results
// keyed by upstream index, plus the ids of upstreams that failed.
func fanOut[T any](ctx context.Context, ups []*upstream, fn func(context.Context, *upstream) (T, error)) (results []T, failed []string) {
	results = make([]T, len(ups))
	errs := make([]error, len(ups))
	var wg sync.WaitGroup
	for i, u := range ups {
		wg.Go(func() {
			// Each worker is its own goroutine: a panic here would skip the
			// middleware's recovery and kill the process, so it is converted
			// into that one upstream failing — the partial-failure shape the
			// caller already handles.
			defer func() {
				if r := recover(); r != nil {
					notePanic(u.log, u.metrics, "fanout", r)
					errs[i] = fmt.Errorf("upstream %q: internal error", u.cfg.ID)
				}
			}()
			results[i], errs[i] = fn(ctx, u)
		})
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

func (g *Gateway) listTools(ctx context.Context, rt *routes, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	principal := auth.PrincipalFromContext(ctx)
	ups := visibleUpstreams(ctx, rt.upstreams)
	lists, failed := fanOut(ctx, ups, func(ctx context.Context, u *upstream) ([]*mcp.Tool, error) {
		return u.listTools(ctx)
	})
	meta, err := partialFailureMeta(failed, len(ups))
	if err != nil {
		return nil, err
	}
	out := &mcp.ListToolsResult{Tools: []*mcp.Tool{}, Meta: meta}
	filter := newListFilter(principal, rt.policy, "tools/call")
	for i, u := range ups {
		// Visibility is decided on the bare name, the item emitted is the
		// namespaced one; the two lists are index-aligned.
		public := u.namespacedTools(ctx, lists[i])
		for j, t := range lists[i] {
			// The tool is in hand here, so a toolKind gate costs a field read
			// rather than a lookup — annotations arrive with the list.
			if !filter.visibleTool(u.cfg.ID, t) {
				continue
			}
			out.Tools = append(out.Tools, public[j])
		}
	}
	filter.finish(g, evt, &out.Meta)
	cursor := ""
	if p, ok := req.GetParams().(*mcp.ListToolsParams); ok && p != nil {
		cursor = p.Cursor
	}
	page, next, jerr := paginate(out.Tools, func(t *mcp.Tool) string { return t.Name },
		"tools", cursor, g.pageSize, principal)
	if jerr != nil {
		return nil, jerr
	}
	out.Tools, out.NextCursor = page, next
	evtItems(evt, len(out.Tools))
	return out, nil
}

func (g *Gateway) callTool(ctx context.Context, rt *routes, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.CallToolParamsRaw)
	if !ok || params == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "missing tool name"}
	}
	evt.Name = params.Name
	u, bare, err := g.resolve(rt, params.Name)
	if err != nil {
		return nil, err
	}
	evt.Upstream = u.cfg.ID
	ev := policy.Evidence{Args: rawArgs(rt, params.Arguments)}
	if rt.policy.NeedsAnnotations() {
		// Lazy: a call that never reaches a toolKind rule never resolves this,
		// and a document with no such rule never builds it at all.
		ev.Tool = func() (policy.ToolAnnotations, bool) { return u.annotationsFor(ctx, bare) }
	}
	if res, err := g.authorizeCall(ctx, evt, rt, u, "tools/call", bare, ev); err != nil {
		return res, err
	}
	// After policy, before the proxy: the hook's allow is necessary but never
	// sufficient, and its operator is never handed traffic the gateway has
	// already refused.
	if err := g.hookIngress(ctx, evt, rt, u, "tools/call", bare, params.Arguments); err != nil {
		return nil, err
	}
	// A missing arguments field must stay missing — a nil RawMessage would
	// marshal as JSON null, which schema-validating upstreams reject.
	var args any
	if len(params.Arguments) > 0 {
		args = params.Arguments
	}
	key, opts := g.bridgeFor(req, u)
	defer g.pushCallCtx(ctx, key)()
	before := g.bridgeActivity(key)
	out, err := u.callTool(ctx, key, opts, &mcp.CallToolParams{
		Name:      bare,
		Arguments: args,
		Meta:      sanitizeRequestMeta(params.Meta),
	})
	g.drainBridge(key, before)
	if err != nil {
		return nil, err
	}
	if err := g.hookEgress(ctx, evt, rt, u, "tools/call", bare, out); err != nil {
		return nil, err
	}
	// If the call minted a task, pin its ownership so later task calls skip
	// the probe.
	g.noteMintedTask(ctx, u, out.Meta)
	noteUsage(ctx, out.Meta)
	tagUpstream(&out.Meta, u)
	return out, nil
}

func (g *Gateway) listPrompts(ctx context.Context, rt *routes, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	principal := auth.PrincipalFromContext(ctx)
	ups := visibleUpstreams(ctx, rt.upstreams)
	lists, failed := fanOut(ctx, ups, func(ctx context.Context, u *upstream) ([]*mcp.Prompt, error) {
		return u.listPrompts(ctx)
	})
	meta, err := partialFailureMeta(failed, len(ups))
	if err != nil {
		return nil, err
	}
	out := &mcp.ListPromptsResult{Prompts: []*mcp.Prompt{}, Meta: meta}
	filter := newListFilter(principal, rt.policy, "prompts/get")
	for i, u := range ups {
		public := u.namespacedPrompts(ctx, lists[i]) // index-aligned — see listTools
		for j, p := range lists[i] {
			if !filter.visible(u.cfg.ID, p.Name) {
				continue
			}
			out.Prompts = append(out.Prompts, public[j])
		}
	}
	filter.finish(g, evt, &out.Meta)
	cursor := ""
	if p, ok := req.GetParams().(*mcp.ListPromptsParams); ok && p != nil {
		cursor = p.Cursor
	}
	page, next, jerr := paginate(out.Prompts, func(p *mcp.Prompt) string { return p.Name },
		"prompts", cursor, g.pageSize, principal)
	if jerr != nil {
		return nil, jerr
	}
	out.Prompts, out.NextCursor = page, next
	evtItems(evt, len(out.Prompts))
	return out, nil
}

func (g *Gateway) getPrompt(ctx context.Context, rt *routes, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.GetPromptParams)
	if !ok || params == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "missing prompt name"}
	}
	evt.Name = params.Name
	u, bare, err := g.resolve(rt, params.Name)
	if err != nil {
		return nil, err
	}
	evt.Upstream = u.cfg.ID
	if res, err := g.authorize(ctx, evt, rt, u, "prompts/get", bare); err != nil {
		return res, err
	}
	if err := g.hookIngress(ctx, evt, rt, u, "prompts/get", bare, nil); err != nil {
		return nil, err
	}
	key, opts := g.bridgeFor(req, u)
	defer g.pushCallCtx(ctx, key)()
	before := g.bridgeActivity(key)
	out, err := u.getPrompt(ctx, key, opts, &mcp.GetPromptParams{
		Name:      bare,
		Arguments: params.Arguments,
		Meta:      sanitizeRequestMeta(params.Meta),
	})
	g.drainBridge(key, before)
	if err != nil {
		return nil, err
	}
	if err := g.hookEgress(ctx, evt, rt, u, "prompts/get", bare, out); err != nil {
		return nil, err
	}
	tagUpstream(&out.Meta, u)
	return out, nil
}

func (g *Gateway) listResources(ctx context.Context, rt *routes, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	ups := visibleUpstreams(ctx, rt.upstreams)
	lists, failed := fanOut(ctx, ups, func(ctx context.Context, u *upstream) ([]*mcp.Resource, error) {
		return u.listResources(ctx)
	})
	meta, err := partialFailureMeta(failed, len(ups))
	if err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	out := &mcp.ListResourcesResult{Resources: []*mcp.Resource{}, Meta: meta}
	filter := newListFilter(principal, rt.policy, "resources/read")
	for i, u := range ups {
		// Visibility is decided on the upstream's own URI, the item emitted is
		// the public one; the two lists are index-aligned, as for tools.
		public := u.namespacedResources(ctx, lists[i])
		for j, r := range lists[i] {
			// Resource URIs are opaque identifiers clients persist; fold never
			// rewrites them, with one documented exception for the MCP Apps
			// `ui://` scheme (see uiresource.go). Ownership is remembered
			// instead — recorded even for filtered resources so reads still
			// route correctly.
			g.resourceOwner.Store(r.URI, u.cfg.ID, 0)
			if !filter.visible(u.cfg.ID, r.URI) {
				continue
			}
			// Shared with every other caller of this list (see cachedList):
			// forwarded as-is, never mutated.
			out.Resources = append(out.Resources, public[j])
		}
	}
	filter.finish(g, evt, &out.Meta)
	cursor := ""
	if p, ok := req.GetParams().(*mcp.ListResourcesParams); ok && p != nil {
		cursor = p.Cursor
	}
	page, next, jerr := paginate(out.Resources, func(r *mcp.Resource) string { return r.URI },
		"resources", cursor, g.pageSize, principal)
	if jerr != nil {
		return nil, jerr
	}
	out.Resources, out.NextCursor = page, next
	evtItems(evt, len(out.Resources))
	return out, nil
}

func (g *Gateway) listResourceTemplates(ctx context.Context, rt *routes, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	ups := visibleUpstreams(ctx, rt.upstreams)
	lists, failed := fanOut(ctx, ups, func(ctx context.Context, u *upstream) ([]*mcp.ResourceTemplate, error) {
		return u.listResourceTemplates(ctx)
	})
	meta, err := partialFailureMeta(failed, len(ups))
	if err != nil {
		return nil, err
	}
	principal := auth.PrincipalFromContext(ctx)
	out := &mcp.ListResourceTemplatesResult{ResourceTemplates: []*mcp.ResourceTemplate{}, Meta: meta}
	filter := newListFilter(principal, rt.policy, "resources/read")
	for i, u := range ups {
		for _, tpl := range lists[i] {
			if !filter.visible(u.cfg.ID, tpl.URITemplate) {
				continue
			}
			// Shared, never mutated — see listResources.
			out.ResourceTemplates = append(out.ResourceTemplates, tpl)
		}
	}
	filter.finish(g, evt, &out.Meta)
	cursor := ""
	if p, ok := req.GetParams().(*mcp.ListResourceTemplatesParams); ok && p != nil {
		cursor = p.Cursor
	}
	page, next, jerr := paginate(out.ResourceTemplates, func(t *mcp.ResourceTemplate) string { return t.URITemplate },
		"resourceTemplates", cursor, g.pageSize, principal)
	if jerr != nil {
		return nil, jerr
	}
	out.ResourceTemplates, out.NextCursor = page, next
	evtItems(evt, len(out.ResourceTemplates))
	return out, nil
}

func (g *Gateway) readResource(ctx context.Context, rt *routes, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.ReadResourceParams)
	if !ok || params == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "missing resource uri"}
	}
	evt.Name = params.URI
	// Three branches below forward these params to an upstream — the minted
	// ui:// owner, the affinity hit, and the probe — so the connection-owned
	// `_meta` keys come off once, here, rather than at each of them. The copy
	// is made only when there is something to strip, and the caller's own
	// params object is left alone for audit and the decision hook.
	if sanitized := sanitizeRequestMeta(params.Meta); len(sanitized) != len(params.Meta) {
		forwarded := *params
		forwarded.Meta = sanitized
		params = &forwarded
	}
	principal := auth.PrincipalFromContext(ctx)
	tn := tenantFrom(ctx)
	denied := false

	// A minted ui:// URI names its owner, so it needs neither the affinity
	// index below nor the probe after it. Policy and the tenant subset are
	// evaluated against the upstream's own URI, the way a tool is authorized
	// on its bare name — a rule written for the upstream keeps working, and a
	// name fold invented never appears in one.
	if u, original, ok := g.uiOwner(rt, params.URI); ok {
		evt.Upstream = u.cfg.ID
		if !tn.sees(u.cfg.ID) {
			evt.Decision, evt.Outcome = "deny", audit.OutcomeDenied
			return nil, errOutsideSubset(tn, "resources/read", original, u.cfg.ID)
		}
		if !rt.policy.Decide(principal, u.cfg.ID, "resources/read", original).Allowed {
			evt.Decision, evt.Outcome = "deny", audit.OutcomeDenied
			return nil, &jsonrpc.Error{Code: codeDenied, Message: fmt.Sprintf("policy denied resources/read %q", params.URI)}
		}
		evt.Decision = "allow"
		// Unlike the affinity path, a failure here is the answer: the URI
		// identified one upstream and that upstream refused, so falling back to
		// probing the others would be asking servers that cannot own it.
		out, err := u.readResource(ctx, &mcp.ReadResourceParams{URI: original, Meta: params.Meta})
		if err != nil {
			return nil, err
		}
		mintReadResult(out, original, params.URI)
		tagUpstream(&out.Meta, u)
		return out, nil
	}

	// Affinity first: route to the upstream the URI was listed from.
	if id, ok := g.resourceOwner.Load(params.URI); ok {
		if u := rt.byID[id]; u != nil {
			evt.Upstream = u.cfg.ID
			// The tenant subset is the coarser cut, so it is read first: an
			// upstream outside it is refused here rather than probed for,
			// exactly as a policy denial would be.
			if !tn.sees(u.cfg.ID) {
				evt.Decision, evt.Outcome = "deny", audit.OutcomeDenied
				return nil, errOutsideSubset(tn, "resources/read", params.URI, u.cfg.ID)
			}
			if !rt.policy.Decide(principal, u.cfg.ID, "resources/read", params.URI).Allowed {
				evt.Decision, evt.Outcome = "deny", audit.OutcomeDenied
				return nil, &jsonrpc.Error{Code: codeDenied, Message: fmt.Sprintf("policy denied resources/read %q", params.URI)}
			}
			evt.Decision = "allow"
			out, err := u.readResource(ctx, params)
			if err == nil {
				tagUpstream(&out.Meta, u)
				return out, nil
			}
		}
	}
	// Probe fallback: try only the upstreams this principal may read from, so
	// URI guessing cannot reach an upstream the caller has no grant on. The
	// tenant's subset bounds the probe set the same way.
	//
	// Unlike the affinity path above, a miss here stays a miss: with no
	// ownership record fold does not know the URI exists, so "not found" is
	// what it actually knows, and reporting a denial instead would answer
	// every typo from a tenanted caller with a refusal.
	for _, u := range visibleUpstreams(ctx, rt.upstreams) {
		if !rt.policy.Decide(principal, u.cfg.ID, "resources/read", params.URI).Allowed {
			denied = true
			continue
		}
		out, err := u.readResource(ctx, params)
		if err == nil {
			g.resourceOwner.Store(params.URI, u.cfg.ID, 0)
			evt.Upstream = u.cfg.ID
			evt.Decision = "allow"
			tagUpstream(&out.Meta, u)
			return out, nil
		}
	}
	if denied {
		evt.Decision, evt.Outcome = "deny", audit.OutcomeDenied
		return nil, &jsonrpc.Error{Code: codeDenied, Message: fmt.Sprintf("policy denied resources/read %q", params.URI)}
	}
	return nil, mcp.ResourceNotFoundError(params.URI)
}

// bridgeFor returns the per-client session key and a way to build the bridge
// options for a request, so server-initiated traffic (sampling, elicitation,
// logging, progress) flows back to the calling client.
//
// The options are returned as a thunk rather than built here because only the
// connect path reads them: a named invocation against an already-open bridged
// session never touches them, and in steady state that is nearly all of them.
// The thunk may run more than once (once per upstream that must connect on a
// logging/setLevel fan-out); each call builds an equivalent value, since the
// options depend only on the downstream session.
func (g *Gateway) bridgeFor(req mcp.Request, u *upstream) (string, bridgeOpts) {
	ss, ok := req.GetSession().(*mcp.ServerSession)
	if !ok || ss.ID() == "" {
		return "", nil
	}
	return ss.ID(), func() *mcp.ClientOptions { return g.bridgeOptions(ss, u) }
}

// bridgeKey is the downstream session a bridged session would be keyed by, or
// "" when this request has none. Split out for the one caller that bridges to
// several upstreams at once and needs the key before it has an upstream: the
// options differ per upstream now that they carry a policy decision about it.
func bridgeKey(req mcp.Request) string {
	ss, ok := req.GetSession().(*mcp.ServerSession)
	if !ok {
		return ""
	}
	return ss.ID()
}

// complete routes completion/complete to the upstream owning the reference:
// prompt refs resolve by namespace, resource refs by URI ownership.
func (g *Gateway) complete(ctx context.Context, rt *routes, req mcp.Request, evt *audit.Event) (mcp.Result, error) {
	params, ok := req.GetParams().(*mcp.CompleteParams)
	if !ok || params == nil || params.Ref == nil {
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "missing completion ref"}
	}
	// Completion is a sub-capability of the reference it completes: gate a
	// prompt-argument completion behind prompts/get and a resource
	// completion behind resources/read, so it cannot be used to enumerate
	// values on a reference the caller may not otherwise reach.
	var u *upstream
	var method, name string
	forwarded := *params
	forwarded.Meta = sanitizeRequestMeta(params.Meta)
	switch params.Ref.Type {
	case "ref/prompt":
		evt.Name = params.Ref.Name
		up, bare, err := g.resolve(rt, params.Ref.Name)
		if err != nil {
			return nil, err
		}
		u, method, name = up, "prompts/get", bare
		ref := *params.Ref
		ref.Name = bare
		forwarded.Ref = &ref
	case "ref/resource":
		evt.Name = params.Ref.URI
		up, err := g.ownerForURI(ctx, rt, params.Ref.URI)
		if err != nil {
			return nil, err
		}
		u, method, name = up, "resources/read", params.Ref.URI
	default:
		return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown completion ref type %q", params.Ref.Type)}
	}
	evt.Upstream = u.cfg.ID
	if res, err := g.authorize(ctx, evt, rt, u, method, name); err != nil {
		return res, err
	}
	out, err := u.complete(ctx, &forwarded)
	if err != nil {
		return nil, err
	}
	// The one named-invocation path that does not tag its result, and so does
	// not get the reverse-direction strip from tagUpstream. It still needs it:
	// the SDK stamps fold's own identity onto a result only when the key is
	// absent, so an upstream's serverInfo left in place would both reach the
	// caller and suppress fold's. Stripped rather than tagged, because adding
	// an upstream marker to completion results would be new caller-visible
	// metadata rather than a fix.
	stripResultMeta(&out.Meta)
	return out, nil
}

// setLevel propagates the client's logging level to its bridged upstream
// sessions (best effort — not every upstream supports logging), then lets
// the SDK server record it for the downstream session.
func (g *Gateway) setLevel(ctx context.Context, rt *routes, req mcp.Request, next mcp.MethodHandler) (mcp.Result, error) {
	params, _ := req.GetParams().(*mcp.SetLoggingLevelParams)
	key := bridgeKey(req)
	if params != nil && key != "" {
		// Only the tenant's own upstreams: propagating a level to one it
		// cannot see would open a bridged session to it on this client's
		// behalf, which is the thing the subset exists to prevent.
		for _, u := range visibleUpstreams(ctx, rt.upstreams) {
			_, opts := g.bridgeFor(req, u)
			_ = u.setLoggingLevel(ctx, key, opts, params.Level) // best effort per upstream
		}
	}
	return next(ctx, "logging/setLevel", req)
}

// authorize applies the policy engine to a named invocation. Denials return
// a policy error; the audit event records the decision either way.
// rawArgs builds the lazy argument source for an invocation, or nil when no
// rule in the running policy constrains arguments — which is every document
// written before `args` existed. The closure memoizes, so a policy that
// consults the arguments twice still unmarshals once, and a policy that never
// consults them leaves the raw JSON untouched on its way upstream.
func rawArgs(rt *routes, raw json.RawMessage) policy.ArgSource {
	if !rt.policy.NeedsArguments() || len(raw) == 0 {
		return nil
	}
	var (
		once   sync.Once
		parsed map[string]any
		ok     bool
	)
	return func() (map[string]any, bool) {
		once.Do(func() {
			ok = json.Unmarshal(raw, &parsed) == nil
		})
		return parsed, ok
	}
}

func (g *Gateway) authorize(ctx context.Context, evt *audit.Event, rt *routes, u *upstream, method, bare string) (mcp.Result, error) {
	return g.authorizeCall(ctx, evt, rt, u, method, bare, policy.Evidence{})
}

func (g *Gateway) authorizeCall(ctx context.Context, evt *audit.Event, rt *routes, u *upstream, method, bare string, ev policy.Evidence) (mcp.Result, error) {
	// The tenant's subset is evaluated before policy: it bounds which
	// upstreams exist for this caller at all, and policy then decides what
	// may be invoked among them. Nothing outside the subset reaches the
	// engine, so a rule written for another tenant's upstream cannot admit
	// this one.
	if tn := tenantFrom(ctx); !tn.sees(u.cfg.ID) {
		evt.Decision, evt.Outcome = "deny", audit.OutcomeDenied
		return nil, errOutsideSubset(tn, method, evt.Name, u.cfg.ID)
	}
	d := rt.policy.DecideCall(auth.PrincipalFromContext(ctx), u.cfg.ID, method, bare, ev)
	evt.RuleID = d.RuleID
	if d.Allowed {
		evt.Decision = "allow"
		return nil, nil
	}
	evt.Decision = "deny"
	evt.Outcome = audit.OutcomeDenied
	msg := fmt.Sprintf("policy denied %s %q", method, evt.Name)
	if d.FailedArg != "" {
		// The path, never the expected value: what the caller sent is theirs
		// already, but what the rule wanted is the operator's configuration,
		// and a denial is a poor place to disclose it a field at a time.
		msg += fmt.Sprintf(": argument %q does not satisfy the grant", d.FailedArg)
	}
	err := &jsonrpc.Error{Code: codeDenied, Message: msg}
	if len(d.MissingScopes) > 0 {
		// Scopes are the one thing a denial *can* name, and the exception is
		// principled rather than convenient: unlike an argument's expected
		// value, a scope is a credential the caller goes and obtains, so
		// naming it is telling them how to succeed rather than describing the
		// operator's configuration. The engine has already bounded what
		// appears here — only scopes this caller lacks, and only from a rule
		// that would otherwise have granted this exact invocation.
		evt.MissingScopes = d.MissingScopes
		msg += fmt.Sprintf(": requires scope %s", strings.Join(d.MissingScopes, ", "))
		err.Message = msg
		data, mErr := json.Marshal(map[string]any{"missingScopes": d.MissingScopes})
		if mErr == nil {
			err.Data = data
		}
	}
	return nil, err
}

// directionServerInitiated marks the audit events that describe the reverse
// path. Absent means client-to-upstream — see audit.Event.Direction.
const directionServerInitiated = "server_initiated"

// authorizeServerInitiated is authorize's reverse-path twin: it decides
// whether u may ask the caller's client for something (sampling, elicitation)
// and returns a func that records how the exchange ended.
//
// Two differences from the forward path are structural rather than
// incidental. The refusal is answered to the upstream, not to the caller — the
// caller sees only whatever the tool does about being told no — which makes
// the audit event the one place this exchange is legible. And the event is
// built here rather than in the middleware, because the middleware wraps
// client requests and this is not one: it arrives on an SDK client goroutine,
// inside a call the middleware is already in the middle of.
func (g *Gateway) authorizeServerInitiated(ctx context.Context, u *upstream, method string, params any) (func(error), error) {
	start := time.Now()
	evt := audit.Event{
		Method:    method,
		Direction: directionServerInitiated,
		Upstream:  u.cfg.ID,
	}
	p := auth.PrincipalFromContext(ctx)
	if p != nil {
		evt.Principal, evt.Issuer = p.Subject, p.Issuer
	}
	evt.Tenant = tenantFrom(ctx).id()
	d := g.rt().policy.DecideServerInitiated(p, u.cfg.ID, method)
	evt.RuleID = d.RuleID
	if !d.Allowed {
		evt.Decision, evt.Outcome = "deny", audit.OutcomeDenied
		evt.LatencyMs = time.Since(start).Milliseconds()
		g.audit.Emit(evt)
		return nil, &jsonrpc.Error{
			Code:    codeDenied,
			Message: fmt.Sprintf("policy denied %s", method),
		}
	}
	evt.Decision = "allow"
	// Policy has allowed it structurally; the hook is where a content
	// question gets asked, which is what design-server-initiated.md deferred
	// here. "Refuse an elicitation that asks for a password" is this call.
	if err := g.hookServerInitiated(ctx, &evt, u, method, params); err != nil {
		evt.LatencyMs = time.Since(start).Milliseconds()
		g.audit.Emit(evt)
		return nil, err
	}
	// The event waits for the answer: an allowed request is not a terminal
	// response until the client has given one, and how long the caller's
	// human took is the most interesting number on an elicitation.
	return func(err error) {
		evt.LatencyMs = time.Since(start).Milliseconds()
		if err != nil {
			evt.Error, evt.Outcome = err.Error(), classify(err)
		} else {
			evt.Outcome = audit.OutcomeOK
		}
		g.audit.Emit(evt)
	}, nil
}

// tagUpstream marks a proxied result with the upstream that served it. It is
// also where the reverse-direction connection key is dropped: every path that
// returns an upstream's own result meta passes through here, so the strip has
// one home rather than four.
func tagUpstream(meta *mcp.Meta, u *upstream) {
	if *meta == nil {
		*meta = mcp.Meta{}
	}
	stripResultMeta(meta)
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

// evtItems records how many items a list handed this caller, after policy
// filtering and pagination — the surface the caller actually received, not the
// federation's total.
func evtItems(evt *audit.Event, n int) {
	if evt != nil {
		evt.ItemsServed = n
	}
}

// metaUsageKey is the result `_meta` key an upstream sets to report its own
// consumption. fold carries whatever it finds there verbatim and never
// synthesizes it — an absent key means the upstream reported nothing, not that
// nothing was consumed.
const metaUsageKey = "usage"

// meterKey carries the per-request consumption counter.
type meterKey struct{}

// meter counts what one downstream request cost. A federated list fans out to
// every upstream, so the count is the request's real price rather than the one
// call the client made.
type meter struct {
	upstreamCalls atomic.Int64
	usage         atomic.Pointer[map[string]any]
}

// withMeter attaches a fresh counter to ctx.
func withMeter(ctx context.Context) (context.Context, *meter) {
	m := &meter{}
	return context.WithValue(ctx, meterKey{}, m), m
}

// meterFrom returns the request's counter, or nil outside a metered request.
func meterFrom(ctx context.Context) *meter {
	m, _ := ctx.Value(meterKey{}).(*meter)
	return m
}

// countUpstreamCall records one upstream invocation against the request.
func countUpstreamCall(ctx context.Context) {
	if m := meterFrom(ctx); m != nil {
		m.upstreamCalls.Add(1)
	}
}

// noteUsage records counters an upstream published in a result's `_meta`.
// Last writer wins: a fan-out reporting usage from several upstreams is not a
// case fold can merge without inventing arithmetic over units it does not
// define, so only named invocations — which route to exactly one upstream —
// carry usage through.
func noteUsage(ctx context.Context, meta mcp.Meta) {
	if len(meta) == 0 {
		return
	}
	raw, ok := meta[metaUsageKey]
	if !ok {
		return
	}
	vals, ok := raw.(map[string]any)
	if !ok || len(vals) == 0 {
		return
	}
	if m := meterFrom(ctx); m != nil {
		m.usage.Store(&vals)
	}
}
