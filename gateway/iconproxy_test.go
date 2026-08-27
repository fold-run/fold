package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync"
	"testing"

	"github.com/fold-run/fold/config"
)

// mintedIconURL federates one upstream and returns the gateway-origin URL its
// tool's icon was rewritten to.
func mintedIconURL(t *testing.T, up *iconUpstream, mutate func(*config.Config)) (string, *httptest.Server) {
	t.Helper()
	ts, _ := iconGateway(t, func(u string) *config.Config {
		cfg := &config.Config{
			Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
			Server:    &config.ServerSection{PublicURL: u, AllowedHosts: []string{"*"}},
		}
		if mutate != nil {
			mutate(cfg)
		}
		return cfg
	})
	session := connect(t, ts.URL, nil)
	icons := toolIcons(t, session, "a__search")
	if len(icons) != 1 {
		t.Fatalf("want one federated icon, got %+v", icons)
	}
	return icons[0].Source, ts
}

func getIcon(t *testing.T, url string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestIconServedFromGatewayOrigin(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, nil)

	res := getIcon(t, url, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("minted icon answered %s", res.Status)
	}
	body, _ := io.ReadAll(res.Body)
	if string(body) != iconPNG {
		t.Errorf("served %d bytes that are not the upstream's icon", len(body))
	}
	for header, want := range map[string]string{
		"Content-Type":                 "image/png",
		"X-Content-Type-Options":       "nosniff",
		"Cross-Origin-Resource-Policy": "cross-origin",
		"Access-Control-Allow-Origin":  "*",
	} {
		if got := res.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q — MCP hosts are commonly on a different origin from the "+
				"gateway, and without the cross-origin headers a browser blocks the embed, which "+
				"makes the whole feature inert for the clients it exists for", header, got, want)
		}
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Error("icon served with no CSP")
	}
	if res.Header.Get("ETag") == "" {
		t.Error("icon served with no ETag")
	}
}

func TestIconFetchCarriesNoCredentials(t *testing.T) {
	t.Setenv("ICON_UPSTREAM_KEY", "super-secret")
	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, func(cfg *config.Config) {
		// The MCP leg demonstrably does carry a credential, so the icon leg
		// not carrying one is a property of the fetcher rather than of the
		// fixture having none to send.
		cfg.Upstreams[0].Auth = &config.UpstreamAuth{Strategy: "static", SecretRef: "ICON_UPSTREAM_KEY"}
	})

	res := getIcon(t, url, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("icon answered %s", res.Status)
	}
	if res.Header.Get("X-Fold-Test-Credential") == "present" {
		t.Error("the icon fetch carried a credential. The specification is explicit — fetch icons " +
			"without credentials, no cookies, no Authorization — and fold's guarantee is structural: " +
			"the icon client carries a plain transport, so there is no path from here to " +
			"auth.UpstreamCredentials at all. Something reused the upstream's own http.Client.")
	}
}

func TestIconCachedAndSingleFlighted(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, nil)

	var wg sync.WaitGroup
	for range 25 {
		wg.Go(func() {
			res, err := http.Get(url)
			if err != nil {
				t.Error(err)
				return
			}
			defer res.Body.Close()
			_, _ = io.Copy(io.Discard, res.Body)
		})
	}
	wg.Wait()

	if n := up.hits.Load(); n != 1 {
		t.Errorf("25 concurrent requests for one icon caused %d upstream fetches, want 1: a page "+
			"with many <img> tags for one icon must not become many requests to the upstream", n)
	}
	getIcon(t, url, nil)
	if n := up.hits.Load(); n != 1 {
		t.Errorf("a later request re-fetched: %d total, want 1 (the bytes are cached for "+
			"server.icons.cacheTtlMs)", n)
	}
}

func TestIconConditionalGet(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, nil)

	first := getIcon(t, url, nil)
	etag := first.Header.Get("ETag")
	_, _ = io.Copy(io.Discard, first.Body)

	second := getIcon(t, url, map[string]string{"If-None-Match": etag})
	if second.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET answered %s, want 304", second.Status)
	}
	body, _ := io.ReadAll(second.Body)
	if len(body) != 0 {
		t.Errorf("304 carried %d bytes of body", len(body))
	}
}

