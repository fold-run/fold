package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

const iconPNG = "\x89PNG\r\n\x1a\n" + "the rest is not a real image, and does not need to be"

// iconUpstream is a real SDK server that also serves its own icon bytes, so
// the icon source is genuinely on the upstream's origin — which is the whole
// condition that makes a federated icon unreachable.
type iconUpstream struct {
	url    string
	hits   *atomic.Int64
	iconAt string // absolute src the tool advertises
	// serve is what the icon path answers with, swappable so a test can make
	// the upstream send an SVG, a redirect, or something oversized.
	serve atomic.Pointer[http.HandlerFunc]
}

func newIconUpstream(t *testing.T, name string, opt func(u *iconUpstream, s *mcp.Server)) *iconUpstream {
	t.Helper()
	up := &iconUpstream{hits: &atomic.Int64{}}
	s := mcp.NewServer(&mcp.Implementation{Name: name, Version: "1.0"}, nil)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	up.url = srv.URL
	up.iconAt = srv.URL + "/brand.png"

	mux.HandleFunc("/brand.png", func(w http.ResponseWriter, r *http.Request) {
		up.hits.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			// Recorded rather than asserted here so the failure lands in the
			// test that cares, with its own message.
			w.Header().Set("X-Fold-Test-Credential", "present")
		}
		if h := up.serve.Load(); h != nil {
			(*h)(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte(iconPNG))
	})
	if opt != nil {
		opt(up, s)
	} else {
		addIconTool(s, "search", up.iconAt)
	}
	mux.Handle("/", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	return up
}

func addIconTool(s *mcp.Server, name string, srcs ...string) {
	icons := make([]mcp.Icon, 0, len(srcs))
	for _, src := range srcs {
		icons = append(icons, mcp.Icon{Source: src, MIMEType: "image/png", Sizes: []string{"48x48"}})
	}
	s.AddTool(&mcp.Tool{
		Name:        name,
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Icons:       icons,
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
}

// iconGateway starts a gateway whose public URL is its own test-server URL,
// so a minted src is genuinely fetchable in the test.
func iconGateway(t *testing.T, build func(publicURL string) *config.Config) (*httptest.Server, *Gateway) {
	t.Helper()
	// fold does not know its own origin — that is the whole reason
	// server.publicUrl exists — so the test has to fix one before building the
	// gateway that mints under it. Take the listener first, name it in the
	// config, then hand it to the server.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()

	gw, err := New(build(url))
	if err != nil {
		ln.Close()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(gw.Close)
	ts := httptest.NewUnstartedServer(gw.Handler())
	ts.Listener.Close()
	ts.Listener = ln
	ts.Start()
	t.Cleanup(ts.Close)
	return ts, gw
}

func toolIcons(t *testing.T, session *mcp.ClientSession, name string) []mcp.Icon {
	t.Helper()
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name == name {
			return tool.Icons
		}
	}
	t.Fatalf("tool %q not in the federated list", name)
	return nil
}

func TestIconsMintedToGatewayOrigin(t *testing.T) {
	alpha := newIconUpstream(t, "alpha", nil)
	beta := newIconUpstream(t, "beta", nil)

	var publicURL string
	ts, _ := iconGateway(t, func(u string) *config.Config {
		publicURL = u
		return &config.Config{
			Upstreams: []config.Upstream{
				{ID: "a", URL: alpha.url, Namespace: "a"},
				{ID: "b", URL: beta.url, Namespace: "b"},
			},
			Server: &config.ServerSection{PublicURL: publicURL, AllowedHosts: []string{"*"}},
		}
	})
	session := connect(t, ts.URL, nil)

	a := toolIcons(t, session, "a__search")
	b := toolIcons(t, session, "b__search")
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("federated icons: alpha %d, beta %d, want 1 each", len(a), len(b))
	}
	for _, got := range []mcp.Icon{a[0], b[0]} {
		if !strings.HasPrefix(got.Source, publicURL+iconPathPrefix) {
			t.Errorf("icon src %q is not on the gateway's origin %q: a conforming client is told to "+
				"verify that icon URIs are same-origin with the server, and an upstream's own origin "+
				"is neither that nor, in a cluster, reachable at all", got.Source, publicURL)
		}
		if got.MIMEType != "image/png" || len(got.Sizes) != 1 || got.Sizes[0] != "48x48" {
			t.Errorf("minting altered fields other than src: %+v", got)
		}
	}
	if a[0].Source == b[0].Source {
		t.Error("two upstreams' icons minted to one URL: the namespace segment is what keeps them apart")
	}
}

func TestIconsPassthroughLeavesSrcAlone(t *testing.T) {
	up := newIconUpstream(t, "solo", nil)
	ts, _ := iconGateway(t, func(u string) *config.Config {
		return &config.Config{
			Upstreams: []config.Upstream{{ID: "solo", URL: up.url}},
			Server:    &config.ServerSection{PublicURL: u, AllowedHosts: []string{"*"}},
		}
	})
	session := connect(t, ts.URL, nil)

	icons := toolIcons(t, session, "search")
	if len(icons) != 1 || icons[0].Source != up.iconAt {
		t.Errorf("passthrough rewrote an icon src: got %+v, want %q — an un-namespaced single "+
			"upstream is not a federation and a client must not be able to tell fold is there",
			icons, up.iconAt)
	}
	res, err := http.Get(ts.URL + iconPathPrefix + "x/" + strings.Repeat("a", iconDigestLen))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("passthrough served %s from /icons; it mints nothing and must serve nothing", res.Status)
	}
}

func TestIconsNotMintedWithoutPublicURL(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
	})
	session := connect(t, ts.URL, nil)

	icons := toolIcons(t, session, "a__search")
	if len(icons) != 1 || icons[0].Source != up.iconAt {
		t.Errorf("icons were rewritten with no public URL to rewrite them to: got %+v", icons)
	}
}

