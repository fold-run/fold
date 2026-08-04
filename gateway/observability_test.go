package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

func TestMetricsEndpoint(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})
	session := connect(t, ts.URL, nil)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		`fold_requests_total{method="tools/call",outcome="ok"} 1`,
		`fold_upstream_requests_total{outcome="ok",upstream="u"}`,
		`fold_upstream_breaker_state{upstream="u"} 0`,
		`fold_build_info`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestTracePropagation(t *testing.T) {
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

	up, lastHeaders := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})
	session := connect(t, ts.URL, map[string]string{
		"traceparent": parent,
		"tracestate":  "vendor=1",
	})
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := lastHeader(lastHeaders, "traceparent"); got != parent {
		t.Errorf("upstream traceparent = %q, want %q", got, parent)
	}
	if got := lastHeader(lastHeaders, "tracestate"); got != "vendor=1" {
		t.Errorf("upstream tracestate = %q, want vendor=1", got)
	}
}