func TestIconUnknownPaths(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, ts := mintedIconURL(t, up, nil)
	digest := url[strings.LastIndex(url, "/")+1:]

	for name, path := range map[string]string{
		"unknown namespace": iconPathPrefix + "nope/" + digest,
		"unknown digest":    iconPathPrefix + "a/" + strings.Repeat("0", iconDigestLen),
		"malformed":         iconPathPrefix + "a/short",
	} {
		res := getIcon(t, ts.URL+path, nil)
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("%s answered %s, want 404 — a client holding a URL from a list generation "+
				"that has rolled will re-list and get a fresh one", name, res.Status)
		}
	}
}

func TestIconMethodNotAllowed(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, nil)

	req, _ := http.NewRequest(http.MethodPost, url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /icons answered %s, want 405", res.Status)
	}
	if res.Header.Get("Allow") == "" {
		t.Error("405 carried no Allow header")
	}
}

func TestIconHeadServesNoBody(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, nil)

	req, _ := http.NewRequest(http.MethodHead, url, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "image/png" {
		t.Errorf("HEAD answered %s / %q", res.Status, res.Header.Get("Content-Type"))
	}
}

// --- the three security decisions ---------------------------------------

func TestIconRejectsSVG(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, nil)

	// Replace what the upstream serves at the icon path with an SVG.
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="fetch('/api/federation')"/>`)
	serveIconBytes(t, up, "image/svg+xml", svg)

	res := getIcon(t, url, nil)
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("an SVG icon answered %s, want 415. Two reasons it must be refused, either "+
			"sufficient: fold would be serving it from fold's own origin, where an SVG's script "+
			"runs same-origin with /console/ and /api/federation — strictly worse than the "+
			"upstream serving it. And SVG is XML with no magic bytes, so the strict magic-byte "+
			"allowlist the specification asks a consumer to maintain cannot admit it at all.",
			res.Status)
	}
}

func TestIconRejectsTypeMismatchByMagicBytes(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, nil)
	serveIconBytes(t, up, "image/png", []byte("<!DOCTYPE html><html><body>not an image</body></html>"))

	res := getIcon(t, url, nil)
	if res.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("HTML declared as image/png answered %s, want 415: the specification says to treat "+
			"the declared MIME type as advisory and detect the content type from the bytes", res.Status)
	}
}

func TestIconRefusesRedirect(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("fold followed a redirect while fetching an icon: the specification asks consumers " +
			"to disallow scheme changes and cross-origin redirects, and a redirect is never a " +
			"legitimate step in fetching an icon")
	}))
	t.Cleanup(target.Close)

	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, nil)
	serveIconFunc(t, up, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/elsewhere.png", http.StatusFound)
	})

	res := getIcon(t, url, nil)
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("a redirecting icon answered %s, want 502", res.Status)
	}
}

func TestIconOverSizeCapRefused(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, func(cfg *config.Config) {
		cfg.Server.Icons = &config.IconsSection{MaxBytes: 1 << 10}
	})
	serveIconBytes(t, up, "image/png", append([]byte(iconPNG), make([]byte, 4<<10)...))

	res := getIcon(t, url, nil)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized icon answered %s, want 413 — refuse rather than truncate, because "+
			"half an image is a response the upstream never sent", res.Status)
	}
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), iconPNG[:4]) {
		t.Error("a partial image was served")
	}
}

func TestIconUpstreamDown(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, _ := mintedIconURL(t, up, nil)
	serveIconFunc(t, up, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal upstream detail: db-primary-3.internal exploded", http.StatusInternalServerError)
	})

	res := getIcon(t, url, nil)
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("a failing icon fetch answered %s, want 502", res.Status)
	}
	body, _ := io.ReadAll(res.Body)
	if strings.Contains(string(body), "db-primary-3.internal") {
		t.Error("the icon endpoint relayed an upstream's internal error text to an unauthenticated caller")
	}
}

func TestIconEndpointNeedsNoBearer(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	url, ts := mintedIconURL(t, up, nil)

	res := getIcon(t, url, nil)
	if res.StatusCode != http.StatusOK {
		t.Errorf("the icon endpoint answered %s to an unauthenticated request. It is unauthenticated "+
			"by necessity rather than by choice: the specification has clients fetch icons without "+
			"credentials and a browser <img> carries no bearer token, so an authenticated endpoint "+
			"would serve nothing to the clients it exists for.", res.Status)
	}
	_ = ts
}

func TestIconsSurviveReload(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	var publicURL string
	base := func() *config.Config {
		return &config.Config{
			Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
			Server:    &config.ServerSection{PublicURL: publicURL, AllowedHosts: []string{"*"}},
		}
	}
	ts, gw := iconGateway(t, func(u string) *config.Config {
		publicURL = u
		return base()
	})
	session := connect(t, ts.URL, nil)
	url := toolIcons(t, session, "a__search")[0].Source

	t.Run("unrelated change keeps the URL live", func(t *testing.T) {
		cfg := base()
		cfg.Policy = &config.Policy{DefaultDecision: "allow"}
		if err := gw.Reload(cfg); err != nil {
			t.Fatal(err)
		}
		if res := getIcon(t, url, nil); res.StatusCode != http.StatusOK {
			t.Errorf("a reload that did not touch this upstream broke its icon URL (%s): "+
				"config-identical upstreams are reused, index and all", res.Status)
		}
	})

	t.Run("changed upstream rebuilds its index", func(t *testing.T) {
		cfg := base()
		cfg.Upstreams[0].CacheTTLMs = 45_000 // a fresh *upstream, cold index
		if err := gw.Reload(cfg); err != nil {
			t.Fatal(err)
		}
		// A client re-lists on reload; that is what repopulates the index.
		if _, err := session.ListTools(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		if res := getIcon(t, url, nil); res.StatusCode != http.StatusOK {
			t.Errorf("an icon URL did not survive a reload that rebuilt its upstream (%s): the "+
				"index miss path must rebuild from the cached list", res.Status)
		}
	})

	t.Run("retired upstream takes its icons", func(t *testing.T) {
		other := newIconUpstream(t, "beta", nil)
		cfg := base()
		cfg.Upstreams = []config.Upstream{{ID: "b", URL: other.url, Namespace: "b"}}
		if err := gw.Reload(cfg); err != nil {
			t.Fatal(err)
		}
		if res := getIcon(t, url, nil); res.StatusCode != http.StatusNotFound {
			t.Errorf("a retired upstream's icon URL answered %s, want 404 — the tool is gone too",
				res.Status)
		}
	})
}

// TestIconLeaderRechecksCacheBeforeFetching pins the window that made
// TestIconCachedAndSingleFlighted flaky on CI and not here: a request whose
// cache miss happened *before* another request's fetch completed, but whose
// entry into the flight happened *after* that flight ended. It finds the
// waiting map empty, becomes a leader in its own right, and fetches bytes the
// gateway is already holding.
//
// It is deliberately not written as a concurrency test. The interleaving
// needs a goroutine to lag between two adjacent statements, which a fast
// machine will not do on demand — 160 racing runs under CPU contention never
// reproduced it locally, while a two-core CI runner hit it on the first
// merge. So the state that window produces is set up directly instead: the
// bytes are in the cache, no flight is in progress, and fetchIcon is called
// exactly as a stale-miss caller would call it.
func TestIconLeaderRechecksCacheBeforeFetching(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	ts, gw := iconGateway(t, func(u string) *config.Config {
		return &config.Config{
			Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
			Server:    &config.ServerSection{PublicURL: u, AllowedHosts: []string{"*"}},
		}
	})
	session := connect(t, ts.URL, nil)
	url := toolIcons(t, session, "a__search")[0].Source

	// One real request, so the cache is warm and the flight has ended.
	if res := getIcon(t, url, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("first fetch answered %s", res.Status)
	}
	if n := up.hits.Load(); n != 1 {
		t.Fatalf("warm-up caused %d fetches, want 1", n)
	}

	// Now enter fetchIcon exactly as a caller whose Load missed before that
	// store would: no flight in progress, bytes already cached.
	parsed, err := neturl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	u, src, ok := gw.iconOwner(gw.rt(), parsed.Path)
	if !ok {
		t.Fatal("the minted URL no longer resolves to an upstream")
	}
	entry, served := gw.fetchIcon(context.Background(), u, src,
		u.cfg.ID+"\x00"+iconDigest(src), httptest.NewRecorder())
	if !served {
		t.Fatal("fetchIcon refused a request for an icon already in the cache")
	}
	if string(entry.body) != iconPNG {
		t.Errorf("returned %d bytes that are not the cached icon", len(entry.body))
	}
	if n := up.hits.Load(); n != 1 {
		t.Errorf("a leader fetched again for bytes already cached: %d upstream fetches, want 1. "+
			"After taking the flight, fetchIcon must re-check the cache — the caller's miss is "+
			"stale by then, and without that second check a request arriving just after a "+
			"completed flight leads a redundant fetch. This is what made the concurrency test "+
			"fail on CI and pass here.", n)
	}
}
