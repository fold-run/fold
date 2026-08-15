package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// The URI two MCP Apps upstreams collide on. It is not contrived: the
// extension requires a ui:// URI to be unique only within one server, and the
// published starter templates ship exactly this shape — no server segment —
// so two teams starting from one template produce two different interfaces
// with one name.
const collidingUIURI = "ui://widget/main"

// appUpstream is an MCP Apps-shaped server: a tool pointing at its interface
// through _meta.ui, and the ui:// resource that interface lives in. body is
// what distinguishes one upstream's app from the other's.
//
// The metadata carries the nested and the deprecated flat form together,
// which is what the TypeScript SDK puts on the wire today.
func appUpstream(t *testing.T, body string) (*httptest.Server, *mcp.Server) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "app-fixture", Version: "1.0"}, &mcp.ServerOptions{
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	srv.AddTool(&mcp.Tool{
		Name:        "show",
		Description: "renders an interface",
		InputSchema: json.RawMessage(`{"type":"object"}`),
		Meta: mcp.Meta{
			metaUI:             map[string]any{metaUIResourceURI: collidingUIURI},
			metaUIResourceFlat: collidingUIURI,
		},
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	srv.AddResource(&mcp.Resource{URI: collidingUIURI, Name: collidingUIURI, MIMEType: "text/html;profile=mcp-app"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
				{URI: req.Params.URI, MIMEType: "text/html;profile=mcp-app", Text: body},
			}}, nil
		})
	// An ordinary resource alongside it: its URI must survive untouched.
	srv.AddResource(&mcp.Resource{URI: "file:///data.txt", Name: "data"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: body}}}, nil
		})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts, srv
}

func uiResourceURI(t *testing.T, tool *mcp.Tool) string {
	t.Helper()
	nested, ok := tool.Meta[metaUI].(map[string]any)
	if !ok {
		t.Fatalf("tool %q lost its _meta.ui: %#v", tool.Name, tool.Meta)
	}
	uri, _ := nested[metaUIResourceURI].(string)
	if uri == "" {
		t.Fatalf("tool %q has no _meta.ui.resourceUri: %#v", tool.Name, tool.Meta)
	}
	return uri
}

// Two upstreams publishing the same ui:// URI must stay two resources through
// the gateway. Before minting, both tools pointed at one URI and a read of it
// was answered by whichever upstream last listed it — so a host rendering
// alpha's tool could get beta's interface, and which one it got depended on
// what some other client had done first.
func TestUIResourceCollisionKeepsUpstreamsApart(t *testing.T) {
	alphaUp, _ := appUpstream(t, "<html>alpha</html>")
	betaUp, _ := appUpstream(t, "<html>beta</html>")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "alpha", URL: alphaUp.URL, Namespace: "alpha"},
		{ID: "beta", URL: betaUp.URL, Namespace: "beta"},
	}})
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	uris := map[string]string{}
	for _, tool := range tools.Tools {
		uris[tool.Name] = uiResourceURI(t, tool)
	}
	if want := "ui://fold/alpha/widget/main"; uris["alpha__show"] != want {
		t.Errorf("alpha__show _meta.ui.resourceUri = %q, want %q", uris["alpha__show"], want)
	}
	if want := "ui://fold/beta/widget/main"; uris["beta__show"] != want {
		t.Errorf("beta__show _meta.ui.resourceUri = %q, want %q", uris["beta__show"], want)
	}

	// Each minted URI reads its own upstream's interface, and says so.
	for _, tc := range []struct{ tool, want, upstream string }{
		{"alpha__show", "<html>alpha</html>", "alpha"},
		{"beta__show", "<html>beta</html>", "beta"},
	} {
		res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uris[tc.tool]})
		if err != nil {
			t.Fatalf("read %s: %v", uris[tc.tool], err)
		}
		if got := res.Contents[0].Text; got != tc.want {
			t.Errorf("%s served %q, want %q — the wrong upstream's app", tc.tool, got, tc.want)
		}
		// The URI a read answers with must be one the caller can use again.
		if got := res.Contents[0].URI; got != uris[tc.tool] {
			t.Errorf("%s content URI = %q, want the minted %q", tc.tool, got, uris[tc.tool])
		}
		if got, _ := res.Meta[metaUpstream].(string); got != tc.upstream {
			t.Errorf("%s answered by upstream %q, want %q", tc.tool, got, tc.upstream)
		}
	}
}

