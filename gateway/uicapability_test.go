package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// appAwareCapabilities is what a host that can render MCP Apps declares at
// initialize — the extension identifier and the one content type the MVP
// defines.
func appAwareCapabilities() *mcp.ClientCapabilities {
	caps := &mcp.ClientCapabilities{}
	caps.AddExtension(uiExtensionID, map[string]any{"mimeTypes": []any{uiMimeType}})
	return caps
}

// gatingUpstream is a server that does what the extension advises: check the
// client's declared capabilities, and register the UI-enabled tool only for a
// client that can render it. Everyone else gets the text-only fallback.
//
// The gate runs in SDK receiving middleware because a Go SDK server holds one
// tool set for every session, and per-session registration is exactly what
// this fixture needs to express. It also records what each session declared,
// so a test can assert on what fold actually said upstream.
func gatingUpstream(t *testing.T) (*httptest.Server, func() []capProfile) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "gated", Version: "1.0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "show", Description: "text only", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "text"}}}, nil
		})

	var mu sync.Mutex
	var seen []capProfile
	srv.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			res, err := next(ctx, method, req)
			ss, _ := req.GetSession().(*mcp.ServerSession)
			if ss == nil {
				return res, err
			}
			profile := profilePlain
			if init := ss.InitializeParams(); init != nil {
				profile = profileFor(init.Capabilities)
			}
			if method == "initialize" {
				mu.Lock()
				seen = append(seen, profileFor(paramsCapabilities(req)))
				mu.Unlock()
			}
			list, ok := res.(*mcp.ListToolsResult)
			if !ok || err != nil || profile != profileUI {
				return res, err
			}
			// The app-enabled registration: same tool, now with an interface.
			upgraded := *list
			upgraded.Tools = []*mcp.Tool{{
				Name:        "show",
				Description: "with an interactive view",
				InputSchema: json.RawMessage(`{"type":"object"}`),
				Meta:        mcp.Meta{metaUI: map[string]any{metaUIResourceURI: "ui://view/main"}},
			}}
			return &upgraded, nil
		}
	})
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts, func() []capProfile {
		mu.Lock()
		defer mu.Unlock()
		return append([]capProfile(nil), seen...)
	}
}

// paramsCapabilities reads the capabilities off an initialize request, which
// is the only place a session's declaration is visible before the SDK has
// finished storing it.
func paramsCapabilities(req mcp.Request) *mcp.ClientCapabilities {
	p, ok := req.GetParams().(*mcp.InitializeParams)
	if !ok || p == nil {
		return nil
	}
	return p.Capabilities
}

func connectWithCaps(t *testing.T, url string, caps *mcp.ClientCapabilities) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, &mcp.ClientOptions{
		Capabilities: caps,
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func toolByName(t *testing.T, res *mcp.ListToolsResult, name string) *mcp.Tool {
	t.Helper()
	for _, tool := range res.Tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q missing from %v", name, toolNamesOf(res.Tools))
	return nil
}

// The parity case, and the reason this exists: a host that declared it can
// render apps must get the upstream's app-enabled registration through the
// gateway, exactly as it would connecting to that upstream directly.
func TestAppAwareClientGetsAppEnabledTools(t *testing.T) {
	up, declared := gatingUpstream(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "gated", URL: up.URL, Namespace: "gated"}},
	})

	session := connectWithCaps(t, ts.URL, appAwareCapabilities())
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	tool := toolByName(t, res, "gated__show")
	nested, _ := tool.Meta[metaUI].(map[string]any)
	if uri, _ := nested[metaUIResourceURI].(string); uri != "ui://fold/gated/view/main" {
		t.Fatalf("app-aware client got _meta.ui = %#v, want the minted interface pointer", tool.Meta)
	}

	// And fold said it upstream rather than inventing it: the upstream saw a
	// client declaring the extension.
	if seen := declared(); len(seen) == 0 || seen[len(seen)-1] != profileUI {
		t.Errorf("upstream saw declarations %v, want the last to be %q", seen, profileUI)
	}
}

