package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// TestHangingStandaloneSSE reproduces a real-world upstream (observed in the
// wild) that accepts the standalone SSE GET but never sends response
// headers. The SDK client opens that stream synchronously during the
// handshake, so without mitigation the hang eats the entire connect budget.
// The gateway's transport converts the hang into a synthetic 405, and the
// session must proceed normally without the stream.
func TestHangingStandaloneSSE(t *testing.T) {
	old := sseHeaderTimeout
	sseHeaderTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sseHeaderTimeout = old })

	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{Name: "tool", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
		})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)

	hang := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			<-hang // accept the GET, never send headers
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)
	// Registered after up.Close so it runs first (LIFO): the hanging GET
	// handler must unblock before the server can close.
	t.Cleanup(func() { close(hang) })

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "u", URL: up.URL, Timeouts: &config.Timeouts{ConnectMs: 2000}},
	}})
	session := connect(t, ts.URL, nil)

	start := time.Now()
	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"})
	if err != nil {
		t.Fatalf("CallTool through hanging-SSE upstream: %v", err)
	}
	if text := out.Content[0].(*mcp.TextContent).Text; text != "ok" {
		t.Errorf("result = %q", text)
	}
	// The call must not have burned the connect budget waiting on the GET.
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("call took %s; the hanging GET should have been cut at %s", elapsed, sseHeaderTimeout)
	}
}

// TestIdleTimeoutBody: silence beyond the idle bound cuts the stream with a
// recognizable error; a stream that keeps delivering stays open across
// several idle windows.
func TestIdleTimeoutBody(t *testing.T) {
	t.Run("hung stream is cut", func(t *testing.T) {
		pr, _ := io.Pipe() // no writer: permanent silence
		body := newIdleTimeoutBody(pr, 100*time.Millisecond)
		defer body.Close()
		done := make(chan error, 1)
		go func() {
			_, err := body.Read(make([]byte, 64))
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "idle") {
				t.Fatalf("expected an idle-timeout error, got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("read never unblocked")
		}
	})

	t.Run("active stream survives", func(t *testing.T) {
		pr, pw := io.Pipe()
		body := newIdleTimeoutBody(pr, 300*time.Millisecond)
		defer body.Close()
		stop := make(chan struct{})
		go func() {
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					if _, err := pw.Write([]byte("data: x\n\n")); err != nil {
						return
					}
				case <-stop:
					_ = pw.Close()
					return
				}
			}
		}()
		deadline := time.Now().Add(1 * time.Second) // > 3 idle windows
		buf := make([]byte, 64)
		for time.Now().Before(deadline) {
			if _, err := body.Read(buf); err != nil {
				t.Fatalf("active stream was cut: %v", err)
			}
		}
		close(stop)
	})
}