// The deprecated flat pointer is rewritten with the nested one: a host reading
// either must reach the same resource.
func TestUIResourceFlatMetadataMintedToo(t *testing.T) {
	up, _ := appUpstream(t, "<html>alpha</html>")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "alpha", URL: up.URL, Namespace: "alpha"},
		{ID: "beta", URL: up.URL, Namespace: "beta"},
	}})
	session := connect(t, ts.URL, nil)

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range tools.Tools {
		flat, _ := tool.Meta[metaUIResourceFlat].(string)
		if nested := uiResourceURI(t, tool); flat != nested {
			t.Errorf("tool %q: flat pointer %q, nested %q — a host reading the flat form lands elsewhere",
				tool.Name, flat, nested)
		}
	}
}

// resources/list carries the same minted URIs, and leaves every other scheme
// exactly as the upstream published it.
func TestUIResourceListMintsOnlyUIScheme(t *testing.T) {
	alphaUp, _ := appUpstream(t, "<html>alpha</html>")
	betaUp, _ := appUpstream(t, "<html>beta</html>")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "alpha", URL: alphaUp.URL, Namespace: "alpha"},
		{ID: "beta", URL: betaUp.URL, Namespace: "beta"},
	}})
	session := connect(t, ts.URL, nil)

	res, err := session.ListResources(context.Background(), nil)
	if err != nil {
		t.Fatalf("list resources: %v", err)
	}
	seen := map[string]int{}
	for _, r := range res.Resources {
		seen[r.URI]++
	}
	for _, want := range []string{"ui://fold/alpha/widget/main", "ui://fold/beta/widget/main"} {
		if seen[want] != 1 {
			t.Errorf("resources/list has %d entries for %q, want exactly 1: %v", seen[want], want, seen)
		}
	}
	if seen[collidingUIURI] != 0 {
		t.Errorf("raw %q still listed: the collision survived", collidingUIURI)
	}
	if seen["file:///data.txt"] != 2 {
		t.Errorf("ordinary resource URIs = %d, want 2 unrewritten copies (one per upstream): %v",
			seen["file:///data.txt"], seen)
	}
}

// Passthrough rewrites nothing — not names, not URIs. A single upstream
// behind fold must be byte-identical to the upstream itself.
func TestUIResourcePassthroughLeavesURIsAlone(t *testing.T) {
	up, _ := appUpstream(t, "<html>only</html>")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "only", URL: up.URL}}})
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if got := uiResourceURI(t, tools.Tools[0]); got != collidingUIURI {
		t.Errorf("passthrough rewrote _meta.ui.resourceUri to %q, want %q", got, collidingUIURI)
	}
	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: collidingUIURI})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := res.Contents[0].URI; got != collidingUIURI {
		t.Errorf("content URI = %q, want %q", got, collidingUIURI)
	}
}