func TestIconsFallBackToAuthResource(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	var resource string
	ts, _ := iconGateway(t, func(u string) *config.Config {
		resource = u
		return &config.Config{
			Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
			Server:    &config.ServerSection{AllowedHosts: []string{"*"}},
			Auth:      &config.Auth{Resource: u},
		}
	})
	session := connect(t, ts.URL, nil)

	icons := toolIcons(t, session, "a__search")
	if len(icons) != 1 || !strings.HasPrefix(icons[0].Source, resource+iconPathPrefix) {
		t.Errorf("auth.resource is fold's canonical external identity and should mint icons when "+
			"server.publicUrl is unset: got %+v", icons)
	}
}

func TestIconsDataURIsPassThrough(t *testing.T) {
	const data = "data:image/png;base64,iVBORw0KGgo="
	up := newIconUpstream(t, "alpha", func(u *iconUpstream, s *mcp.Server) {
		addIconTool(s, "search", data)
	})
	ts, _ := iconGateway(t, func(u string) *config.Config {
		return &config.Config{
			Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
			Server:    &config.ServerSection{PublicURL: u, AllowedHosts: []string{"*"}},
		}
	})
	session := connect(t, ts.URL, nil)

	icons := toolIcons(t, session, "a__search")
	if len(icons) != 1 || icons[0].Source != data {
		t.Errorf("a data: icon was rewritten: %+v — it is already same-origin-safe and already "+
			"reachable, so proxying it would only re-serve bytes fold is holding", icons)
	}
	if n := up.hits.Load(); n != 0 {
		t.Errorf("a data: icon caused %d outbound fetches", n)
	}
}

func TestIconsForeignHostPassesThrough(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("fold fetched an icon from a host no upstream was configured with: %s", r.URL)
	}))
	t.Cleanup(other.Close)

	src := other.URL + "/elsewhere.png"
	up := newIconUpstream(t, "alpha", func(u *iconUpstream, s *mcp.Server) {
		addIconTool(s, "search", src)
	})
	ts, _ := iconGateway(t, func(u string) *config.Config {
		return &config.Config{
			Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
			Server:    &config.ServerSection{PublicURL: u, AllowedHosts: []string{"*"}},
		}
	})
	session := connect(t, ts.URL, nil)

	icons := toolIcons(t, session, "a__search")
	if len(icons) != 1 || icons[0].Source != src {
		t.Errorf("an icon on a host the upstream was not configured with should be republished as "+
			"sent, not minted: got %+v", icons)
	}
	// And the digest for it must not be a way in either.
	res, err := http.Get(ts.URL + iconPathPrefix + "a/" + iconDigest(src))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("a hand-built digest for an off-origin src was served %s: the host bound must be "+
			"checked at mint time and again before the fetch, so a digest cannot smuggle a request "+
			"to a host the operator never configured", res.Status)
	}
}

func TestIconsUnsafeSchemesDropped(t *testing.T) {
	up := newIconUpstream(t, "alpha", func(u *iconUpstream, s *mcp.Server) {
		addIconTool(s, "search", "javascript:alert(1)", u.iconAt, "file:///etc/passwd")
	})
	ts, _ := iconGateway(t, func(u string) *config.Config {
		return &config.Config{
			Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
			Server:    &config.ServerSection{PublicURL: u, AllowedHosts: []string{"*"}},
		}
	})
	session := connect(t, ts.URL, nil)

	icons := toolIcons(t, session, "a__search")
	if len(icons) != 1 {
		t.Fatalf("unsafe icon schemes survived federation: %+v — a client MUST reject these, and "+
			"republishing one under fold's name is fold vouching for it", icons)
	}
	if !strings.Contains(icons[0].Source, iconPathPrefix) {
		t.Errorf("the safe icon beside the unsafe ones was not minted: %+v", icons)
	}
}

