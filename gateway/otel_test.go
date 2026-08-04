package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// TestTracingExportsSpans: with tracing configured, requests produce spans
// that reach the OTLP/HTTP collector; Close flushes the batcher.
func TestTracingExportsSpans(t *testing.T) {
	var exports atomic.Int64
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/traces" {
			exports.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)

	up, _ := newUpstreamServer(t, "traced_tool")
	gw, err := New(&config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Tracing:   &config.Tracing{OTLPEndpoint: collector.URL},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(gw.Close)
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)

	session := connect(t, ts.URL, nil)
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "traced_tool"}); err != nil {
		t.Fatal(err)
	}
	session.Close()

	gw.Close() // shutdown flushes buffered spans
	if exports.Load() == 0 {
		t.Fatal("no spans reached the OTLP collector")
	}
}

// TestTracingJoinsCallerTrace: an incoming traceparent keeps its trace id
// end to end, but the upstream sees fold's client span as its parent — the
// gateway hop appears in the trace instead of being invisible.
func TestTracingJoinsCallerTrace(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(collector.Close)

	up, lastHeaders := newUpstreamServer(t, "traced_tool")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Tracing:   &config.Tracing{OTLPEndpoint: collector.URL},
	})

	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	const parentSpan = "00f067aa0ba902b7"
	parent := "00-" + traceID + "-" + parentSpan + "-01"
	session := connect(t, ts.URL, map[string]string{"traceparent": parent})
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "traced_tool"}); err != nil {
		t.Fatal(err)
	}

	got := lastHeader(lastHeaders, "traceparent")
	if !strings.Contains(got, traceID) {
		t.Errorf("upstream traceparent %q lost the caller's trace id %q", got, traceID)
	}
	if got == parent || strings.Contains(got, parentSpan) {
		t.Errorf("upstream traceparent %q should carry fold's span, not the caller's span id", got)
	}
}
