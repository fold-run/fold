package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/fold-run/fold/internal/bounded"
)

// The serving half of icon.go: fold hands a client a URL on its own origin,
// and this is what answers it.
//
// Everything here is the specification's own advice to a consumer of icon
// metadata, applied by the gateway so that every client behind it gets it
// whether or not the client implements it:
//
//   - fetch without credentials. Not by policy but by construction: the
//     client used here carries a plain transport, so there is no code path
//     from an icon fetch to auth.UpstreamCredentials at all;
//   - refuse redirects outright. The spec asks consumers to disallow scheme
//     changes and cross-origin hops; a redirect is never a legitimate step in
//     fetching an icon, and allowing even a same-host one reopens the origin
//     question this whole file exists to close;
//   - bound the body, and refuse rather than truncate — half an image is a
//     response the upstream never sent, which is the rewriting fold declines
//     everywhere else;
//   - sniff the bytes. The declared type is advisory, so fold serves what the
//     magic bytes say and refuses on a mismatch.
//
// It is unauthenticated, and that is forced rather than chosen: the spec has
// clients fetch icons without credentials, and a browser <img> carries no
// bearer token, so an authenticated endpoint would serve nothing to the
// clients it exists for. What it discloses is bounded accordingly — the path
// names no tool, prompt, or resource, only a namespace and a digest, and the
// bytes are branding artwork identical for every caller. fold treats them as
// public data, the same class as the console's static assets and the
// .well-known documents. An operator who does not accept that sets
// server.icons.enabled to false. Note what this is *not*: policy filtering is
// preserved here by unguessability, not by authentication. A principal who
// cannot see a tool is never handed its icon URL, but a principal who guesses
// one gets the bytes — which is why the endpoint serves only images and never
// a name, a description, or a schema.

const (
	// iconCacheEntries bounds the fetched-bytes cache across the whole
	// federation. bounded.Map holds at most 2x this across generations, so
	// with a 256 KiB default cap the resident ceiling is ~128 MiB.
	iconCacheEntries = 256
)

// iconEntry is one cached icon body.
type iconEntry struct {
	contentType string
	body        []byte
	etag        string
}

// iconFlight collapses concurrent fetches for one icon. A page with twenty
// <img> tags pointing at the same icon must cost the upstream one request.
type iconFlight struct {
	mu      sync.Mutex
	waiting map[string]chan struct{}
}

func newIconFlight() *iconFlight { return &iconFlight{waiting: map[string]chan struct{}{}} }

// begin returns either a token to do the work (leader true) or a channel that
// closes when whoever is doing it finishes.
func (f *iconFlight) begin(key string) (leader bool, done chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ch, ok := f.waiting[key]; ok {
		return false, ch
	}
	ch := make(chan struct{})
	f.waiting[key] = ch
	return true, ch
}

func (f *iconFlight) finish(key string, ch chan struct{}) {
	f.mu.Lock()
	delete(f.waiting, key)
	f.mu.Unlock()
	close(ch)
}

// newIconClient builds the fetcher: no credentials, no redirects, no reuse of
// the upstream's own client (which carries credentialTransport).
func newIconClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConns:        32,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// sniffImageType identifies an image from its leading bytes. The allowlist is
// deliberately short and deliberately raster.
//
// image/svg+xml is absent, and its absence is the security decision in this
// file. Two arguments, either sufficient. fold would be serving the SVG from
// *fold's own origin*, where an SVG's embedded script runs same-origin with
// /console/, /api/federation, and /oauth/token — strictly worse than the
// upstream serving it, because the upstream's origin holds nothing of fold's.
// And structurally: SVG is XML and has no magic bytes, so a strict magic-byte
// allowlist — which is exactly what the specification asks a consumer to
// maintain — cannot admit it. Admitting it would mean trusting the declared
// content type or parsing the XML, and the second is the content inspection
// fold has a documented non-goal about. An upstream whose only icon is an SVG
// therefore renders as no icon; that is in README "Not implemented".
func sniffImageType(b []byte) (string, bool) {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", true
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg", true
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp", true
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif", true
	}
	return "", false
}

// handleIcon answers a minted icon URL.
func (g *Gateway) handleIcon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rt := g.rt()
	u, src, ok := g.iconOwner(rt, r.URL.Path)
	if !ok {
		// Unknown namespace, unknown digest, or a URL minted against a list
		// generation that has since rolled. A client holding a stale URL will
		// re-list and get a fresh one, so this is the correct answer rather
		// than something to repair.
		http.NotFound(w, r)
		return
	}
	key := u.cfg.ID + "\x00" + iconDigest(src)

	entry, ok := g.iconBytes.Load(key)
	if !ok {
		entry, ok = g.fetchIcon(r.Context(), u, src, key, w)
		if !ok {
			return // fetchIcon has written the response
		}
	}

	h := w.Header()
	h.Set("Content-Type", entry.contentType)
	h.Set("X-Content-Type-Options", "nosniff")
	// Belt and braces: nothing on the allowlist executes, and if a future
	// format did, this would still not let it reach anything.
	h.Set("Content-Security-Policy", "default-src 'none'; sandbox")
	// MCP hosts are commonly Electron or web applications on a different
	// origin from the gateway, and without these two a browser blocks the
	// embed — the feature would be inert for exactly the clients it is for.
	// Safe because the endpoint is unauthenticated and the bytes are the same
	// for every caller.
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", "public, max-age="+strconv.Itoa(int(g.cfg.IconCacheTTL().Seconds())))
	h.Set("ETag", entry.etag)
	if match := r.Header.Get("If-None-Match"); match == entry.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(entry.body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(entry.body)
}