// The other half of proxying a declaration: a client that declared nothing
// must still get the fallback, from a session that declared nothing.
func TestPlainClientStillGetsTheFallback(t *testing.T) {
	up, declared := gatingUpstream(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "gated", URL: up.URL, Namespace: "gated"}},
	})

	session := connect(t, ts.URL, nil)
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if tool := toolByName(t, res, "gated__show"); tool.Meta[metaUI] != nil {
		t.Errorf("plain client got _meta.ui = %#v, want none", tool.Meta)
	}
	for _, p := range declared() {
		if p == profileUI {
			t.Errorf("upstream saw a ui declaration for a federation with no app-aware client: %v", declared())
		}
	}
}

// Both kinds of client at once is the case a shared root session cannot
// serve, and the reason the session and its list cache are keyed by profile:
// neither client may be served the other's list.
func TestProfilesDoNotServeEachOthersLists(t *testing.T) {
	up, _ := gatingUpstream(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "gated", URL: up.URL, Namespace: "gated"}},
	})

	appAware := connectWithCaps(t, ts.URL, appAwareCapabilities())
	plain := connect(t, ts.URL, nil)
	ctx := context.Background()

	// Interleaved, and repeated: a single-slot cache would hand the second
	// caller whatever the first one warmed.
	for range 3 {
		appRes, err := appAware.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("app-aware list: %v", err)
		}
		plainRes, err := plain.ListTools(ctx, nil)
		if err != nil {
			t.Fatalf("plain list: %v", err)
		}
		if toolByName(t, appRes, "gated__show").Meta[metaUI] == nil {
			t.Fatal("app-aware client lost its interface pointer to the plain client's cached list")
		}
		if toolByName(t, plainRes, "gated__show").Meta[metaUI] != nil {
			t.Fatal("plain client was served the app-aware client's cached list")
		}
	}
}

// A tool call rides the per-client bridged session, which carries the same
// declaration — so an upstream that gates its *behaviour* on the capability
// sees the caller as the caller is.
func TestBridgedSessionCarriesTheDeclaration(t *testing.T) {
	var mu sync.Mutex
	var callProfiles []capProfile
	srv := mcp.NewServer(&mcp.Implementation{Name: "gated-call", Version: "1.0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "show", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			profile := profilePlain
			if init := req.Session.InitializeParams(); init != nil {
				profile = profileFor(init.Capabilities)
			}
			mu.Lock()
			callProfiles = append(callProfiles, profile)
			mu.Unlock()
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "gated", URL: up.URL, Namespace: "gated"}},
	})
	session := connectWithCaps(t, ts.URL, appAwareCapabilities())
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "gated__show"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(callProfiles) != 1 || callProfiles[0] != profileUI {
		t.Errorf("upstream saw call profiles %v, want [%q]", callProfiles, profileUI)
	}
}

// A profile is fold's own vocabulary, not the client's map. A client that
// invents extension identifiers must not mint a session or a cache entry per
// invention — the bound that keeps this feature from being a resource
// amplifier.
func TestProfileIgnoresUnknownExtensions(t *testing.T) {
	caps := &mcp.ClientCapabilities{}
	caps.AddExtension("com.example/invented", map[string]any{"mimeTypes": []any{"text/html"}})
	caps.AddExtension("com.example/another", map[string]any{})
	if got := profileFor(caps); got != profilePlain {
		t.Errorf("profileFor(unknown extensions) = %q, want %q", got, profilePlain)
	}

	// Declared, but for a content type fold does not serve: not a claim about
	// today's interfaces.
	future := &mcp.ClientCapabilities{}
	future.AddExtension(uiExtensionID, map[string]any{"mimeTypes": []any{"text/html;profile=mcp-app-v9"}})
	if got := profileFor(future); got != profilePlain {
		t.Errorf("profileFor(future mime type) = %q, want %q", got, profilePlain)
	}

	// The real declaration, in the shape the SDK produces.
	if got := profileFor(appAwareCapabilities()); got != profileUI {
		t.Errorf("profileFor(app-aware) = %q, want %q", got, profileUI)
	}
	if got := profileFor(nil); got != profilePlain {
		t.Errorf("profileFor(nil) = %q, want %q", got, profilePlain)
	}
}

// Cache keys must not collide across profiles, including in Redis where the
// key is all that separates one instance's entry from another's.
func TestProfileQualifiesCacheKeys(t *testing.T) {
	if got := profilePlain.qualify("tools"); got != "tools" {
		t.Errorf("plain profile changed the key: %q", got)
	}
	if profileUI.qualify("tools") == profilePlain.qualify("tools") {
		t.Error("ui and plain profiles share a cache key")
	}
}
