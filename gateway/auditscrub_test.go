package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// audit.scrub edits the record, never the response. The usage map an upstream
// publishes in its result _meta is the same object the gateway hands to audit
// and then serializes to the client, so a scrub that deleted keys in place
// would redact the caller's result as well as the trail — a control meant to
// keep secrets out of the sinks silently taking data away from the client.
// The invisibility rule says the response through fold matches the upstream's.
func TestScrubRedactsTheTrailNotTheResponse(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "usage-fixture", Version: "1.0"}, nil)
	srv.AddTool(&mcp.Tool{Name: "reports", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Meta:    mcp.Meta{"usage": map[string]any{"inputTokens": float64(12), "secret": "for-the-client"}},
				Content: []mcp.Content{&mcp.TextContent{Text: "ok"}},
			}, nil
		})
	up := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(up.Close)

	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "a", URL: up.URL, Namespace: "a"}},
		Audit: &config.Audit{
			Sinks: []config.AuditSink{{Type: "file", Path: auditPath}},
			Scrub: &config.AuditScrub{RedactUsageKeys: []string{"secret"}},
		},
	})
	session := connect(t, ts.URL, nil)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "a__reports"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	usage, _ := res.Meta["usage"].(map[string]any)
	if usage["secret"] != "for-the-client" {
		t.Fatalf("the scrub reached the client's result: usage=%v", res.Meta["usage"])
	}
	if usage["inputTokens"] != float64(12) {
		t.Fatalf("usage[inputTokens] = %v, want 12", usage["inputTokens"])
	}

	// The file sink writes synchronously on Emit, which runs before the
	// result is returned, so the record is on disk by now.
	events := readAuditEvents(t, auditPath, "tools/call")
	if len(events) != 1 {
		t.Fatalf("got %d tools/call events, want 1", len(events))
	}
	if _, ok := events[0].Usage["secret"]; ok {
		t.Fatalf("secret reached the audit sink: %v", events[0].Usage)
	}
	if events[0].Usage["inputTokens"] != float64(12) {
		t.Fatalf("scrub removed more than it was told to: %v", events[0].Usage)
	}
}
