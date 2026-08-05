package kubediscovery

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/gateway"
)

// TestFullLoopWithGateway proves the whole self-serve story end to end:
// a labeled Kubernetes Service appears in the fake API → the producer maps
// and serves the discovery document → a real fold gateway polls it and
// federates the upstream → an MCP client sees and calls the tool. Then the
// Service is delisted and the tool disappears.
func TestFullLoopWithGateway(t *testing.T) {
	// A real MCP server standing in for the team's Service.
	server := mcp.NewServer(&mcp.Implementation{Name: "team-svc", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{Name: "team_tool", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	upstream := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil))
	t.Cleanup(upstream.Close)

	// The fake cluster: one labeled Service whose fold.run/url points at the
	// MCP server (cluster DNS does not resolve in a test).
	api, services, _ := fakeKubeAPI(t)
	svc := service("prod", "team-svc", map[string]string{AnnURL: upstream.URL})
	services.Store([]Service{svc})

	client, err := NewClient(api.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	producer := &Producer{Client: client, Interval: 50 * time.Millisecond, Log: slog.New(slog.DiscardHandler)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go producer.Run(ctx)
	front := httptest.NewServer(producer.Handler())
	t.Cleanup(front.Close)

	// A real gateway consuming the producer.
	gw, err := gateway.New(&config.Config{
		Upstreams: []config.Upstream{{ID: "static", URL: upstream.URL, Namespace: "static"}},
		Discovery: &config.Discovery{URL: front.URL, IntervalMs: 50},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	gwServer := httptest.NewServer(gw.Handler())
	t.Cleanup(gwServer.Close)

	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	session, err := mcpClient.Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: gwServer.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	listNames := func() string {
		res, err := session.ListTools(context.Background(), nil)
		if err != nil {
			return "err:" + err.Error()
		}
		var names []string
		for _, tool := range res.Tools {
			names = append(names, tool.Name)
		}
		return strings.Join(names, ",")
	}
	waitList := func(cond func(string) bool, msg string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cond(listNames()) {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s (list: %s)", msg, listNames())
	}

	// Labeled Service → federated tool, no operator involvement.
	waitList(func(names string) bool { return strings.Contains(names, "prod-team--svc__team_tool") },
		"discovered service never became routable through the gateway")
	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "prod-team--svc__team_tool"})
	if err != nil {
		t.Fatalf("CallTool through the full loop: %v", err)
	}
	if text := out.Content[0].(*mcp.TextContent).Text; text != "ok" {
		t.Errorf("tool answered %q", text)
	}

	// Delisted Service → tool disappears; the static upstream is untouched.
	services.Store([]Service{})
	waitList(func(names string) bool {
		return !strings.Contains(names, "prod-team--svc__") && strings.Contains(names, "static__team_tool")
	}, "delisted service never left the federation")
}
