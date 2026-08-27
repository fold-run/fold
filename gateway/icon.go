package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/internal/bounded"
)

// MCP lets a server hang an icon off a tool, prompt, resource, or resource
// template, and it tells the *client* how to handle one: fetch only https or
// data:, reject unsafe schemes and cross-origin redirects, fetch without
// credentials, sniff the bytes rather than trust the declared type — and
// "verify that icon URIs are from the same origin as the server."
//
// That last rule is what makes an icon a federation problem. An upstream's
// icon points at the upstream's origin, which behind a gateway is neither the
// origin the client connected to nor, for the cluster-internal upstream that
// is the common enterprise case, an origin the client can reach at all. So a
// conforming client rejects every federated icon and a lenient one fails to
// load it. fold looks like a server whose upstreams have no icons, which is
// the invisibility rule broken in the direction nobody reports.
//
// The fix is the one uiresource.go already made for the MCP Apps ui:// scheme,
// and it is safe for the same reason: an icon src is not an identifier a
// client persists — it is republished on every list — so rewriting it costs
// nothing a client relies on. fold mints its own URL for each icon and serves
// the bytes itself, from the origin the client already trusts.
//
// Four things bound it, and they are the reason this is not an open proxy:
//
//   - only an icon whose host is one of that upstream's own configured
//     endpoints is minted, so fold only ever fetches where the operator
//     already pointed it. Anything else is republished untouched — already a
//     public URL, and dropping it would only take an icon away from a client
//     lenient enough to have rendered it;
//   - unsafe schemes (javascript:, file:, ftp:, ws:) are dropped rather than
//     republished, because putting fold's name on one is fold vouching for it;
//   - data: URIs pass through — already same-origin-safe and already
//     reachable, so proxying would only mean re-serving bytes fold is holding;
//   - passthrough mints nothing at all, exactly like mintUIURI: an
//     un-namespaced single upstream is not a federation and has no collision
//     to resolve.
//
// The minted form is {publicBase}/icons/{namespace}/{digest}. It resolves the
// way uiOwner does, from the URL itself — the namespace names the upstream and
// the digest indexes its own list — so there is no global index to consult and
// no probe. The digest is over the upstream's src, which makes it stable
// across a fleet: an instance that never minted a URL can still resolve one
// another instance handed out.
const (
	iconPathPrefix = "/icons/"
	// iconDigestLen is the hex width of the digest segment: 128 bits of
	// SHA-256, wide enough that the mapping is injective in practice and
	// short enough to keep a list of icon URLs small.
	iconDigestLen = 32
)

// iconDigest names an icon by its upstream source. Deterministic across
// processes and instances, which is what lets any instance in a fleet resolve
// a URL minted by any other.
func iconDigest(src string) string {
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:iconDigestLen/2])
}

// iconDisposition classifies one icon src.
type iconDisposition int

const (
	iconPassThrough iconDisposition = iota // republished exactly as sent
	iconMint                               // rewritten to fold's origin
	iconDrop                               // removed: fold will not vouch for it
)

// classifyIcon decides what happens to one src. The host check is the same
// bound credential attachment uses — nothing rides to a host the upstream did
// not configure — applied here so an off-origin icon is never minted, and
// again before the fetch.
func (u *upstream) classifyIcon(src string) iconDisposition {
	switch {
	case src == "":
		return iconDrop
	case strings.HasPrefix(src, "data:"):
		return iconPassThrough
	}
	scheme, rest, ok := strings.Cut(src, "://")
	if !ok {
		// No authority at all: a relative reference, or an opaque scheme like
		// javascript:. Neither is a URI a client may fetch, and MCP defines no
		// base for a client to resolve a relative one against.
		return iconDrop
	}
	if scheme != "http" && scheme != "https" {
		return iconDrop
	}
	host, _, _ := strings.Cut(rest, "/")
	if !u.hosts[host] {
		return iconPassThrough
	}
	return iconMint
}

