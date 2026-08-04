package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// TestListChangedFanOut proves the freshness loop: an upstream mutates its
// tool set → the gateway hears list_changed, drops its cache, and re-emits
// the notification → the client refetches and sees the new tool.
func TestListChangedFanOut(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	echo := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	server.AddTool(&mcp.Tool{Name: "original", InputSchema: json.RawMessage(`{"type":"object"}`)}, echo)
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	var notified atomic.Bool
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			notified.Store(true)
		},
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// Prime the gateway's list cache (and its root upstream session, whose
	// handlers listen for upstream list_changed notifications).
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}

	// The upstream grows a tool.
	server.AddTool(&mcp.Tool{Name: "brand_new", InputSchema: json.RawMessage(`{"type":"object"}`)}, echo)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !notified.Load() {
		time.Sleep(20 * time.Millisecond)
	}
	if !notified.Load() {
		t.Fatal("client never received notifications/tools/list_changed through the gateway")
	}

	// The refreshed list must include the new tool (cache invalidated) and
	// never the sentinel.
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Join(toolNames(res), ",")
	if !strings.Contains(names, "brand_new") {
		t.Errorf("refetched list %q missing brand_new", names)
	}
	if strings.Contains(names, "sentinel") {
		t.Errorf("sentinel leaked into tool list: %q", names)
	}
}
