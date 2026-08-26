package gateway

import (
	"context"
	"net/http"
	"slices"
	"strings"
)

// mcpParamPrefix is the header namespace a client uses to mirror selected tool
// arguments into HTTP headers, so an intermediary can route on them without
// parsing the JSON-RPC body.
const mcpParamPrefix = "Mcp-Param-"

// The streamable HTTP transport addresses intermediaries by name here:
//
//	Intermediate servers that do not recognize an `Mcp-Param-{Name}` header
//	MUST forward it and otherwise ignore it, as required by the HTTP
//	Semantics RFC.
//
// fold is such an intermediary and was dropping them. Its upstream leg is a
// fresh SDK client session whose headers the SDK generates, so nothing a
// caller sent survived the hop — verified against a real upstream, which saw
// neither the caller's `Mcp-Param-*` nor any other header it had set.
//
// "and otherwise ignore it" is the half that decides the shape of this. fold
// does not read these values, does not route on them, and does not let them
// influence anything it decides: the body remains the only thing fold parses,
// which is what keeps the routing headers a hint rather than a second,
// forgeable control surface. They are relayed because the caller and the
// upstream have an agreement fold is not party to.
//
// Deliberately narrow. fold does not forward arbitrary client headers — that
// is a standing design decision, not an omission, and the reason a caller
// cannot smuggle a credential or an identity assertion to an upstream through
// one. This is the single namespace a specification requires be passed
// through, and it inherits the same host bound as every other thing fold
// attaches: nothing is relayed to a host outside the upstream's configured
// endpoints.
//
// Two measured consequences, recorded because neither is obvious from the code.
//
// The headers ride the request *context*, so every upstream request made under
// one carries them — including the bridged session's own initialize and
// initialized, which are issued lazily inside the first named invocation and
// inherit that call's headers. Harmless as the specification is written, since
// a server validates a param header only for a tools/call naming a tool that
// annotates it. But a call-scoped value is landing on a handshake, and an
// intermediary routing on it there would pin a whole session using a value
// scoped to one call.
//
// And on the default configuration nothing validates these at all. The
// upstream's own server checks header against body only once it has negotiated
// 2026-07-28, and fold's default upstream protocol pins the leg below that. So
// "fold ignores them" is true of fold and not of the deployment: an upstream
// that routes on these must validate them against the body itself, because
// neither end of fold's hop does.

type paramHeaderKey struct{}

// withParamHeaders lifts the caller's Mcp-Param-* headers onto the context so
// the upstream transport can replay them.
//
// Nothing is copied when there are none, which is every request today and
// every request from a client that does not use the mechanism: the loop reads
// the header map and allocates only once it has found one to keep.
func withParamHeaders(ctx context.Context, hdr http.Header) context.Context {
	if len(hdr) == 0 {
		return ctx
	}
	var kept http.Header
	for name, values := range hdr {
		// textproto canonicalizes on the way in, so a prefix comparison is
		// enough; the fold is belt-and-braces for a hand-built header map.
		if !strings.HasPrefix(name, mcpParamPrefix) &&
			!strings.HasPrefix(http.CanonicalHeaderKey(name), mcpParamPrefix) {
			continue
		}
		if kept == nil {
			kept = make(http.Header, 2)
		}
		// Cloned rather than aliased: this slice belongs to the inbound
		// request, and an Add on the outgoing copy could otherwise append
		// into its backing array. Free on the common path, which never
		// reaches here.
		kept[http.CanonicalHeaderKey(name)] = slices.Clone(values)
	}
	if kept == nil {
		return ctx
	}
	return context.WithValue(ctx, paramHeaderKey{}, kept)
}

// injectParamHeaders replays the caller's Mcp-Param-* headers onto an outgoing
// upstream request. It never overwrites a header the SDK client set for
// itself, and the reason is stronger than "the client's is fresher": the
// specification requires the header to *agree with the body*, and only a value
// fold's own client derived from the body it is about to send is guaranteed to.
// Letting the caller's copy win would let a caller supply a header that
// disagrees with the request fold authorized, and a conforming upstream would
// then reject its own call.
//
// The branch is currently unreachable through the gateway: the SDK mints these
// headers from a tool schema cached on the session that listed it, fold lists
// on the root session and invokes on a bridged one, so the invoking session has
// no schema to mint from. It is written for the day that stops being true.
func injectParamHeaders(ctx context.Context, hdr http.Header) {
	kept, ok := ctx.Value(paramHeaderKey{}).(http.Header)
	if !ok {
		return
	}
	for name, values := range kept {
		if _, exists := hdr[name]; exists {
			continue
		}
		hdr[name] = values
	}
}