// TestIconMintDoesNotMutateCachedList is the one that must not regress.
// mcp.Tool.Icons is a slice of values, so the shallow struct copy every egress
// path takes shares the backing array: rewriting an element in place would
// corrupt the entry cachedList hands every other request, and with Redis, the
// whole fleet's.
func TestIconMintDoesNotMutateCachedList(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	ts, gw := iconGateway(t, func(u string) *config.Config {
		return &config.Config{
			Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
			Server:    &config.ServerSection{PublicURL: u, AllowedHosts: []string{"*"}},
		}
	})
	session := connect(t, ts.URL, nil)

	if got := toolIcons(t, session, "a__search"); len(got) != 1 ||
		!strings.Contains(got[0].Source, iconPathPrefix) {
		t.Fatalf("icon not minted: %+v", got)
	}

	// The bare list the cache holds must still name the upstream's own src.
	u := gw.rt().byID["a"]
	bare, err := u.listTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range bare {
		for _, icon := range tool.Icons {
			if icon.Source != up.iconAt {
				t.Fatalf("minting wrote through into the cached list: cached src is %q, want the "+
					"upstream's own %q. Icons is a []Icon, so a shallow struct copy shares its "+
					"backing array — mintIcons must slices.Clone before writing, or every other "+
					"request (and, with Redis, every other instance) reads fold's rewrite as if "+
					"the upstream had sent it.", icon.Source, up.iconAt)
			}
		}
	}
}

func TestIconMintIsMemoized(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	ts, gw := iconGateway(t, func(u string) *config.Config {
		return &config.Config{
			Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
			Server:    &config.ServerSection{PublicURL: u, AllowedHosts: []string{"*"}},
		}
	})
	connect(t, ts.URL, nil)

	u := gw.rt().byID["a"]
	ctx := context.Background()
	bare, err := u.listTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := u.namespacedTools(ctx, bare)
	second := u.namespacedTools(ctx, bare)
	if &first[0] != &second[0] {
		t.Error("the namespaced view was rebuilt for an unchanged list: the mint is a pure function " +
			"of the decoded list and belongs inside the publicView memo, or it lands on the " +
			"per-request path the latency gate covers")
	}
}

func TestSplitIconPath(t *testing.T) {
	good := strings.Repeat("ab", iconDigestLen/2)
	cases := map[string]struct {
		path       string
		ns, digest string
		ok         bool
	}{
		"minted":          {iconPathPrefix + "alpha/" + good, "alpha", good, true},
		"no prefix":       {"/other/alpha/" + good, "", "", false},
		"no digest":       {iconPathPrefix + "alpha", "", "", false},
		"empty namespace": {iconPathPrefix + "/" + good, "", "", false},
		"short digest":    {iconPathPrefix + "alpha/abc", "", "", false},
		"not hex":         {iconPathPrefix + "alpha/" + strings.Repeat("zz", iconDigestLen/2), "", "", false},
		"traversal":       {iconPathPrefix + "alpha/../../etc/passwd", "", "", false},
	}
	for name, tc := range cases {
		ns, digest, ok := splitIconPath(tc.path)
		if ok != tc.ok || ns != tc.ns || digest != tc.digest {
			t.Errorf("%s: splitIconPath(%q) = (%q, %q, %v), want (%q, %q, %v)",
				name, tc.path, ns, digest, ok, tc.ns, tc.digest, tc.ok)
		}
	}
}

func TestIconDigestIsStable(t *testing.T) {
	const src = "https://upstream.internal/brand.png"
	if a, b := iconDigest(src), iconDigest(src); a != b {
		t.Fatalf("digest is not deterministic: %q vs %q", a, b)
	}
	if got := len(iconDigest(src)); got != iconDigestLen {
		t.Errorf("digest is %d chars, want %d — the width is part of the URL shape "+
			"splitIconPath validates", got, iconDigestLen)
	}
	if iconDigest(src) == iconDigest(src+"?v=2") {
		t.Error("two sources digested to one name")
	}
}

func TestGatewayIdentityIsFoldsOwn(t *testing.T) {
	up := newIconUpstream(t, "alpha", nil)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: up.url, Namespace: "a"}},
		Server: &config.ServerSection{Identity: &config.Identity{
			WebsiteURL: "https://acme.example/mcp",
			Icons: []config.Icon{{
				Src: "data:image/png;base64,iVBORw0KGgo=", MIMEType: "image/png", Theme: "dark",
			}},
		}},
	})
	session := connect(t, ts.URL, nil)

	impl := session.InitializeResult().ServerInfo
	if impl.Name != "fold" {
		t.Errorf("server name is %q; a federation of N upstreams has one server info and it is "+
			"fold's own", impl.Name)
	}
	if impl.WebsiteURL != "https://acme.example/mcp" {
		t.Errorf("websiteUrl %q, want the configured one", impl.WebsiteURL)
	}
	if len(impl.Icons) != 1 || impl.Icons[0].Theme != "dark" {
		t.Errorf("identity icons did not reach the server info: %+v", impl.Icons)
	}
}

var _ = fmt.Sprintf

// serveIconBytes makes the upstream answer its icon path with exactly these
// bytes under this declared type — the declared type being advisory is half of
// what the validation tests are about.
func serveIconBytes(t *testing.T, up *iconUpstream, declared string, body []byte) {
	t.Helper()
	serveIconFunc(t, up, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", declared)
		_, _ = w.Write(body)
	})
}

// serveIconFunc swaps the upstream's icon handler outright.
func serveIconFunc(t *testing.T, up *iconUpstream, h http.HandlerFunc) {
	t.Helper()
	up.serve.Store(&h)
	t.Cleanup(func() { up.serve.Store(nil) })
}
