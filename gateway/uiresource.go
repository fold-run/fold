package gateway

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP Apps (SEP-1865) hangs an interactive HTML interface off a tool by
// pointing at a resource in the tool's `_meta.ui`. The extension requires
// those URIs to be unique only *within one server*, and the published starter
// templates ship URIs with no server segment at all — so two upstreams built
// from the same template advertise the same URI, and a gateway that federates
// them has two different resources with one name.
//
// Every other resource URI is opaque and passes through fold untouched, which
// is the rule this file is the single exception to. It is a narrow one:
//
//   - only the `ui://` scheme is rewritten, so no URI a client persists for
//     any other purpose is affected;
//   - the minted form is derived from the namespace the operator already
//     chose, so it is stable across restarts, reloads, and gateway instances;
//   - and the URI is not an identifier the client stores — it is republished
//     in `_meta.ui` on every `tools/list`, which is what makes rewriting it
//     safe where rewriting a real resource URI would not be.
//
// Without it, `resources/read` of a colliding URI resolves by whichever
// upstream last listed it — so which vendor's interface a host renders depends
// on what some other client did first, and one of the two apps is always
// wrong.
const (
	uiScheme = "ui://"
	// uiMintHost is the authority fold mints under. A client only ever sees
	// this form for a resource fold itself published in a tool's metadata.
	uiMintHost = "fold"
)

// isUIURI reports whether uri belongs to the MCP Apps interface scheme.
func isUIURI(uri string) bool { return strings.HasPrefix(uri, uiScheme) }

// mintUIURI rewrites an upstream's ui:// URI into the federation-scoped form
// clients see: ui://fold/{namespace}/{rest}. Anything else — another scheme,
// or an upstream running passthrough, where names are not rewritten either —
// is returned unchanged.
func (u *upstream) mintUIURI(uri string) string {
	if u.cfg.Namespace == "" || !isUIURI(uri) {
		return uri
	}
	return uiScheme + uiMintHost + "/" + u.cfg.Namespace + "/" + uri[len(uiScheme):]
}

// splitUIURI reverses mintUIURI. ok is false for every URI fold did not mint,
// including a raw ui:// URI that reached a client some other way — those keep
// their existing behaviour on the read path rather than being guessed at.
func splitUIURI(uri string) (namespace, original string, ok bool) {
	const prefix = uiScheme + uiMintHost + "/"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", false
	}
	ns, rest, found := strings.Cut(uri[len(prefix):], "/")
	if !found || ns == "" || rest == "" {
		return "", "", false
	}
	return ns, uiScheme + rest, true
}

// uiOwner resolves a minted ui:// URI to the upstream that published it and
// the URI that upstream knows it by. Unlike the affinity index this replaces,
// the answer comes from the URI itself: no probe, no fan-out, and no
// dependence on which lists a client happened to fetch first.
func (g *Gateway) uiOwner(rt *routes, uri string) (*upstream, string, bool) {
	if rt.passthrough {
		return nil, "", false
	}
	ns, original, ok := splitUIURI(uri)
	if !ok {
		return nil, "", false
	}
	u := rt.byNamespace[ns]
	if u == nil {
		return nil, "", false
	}
	return u, original, true
}

// Tool metadata keys the extension defines. The flat form is deprecated by the
// specification but still emitted alongside the nested one by the TypeScript
// SDK, and a host is free to read either — so fold rewrites both or neither.
const (
	metaUI             = "ui"
	metaUIResourceURI  = "resourceUri"
	metaUIResourceFlat = "ui/resourceUri"
)

// mintToolMeta returns m with any ui:// resource pointer rewritten to its
// minted form, copying only when there is something to rewrite. The input is
// shared with the cached list and read-only (see cachedList), which is why the
// nested map is copied rather than edited in place.
func (u *upstream) mintToolMeta(m mcp.Meta) mcp.Meta {
	if len(m) == 0 || u.cfg.Namespace == "" {
		return m
	}
	nested, _ := m[metaUI].(map[string]any)
	uri, _ := nested[metaUIResourceURI].(string)
	flat, _ := m[metaUIResourceFlat].(string)
	if !isUIURI(uri) && !isUIURI(flat) {
		return m
	}
	out := make(mcp.Meta, len(m))
	maps.Copy(out, m)
	if isUIURI(uri) {
		sub := make(map[string]any, len(nested))
		maps.Copy(sub, nested)
		sub[metaUIResourceURI] = u.mintUIURI(uri)
		out[metaUI] = sub
	}
	if isUIURI(flat) {
		out[metaUIResourceFlat] = u.mintUIURI(flat)
	}
	return out
}

// namespacedResources returns the resource list as clients see it, with ui://
// URIs minted. It is namespacedTools for resources, with one difference: a
// federation typically has no ui:// resources at all, so the common answer is
// the input slice itself — memoized like the others, so the scan that decides
// that happens once per cache generation rather than once per request.
func (u *upstream) namespacedResources(ctx context.Context, bare []*mcp.Resource) []*mcp.Resource {
	if u.cfg.Namespace == "" {
		return bare
	}
	profile := capProfileFrom(ctx)
	u.publicMu.Lock()
	defer u.publicMu.Unlock()
	if items, ok := u.publicResources[profile].get(bare); ok {
		return items
	}
	items := bare
	for i, r := range bare {
		if !isUIURI(r.URI) {
			continue
		}
		if len(items) == 0 || &items[0] == &bare[0] {
			items = slices.Clone(bare) // copy on first rewrite, not before
		}
		nr := *r
		nr.URI = u.mintUIURI(r.URI)
		items[i] = &nr
	}
	if u.publicResources == nil {
		u.publicResources = map[capProfile]publicView[mcp.Resource]{}
	}
	u.publicResources[profile] = publicView[mcp.Resource]{from: bare, items: items}
	return items
}

// mintReadResult rewrites the URIs a read answered with back into the minted
// form the caller asked with, so a client never sees a URI it cannot use
// again. The result is freshly received from the upstream and not shared, so
// it is edited in place.
func mintReadResult(out *mcp.ReadResourceResult, original, minted string) {
	for _, c := range out.Contents {
		if c != nil && c.URI == original {
			c.URI = minted
		}
	}
}