// A subscription taken on the minted URI must reach the upstream under the
// URI it knows, and the update must come back under the URI the client
// subscribed with — otherwise a client cannot match the notification to its
// own subscription.
func TestUIResourceSubscriptionRoundTrip(t *testing.T) {
	var subscribed atomic.Value // string
	alphaSrv := mcp.NewServer(&mcp.Implementation{Name: "app-fixture", Version: "1.0"}, &mcp.ServerOptions{
		SubscribeHandler: func(_ context.Context, req *mcp.SubscribeRequest) error {
			subscribed.Store(req.Params.URI)
			return nil
		},
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	alphaSrv.AddResource(&mcp.Resource{URI: collidingUIURI, Name: collidingUIURI},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "x"}}}, nil
		})
	alphaUp := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return alphaSrv }, nil))
	t.Cleanup(alphaUp.Close)
	betaUp, _ := appUpstream(t, "<html>beta</html>")

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "alpha", URL: alphaUp.URL, Namespace: "alpha"},
		{ID: "beta", URL: betaUp.URL, Namespace: "beta"},
	}})

	var updated atomic.Value // string
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, &mcp.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updated.Store(req.Params.URI)
		},
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	const minted = "ui://fold/alpha/widget/main"
	if _, err := session.ListResources(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Subscribe(context.Background(), &mcp.SubscribeParams{URI: minted}); err != nil {
		t.Fatalf("subscribe %s: %v", minted, err)
	}
	if got, _ := subscribed.Load().(string); got != collidingUIURI {
		t.Errorf("upstream subscribed to %q, want its own %q", got, collidingUIURI)
	}

	if err := alphaSrv.ResourceUpdated(context.Background(), &mcp.ResourceUpdatedNotificationParams{URI: collidingUIURI}); err != nil {
		t.Fatalf("upstream ResourceUpdated: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && updated.Load() == nil {
		time.Sleep(20 * time.Millisecond)
	}
	if got, _ := updated.Load().(string); got != minted {
		t.Errorf("client heard resources/updated for %q, want the URI it subscribed with, %q", got, minted)
	}
}

// Policy decides on the upstream's own URI, the way a tool is authorized on
// its bare name: a rule an operator wrote against the upstream keeps working,
// and a URI fold invented never has to appear in one.
func TestUIResourcePolicyDecidesOnUpstreamURI(t *testing.T) {
	alphaUp, _ := appUpstream(t, "<html>alpha</html>")
	betaUp, _ := appUpstream(t, "<html>beta</html>")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{
			{ID: "alpha", URL: alphaUp.URL, Namespace: "alpha"},
			{ID: "beta", URL: betaUp.URL, Namespace: "beta"},
		},
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID: "alpha-only",
				Allow: []config.PolicyAllow{
					{Server: "alpha", Methods: []string{"resources/read"}, Names: []string{"ui://widget/*"}},
					{Server: "alpha", Methods: []string{"tools/list", "resources/list"}},
					{Server: "beta", Methods: []string{"tools/list", "resources/list"}},
				},
			}},
		},
	})
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ui://fold/alpha/widget/main"}); err != nil {
		t.Errorf("read allowed by a rule naming the upstream's own URI failed: %v", err)
	}
	if _, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "ui://fold/beta/widget/main"}); err == nil {
		t.Error("read of an upstream with no resources/read grant succeeded")
	}
}

func TestSplitUIURI(t *testing.T) {
	for _, tc := range []struct {
		uri, ns, original string
		ok                bool
	}{
		{"ui://fold/alpha/widget/main", "alpha", "ui://widget/main", true},
		{"ui://fold/alpha/main", "alpha", "ui://main", true},
		// Not minted by fold: left to the existing read path rather than guessed at.
		{"ui://widget/main", "", "", false},
		{"ui://fold/alpha", "", "", false},
		{"ui://fold//main", "", "", false},
		{"ui://fold/", "", "", false},
		{"file:///data.txt", "", "", false},
		{"", "", "", false},
	} {
		ns, original, ok := splitUIURI(tc.uri)
		if ok != tc.ok || ns != tc.ns || original != tc.original {
			t.Errorf("splitUIURI(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.uri, ns, original, ok, tc.ns, tc.original, tc.ok)
		}
	}
}

// Round-tripping is what makes the scheme safe: whatever an upstream
// publishes, minting it and splitting it back must return the original.
func TestMintUIURIRoundTrips(t *testing.T) {
	u := &upstream{cfg: config.Upstream{ID: "alpha", Namespace: "alpha"}}
	for _, uri := range []string{
		"ui://widget/main",
		"ui://a",
		"ui://fold/alpha/already-looks-minted",
		"ui://weird//double//slashes",
	} {
		minted := u.mintUIURI(uri)
		ns, original, ok := splitUIURI(minted)
		if !ok || ns != "alpha" || original != uri {
			t.Errorf("mint(%q) = %q, split → (%q, %q, %v)", uri, minted, ns, original, ok)
		}
	}
	// Other schemes are not fold's to rewrite.
	if got := u.mintUIURI("file:///x"); got != "file:///x" {
		t.Errorf("mintUIURI rewrote a non-ui scheme: %q", got)
	}
}