// mintIcons returns the icon list as clients see it. The input is shared with
// the cached list and read-only (see cachedList) — and more sharply so than
// elsewhere, because Icons is a slice of *values*: a shallow struct copy of
// the tool shares this backing array, so writing through it would corrupt the
// entry every other request reads, and with Redis, the fleet's. Hence the
// clone, and hence the common case — a federation with no icons at all —
// returning the input slice untouched.
func (u *upstream) mintIcons(icons []mcp.Icon) []mcp.Icon {
	if len(icons) == 0 || u.cfg.Namespace == "" || u.iconBase == "" {
		return icons
	}
	out := icons
	cloned := false
	drops := 0
	for i, icon := range icons {
		switch u.classifyIcon(icon.Source) {
		case iconPassThrough:
			continue
		case iconDrop:
			drops++
		case iconMint:
			if !cloned {
				out = slices.Clone(icons)
				cloned = true
			}
			digest := iconDigest(icon.Source)
			u.iconIndex.Store(digest, icon.Source, u.iconIndexTTL)
			out[i].Source = u.iconBase + iconPathPrefix + u.cfg.Namespace + "/" + digest
		}
	}
	if drops == 0 {
		return out
	}
	// A dropped icon is the one place this removes data, so it is done in a
	// second pass over the already-decided list rather than mixed into the
	// rewrite, where an index shift would be easy to get wrong.
	kept := make([]mcp.Icon, 0, len(out)-drops)
	for i, icon := range out {
		if u.classifyIcon(icons[i].Source) == iconDrop {
			continue
		}
		kept = append(kept, icon)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// splitIconPath reverses the minted path. ok is false for anything fold did
// not mint, which is answered 404 rather than guessed at — the same posture
// splitUIURI takes.
func splitIconPath(path string) (namespace, digest string, ok bool) {
	if !strings.HasPrefix(path, iconPathPrefix) {
		return "", "", false
	}
	ns, rest, found := strings.Cut(path[len(iconPathPrefix):], "/")
	if !found || ns == "" || len(rest) != iconDigestLen {
		return "", "", false
	}
	if strings.ContainsAny(rest, "/?#") {
		return "", "", false
	}
	for _, c := range rest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", "", false
		}
	}
	return ns, rest, true
}

// iconOwner resolves a minted path to the upstream that published the icon
// and the src that upstream knows it by.
//
// The index is a bounded map, so an entry can be evicted; the miss path is
// what makes that safe. It re-runs the mint over the upstream's own cached
// lists — warm, since a client only holds this URL because it just listed —
// and looks again. That is the resourceOwner probe-fallback pattern, and it is
// also what makes an icon URL survive a reload that rebuilt its upstream.
func (g *Gateway) iconOwner(rt *routes, path string) (*upstream, string, bool) {
	if rt.passthrough {
		return nil, "", false
	}
	ns, digest, ok := splitIconPath(path)
	if !ok {
		return nil, "", false
	}
	u := rt.byNamespace[ns]
	if u == nil {
		return nil, "", false
	}
	if src, ok := u.iconIndex.Load(digest); ok {
		return u, src, true
	}
	if !u.reindexIcons() {
		return nil, "", false
	}
	src, ok := u.iconIndex.Load(digest)
	if !ok {
		return nil, "", false
	}
	return u, src, true
}

// iconIndexEntries bounds the digest→src index per upstream. Keyed by a digest
// of a string the upstream chooses, so it is bounded like every other such
// map; eviction costs a rebuild from the cached list, never an error.
const iconIndexEntries = 1024

// reindexIcons repopulates the index from whatever lists this upstream has
// cached, without fetching. It reports whether it had anything to work with:
// an upstream whose caching is disabled — a caller-derived credential, where
// one caller's list must never serve another — has no shared list to rebuild
// from and mints nothing in the first place.
func (u *upstream) reindexIcons() bool {
	if u.cacheTTL <= 0 || u.iconBase == "" || u.cfg.Namespace == "" {
		return false
	}
	u.publicMu.Lock()
	defer u.publicMu.Unlock()
	found := false
	record := func(icons []mcp.Icon) {
		for _, icon := range icons {
			if u.classifyIcon(icon.Source) != iconMint {
				continue
			}
			u.iconIndex.Store(iconDigest(icon.Source), icon.Source, u.iconIndexTTL)
			found = true
		}
	}
	for _, view := range u.publicTools {
		for _, t := range view.from {
			record(t.Icons)
		}
	}
	for _, view := range u.publicPrompts {
		for _, p := range view.from {
			record(p.Icons)
		}
	}
	for _, view := range u.publicResources {
		for _, r := range view.from {
			record(r.Icons)
		}
	}
	return found
}

// newIconIndex builds the per-upstream digest→src map.
func newIconIndex() *bounded.Map[string] { return bounded.New[string](iconIndexEntries) }
