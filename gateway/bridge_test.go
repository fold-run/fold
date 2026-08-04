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

// TestServerInitiatedBridging proves the full loop: a tool on the upstream
// requests sampling and elicitation from, sends progress and log messages
// to, the downstream client — all through the gateway.
func TestServerInitiatedBridging(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "needs_client",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		ss := req.Session

		if err := ss.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
			ProgressToken: req.Params.GetProgressToken(),
			Progress:      0.5,
		}); err != nil {
			t.Logf("NotifyProgress: %v", err)
		}
		ss.Log(ctx, &mcp.LoggingMessageParams{Level: "info", Data: "working"})

		sampled, err := ss.CreateMessage(ctx, &mcp.CreateMessageParams{
			MaxTokens: 10,
			Messages: []*mcp.SamplingMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "2+2?"}},
			},
		})
		if err != nil {
			return nil, err
		}
		elicited, err := ss.Elicit(ctx, &mcp.ElicitParams{
			Message:         "confirm?",
			RequestedSchema: json.RawMessage(`{"type":"object"}`),
		})
		if err != nil {
			return nil, err
		}
		text := sampled.Content.(*mcp.TextContent).Text + "/" + elicited.Action
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	var progressed, logged atomic.Bool
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, &mcp.ClientOptions{
		CreateMessageHandler: func(ctx context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			return &mcp.CreateMessageResult{
				Content: &mcp.TextContent{Text: "4"},
				Model:   "test-model",
				Role:    "assistant",
			}, nil
		},
		ElicitationHandler: func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
		},
		LoggingMessageHandler: func(ctx context.Context, req *mcp.LoggingMessageRequest) {
			logged.Store(true)
		},
		ProgressNotificationHandler: func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
			progressed.Store(true)
		},
	})
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })

	// Logging is opt-in: messages flow only after the client sets a level.
	if err := session.SetLoggingLevel(context.Background(), &mcp.SetLoggingLevelParams{Level: "debug"}); err != nil {
		t.Fatalf("SetLoggingLevel: %v", err)
	}

	params := &mcp.CallToolParams{Name: "needs_client", Arguments: map[string]any{}}
	params.SetProgressToken("tok-1")
	out, err := session.CallTool(context.Background(), params)
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if text := out.Content[0].(*mcp.TextContent).Text; text != "4/accept" {
		t.Errorf("round-trip = %q, want 4/accept", text)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && (!progressed.Load() || !logged.Load()) {
		time.Sleep(10 * time.Millisecond)
	}
	if !progressed.Load() {
		t.Error("progress notification did not reach the client")
	}
	if !logged.Load() {
		t.Error("log message did not reach the client")
	}
}
