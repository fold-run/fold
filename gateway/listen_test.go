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

// TestNotificationFanIn proves both notification kinds fan in through the
// gateway to a default-options client: an upstream mutates its tool set and
// updates a subscribed resource, and the client hears tools/list_changed and
// resources/updated — resource URIs untouched.
//
// It also pins the negotiated protocol as a drift canary. The SDK's
// streamable HTTP server currently supports the 2026-07-28 protocol (whose
// notifications ride SEP-2575 subscriptions/listen streams) only with
// StreamableHTTPOptions.Stateless, which fold cannot use: session-keyed
// bridging (sampling, elicitation, per-client streams) requires stateful
// sessions. Default-options clients therefore fall back to the legacy
// handshake today. fold's fan-in already sits on the surfaces the SDK reuses
// for listen streams (SubscribeHandler, notifySessions), so when the SDK
// lifts the stateless-only restriction this canary fails: flip the
// assertion, and close the README "Not implemented" entry.
func TestNotificationFanIn(t *testing.T) {
	const uri = "file:///watched.txt"
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, &mcp.ServerOptions{
		SubscribeHandler:   func(context.Context, *mcp.SubscribeRequest) error { return nil },
		UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { return nil },
	})
	echo := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	server.AddTool(&mcp.Tool{Name: "original", InputSchema: json.RawMessage(`{"type":"object"}`)}, echo)
	server.AddResource(&mcp.Resource{URI: uri, Name: "watched"},
		func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "x"}}}, nil
		})
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL, Namespace: "ns"}},
	})

	var toolsChanged atomic.Bool
	var updatedURI atomic.Value // string
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			toolsChanged.Store(true)
		},
		ResourceUpdatedHandler: func(_ context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
			updatedURI.Store(req.Params.URI)
		},
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// Drift canary — see the doc comment. A failure here means the SDK now
	// negotiates the listen-stream protocol against fold's stateful server.
	if v := session.InitializeResult().ProtocolVersion; v >= "2026-07-28" {
		t.Fatalf("client negotiated %q against the stateful gateway: the SDK's stateless-only "+
			"restriction has been lifted — verify listen-stream fan-in, update this test and "+
			`the README "Not implemented" entry`, v)
	}

	// Prime the root session so the gateway hears upstream notifications.
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := session.Subscribe(context.Background(), &mcp.SubscribeParams{URI: uri}); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Upstream mutates: a new tool and a resource update.
	server.AddTool(&mcp.Tool{Name: "brand_new", InputSchema: json.RawMessage(`{"type":"object"}`)}, echo)
	if err := server.ResourceUpdated(context.Background(), &mcp.ResourceUpdatedNotificationParams{URI: uri}); err != nil {
		t.Fatalf("upstream ResourceUpdated: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (!toolsChanged.Load() || updatedURI.Load() == nil) {
		time.Sleep(20 * time.Millisecond)
	}
	if !toolsChanged.Load() {
		t.Error("tools/list_changed never arrived through the gateway")
	}
	got, _ := updatedURI.Load().(string)
	if got != uri {
		t.Errorf("resources/updated URI = %q, want %q (URIs are opaque and must not be rewritten)", got, uri)
	}
}