// fetchIcon retrieves and validates one icon, collapsing concurrent callers.
// It writes an error response and reports false when the icon cannot be
// served.
func (g *Gateway) fetchIcon(ctx context.Context, u *upstream, src, key string, w http.ResponseWriter) (iconEntry, bool) {
	leader, done := g.iconInflight.begin(key)
	if !leader {
		select {
		case <-done:
		case <-ctx.Done():
			http.Error(w, "cancelled", http.StatusGatewayTimeout)
			return iconEntry{}, false
		}
		if entry, ok := g.iconBytes.Load(key); ok {
			return entry, true
		}
		// The leader failed. Answer rather than queue behind a second
		// attempt: a failing upstream must not turn one page of <img> tags
		// into a retry storm against it.
		http.Error(w, "icon unavailable", http.StatusBadGateway)
		return iconEntry{}, false
	}
	// Leadership is held; check the cache once more before spending a fetch.
	// The caller's miss happened before this point, and in between another
	// request may have led a fetch to completion and published it — leaving
	// this goroutine to arrive after that flight ended, find the waiting map
	// empty, and lead a second fetch for bytes the gateway is already
	// holding. That is the amplification this mechanism exists to prevent.
	// The first Load is the fast path; this one is the correct one.
	if entry, ok := g.iconBytes.Load(key); ok {
		g.iconInflight.finish(key, done)
		return entry, true
	}
	entry, status := g.doFetchIcon(ctx, u, src)
	// Publish before waking the followers, or one of them can read the cache
	// in the window between the close and the store and conclude the leader
	// failed.
	if status == http.StatusOK {
		g.iconBytes.Store(key, entry, g.cfg.IconCacheTTL())
	}
	g.iconInflight.finish(key, done)
	if status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return iconEntry{}, false
	}
	return entry, true
}

// doFetchIcon performs the single outbound request.
func (g *Gateway) doFetchIcon(ctx context.Context, u *upstream, src string) (iconEntry, int) {
	// Re-check the host bound immediately before the request, not only at
	// mint time — the doubled check credentialTransport also makes, and the
	// reason a digest cannot be used to smuggle a fetch somewhere else.
	if u.classifyIcon(src) != iconMint {
		g.metrics.reject("icon_off_origin")
		return iconEntry{}, http.StatusNotFound
	}
	// Detached from the caller's context so one client hanging up does not
	// cancel the fetch every other waiter is about to read; the client's own
	// timeout still bounds it.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), g.cfg.IconTimeout())
	defer cancel()

	//nolint:gosec // G704: src is bounded to this upstream's own configured endpoint hosts by
	// classifyIcon, checked at mint time and again immediately above — the same host bound
	// credential attachment uses. An icon on any other host is never minted and never fetched.
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, src, nil)
	if err != nil {
		return iconEntry{}, http.StatusBadGateway
	}
	req.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/gif")
	resp, err := g.iconClient.Do(req) //nolint:gosec // G704: see the host bound above
	if err != nil {
		u.log.Debug("icon fetch failed", "error", err)
		g.metrics.reject("icon_upstream_error")
		return iconEntry{}, http.StatusBadGateway
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Includes every 3xx: CheckRedirect stops the client rather than
		// following, so a redirect arrives here as a non-2xx and is refused.
		g.metrics.reject("icon_upstream_error")
		return iconEntry{}, http.StatusBadGateway
	}

	maxBytes := g.cfg.IconMaxBytes()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		g.metrics.reject("icon_upstream_error")
		return iconEntry{}, http.StatusBadGateway
	}
	if int64(len(body)) > maxBytes {
		g.metrics.reject("icon_too_large")
		return iconEntry{}, http.StatusRequestEntityTooLarge
	}
	ct, ok := sniffImageType(body)
	if !ok {
		g.metrics.reject("icon_type_rejected")
		return iconEntry{}, http.StatusUnsupportedMediaType
	}
	sum := sha256.Sum256(body)
	return iconEntry{
		contentType: ct,
		body:        body,
		etag:        `"sha256-` + hex.EncodeToString(sum[:8]) + `"`,
	}, http.StatusOK
}

// newIconBytesCache builds the federation-wide byte cache.
func newIconBytesCache() *bounded.Map[iconEntry] { return bounded.New[iconEntry](iconCacheEntries) }
