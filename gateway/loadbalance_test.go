package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// countingUpstreamServer runs a real SDK MCP server that counts the sessions
// established against it (initialize requests), so tests can observe where
// the balancer sent new connections.
func countingUpstreamServer(t *testing.T, toolNames ...string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	for _, name := range toolNames {
		server.AddTool(&mcp.Tool{
			Name:        name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + req.Params.Name}},
			}, nil
		})
	}
	var sessions atomic.Int64
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.Body != nil {
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if bytes.Contains(body, []byte(`"initialize"`)) {
				sessions.Add(1)
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, &sessions
}

// TestLoadBalanceDistributesSessions: with urls, new sessions (the shared
// root session plus each client's bridged session) rotate round-robin across
// replicas, so several clients end up spread over both endpoints.
func TestLoadBalanceDistributesSessions(t *testing.T) {
	epA, sessionsA := countingUpstreamServer(t, "ping_tool")
	epB, sessionsB := countingUpstreamServer(t, "ping_tool")

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "lb", URLs: []string{epA.URL, epB.URL}},
	}})

	// Three clients: lists share the root session; each client's tools/call
	// gets its own bridged session, born round-robin.
	for range 3 {
		session := connect(t, ts.URL, nil)
		if _, err := session.ListTools(context.Background(), nil); err != nil {
			t.Fatalf("ListTools: %v", err)
		}
		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping_tool"}); err != nil {
			t.Fatalf("CallTool: %v", err)
		}
	}

	a, b := sessionsA.Load(), sessionsB.Load()
	if a == 0 || b == 0 {
		t.Errorf("sessions were not balanced across endpoints: endpointA=%d endpointB=%d", a, b)
	}
}

// TestLoadBalanceFailover: a dead replica listed first must not take the
// upstream down — connect skips to the healthy endpoint, the request
// succeeds, and the balancer reports the dead endpoint out of rotation.
func TestLoadBalanceFailover(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // connection refused from here on

	live, _ := countingUpstreamServer(t, "ping_tool")
	ts, gw := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "lb", URLs: []string{deadURL, live.URL}},
	}})

	session := connect(t, ts.URL, nil)
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools through failover: %v", err)
	}
	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "ping_tool"})
	if err != nil {
		t.Fatalf("CallTool through failover: %v", err)
	}
	if text := out.Content[0].(*mcp.TextContent).Text; text != "echo:ping_tool" {
		t.Errorf("got %q, want echo:ping_tool", text)
	}

	// The dead endpoint is ejected; the live one is in rotation.
	statuses := gw.rt().byID["lb"].endpoints.snapshot(true)
	byURL := map[string]bool{}
	for _, s := range statuses {
		byURL[s.URL] = s.Healthy
	}
	if byURL[deadURL] {
		t.Errorf("dead endpoint %s still marked healthy: %+v", deadURL, statuses)
	}
	if !byURL[live.URL] {
		t.Errorf("live endpoint %s marked unhealthy: %+v", live.URL, statuses)
	}

	// /healthz surfaces the per-endpoint view (detailed: auth disabled).
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var health struct {
		Upstreams []struct {
			ID        string `json:"id"`
			Endpoints []struct {
				URL     string `json:"url"`
				Healthy bool   `json:"healthy"`
			} `json:"endpoints"`
		} `json:"upstreams"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if len(health.Upstreams) != 1 || len(health.Upstreams[0].Endpoints) != 2 {
		t.Fatalf("healthz missing per-endpoint view: %+v", health)
	}
}

// TestEndpointPoolRotationAndCooldown covers the balancer's contract:
// round-robin rotation, ejected endpoints ordered last but never dropped,
// and recovery after the cooldown.
func TestEndpointPoolRotationAndCooldown(t *testing.T) {
	p := newEndpointPool([]string{"a", "b"}, 25*time.Millisecond)

	if got := p.candidates(); got[0] != "a" || got[1] != "b" {
		t.Errorf("first pick = %v, want [a b]", got)
	}
	if got := p.candidates(); got[0] != "b" || got[1] != "a" {
		t.Errorf("rotated pick = %v, want [b a]", got)
	}

	p.markDown("a")
	for range 3 {
		if got := p.candidates(); got[0] != "b" || got[1] != "a" {
			t.Errorf("with a ejected, pick = %v, want [b a]", got)
		}
	}
	time.Sleep(30 * time.Millisecond)
	seenAFirst := false
	for range 2 {
		if p.candidates()[0] == "a" {
			seenAFirst = true
		}
	}
	if !seenAFirst {
		t.Error("endpoint a never returned to rotation after cooldown")
	}

	p.markDown("b")
	p.markDown("a")
	if got := p.candidates(); len(got) != 2 {
		t.Errorf("all-ejected pool must still offer every endpoint, got %v", got)
	}
	p.markUp("a")
	if s := p.snapshot(true); !s[0].Healthy || s[1].Healthy {
		t.Errorf("snapshot after markUp(a) = %+v, want a healthy, b not", s)
	}
}
