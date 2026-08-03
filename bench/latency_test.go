// Package bench measures the gateway's added latency on the proxy path:
// the same client, the same upstream, with and without fold-go in between.
// Run with FOLD_BENCH=1 (skipped otherwise); CI gates on the p50 bound.
package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold-go/config"
	"github.com/fold-run/fold-go/gateway"
)

const (
	warmup     = 200
	iterations = 2000
	// maxAddedP50 is deliberately loose for shared CI runners; typical
	// local numbers are well under 1ms. It gates regressions, not records.
	maxAddedP50 = 5 * time.Millisecond
)

func newEchoUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "bench-upstream", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "echo",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func connectSession(t *testing.T, url string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "bench-client", Version: "1.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", url, err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// measure returns sorted per-call latencies for iterations sequential calls.
func measure(t *testing.T, session *mcp.ClientSession) []time.Duration {
	t.Helper()
	params := &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{}}
	for i := 0; i < warmup; i++ {
		if _, err := session.CallTool(context.Background(), params); err != nil {
			t.Fatalf("warmup call: %v", err)
		}
	}
	out := make([]time.Duration, iterations)
	for i := range out {
		start := time.Now()
		if _, err := session.CallTool(context.Background(), params); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		out[i] = time.Since(start)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	idx := int(float64(len(sorted)-1) * p)
	return sorted[idx]
}

func TestAddedLatencyGate(t *testing.T) {
	if os.Getenv("FOLD_BENCH") == "" {
		t.Skip("set FOLD_BENCH=1 to run the latency gate")
	}

	up := newEchoUpstream(t)

	gw, err := gateway.New(&config.Config{
		Upstreams: []config.Upstream{{ID: "bench", URL: up.URL}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	gwServer := httptest.NewServer(gw.Handler())
	t.Cleanup(gwServer.Close)

	direct := measure(t, connectSession(t, up.URL))
	proxied := measure(t, connectSession(t, gwServer.URL+"/mcp"))

	report := func(name string, d []time.Duration) {
		t.Logf("%-8s p50=%-10s p90=%-10s p99=%s", name,
			percentile(d, 0.50), percentile(d, 0.90), percentile(d, 0.99))
	}
	report("direct", direct)
	report("gateway", proxied)

	added := percentile(proxied, 0.50) - percentile(direct, 0.50)
	t.Logf("added p50 latency: %s (gate: < %s)", added, maxAddedP50)
	fmt.Printf("BENCH_RESULT added_p50=%s direct_p50=%s gateway_p50=%s gateway_p99=%s\n",
		added, percentile(direct, 0.50), percentile(proxied, 0.50), percentile(proxied, 0.99))

	if added > maxAddedP50 {
		t.Fatalf("added p50 latency %s exceeds gate %s", added, maxAddedP50)
	}
}
