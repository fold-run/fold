package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// newUpstreamServer runs a real SDK MCP server over streamable HTTP with one
// echo tool, recording the headers of the last tools/call-bearing request.
func newUpstreamServer(t *testing.T, toolNames ...string) (*httptest.Server, *atomic.Value) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	for _, name := range toolNames {
		server.AddTool(&mcp.Tool{
			Name:        name,
			Description: "fixture tool " + name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + req.Params.Name}},
			}, nil
		})
	}
	server.AddPrompt(&mcp.Prompt{Name: "greeting"}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: "hello"}},
		}}, nil
	})

	var lastHeaders atomic.Value
	lastHeaders.Store(http.Header{})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastHeaders.Store(r.Header.Clone())
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, &lastHeaders
}

func lastHeader(v *atomic.Value, name string) string {
	return v.Load().(http.Header).Get(name)
}

func startGateway(t *testing.T, cfg *config.Config) (*httptest.Server, *Gateway) {
	t.Helper()
	gw, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(gw.Close)
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)
	return ts, gw
}

func connect(t *testing.T, url string, headers map[string]string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "1.0"}, nil)
	httpClient := http.DefaultClient
	if headers != nil {
		httpClient = &http.Client{Transport: headerTransport{headers: headers}}
	}
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint:   url + "/mcp",
		HTTPClient: httpClient,
	}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

type headerTransport struct{ headers map[string]string }

func (h headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

func toolNames(res *mcp.ListToolsResult) []string {
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func TestFederationListAndCall(t *testing.T) {
	up1, _ := newUpstreamServer(t, "get_weather")
	up2, _ := newUpstreamServer(t, "search", "index")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "weather", URL: up1.URL, Namespace: "wx"},
		{ID: "search", URL: up2.URL, Namespace: "search"},
	}})
	session := connect(t, ts.URL, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	got := strings.Join(toolNames(res), ",")
	for _, want := range []string{"wx__get_weather", "search__search", "search__index"} {
		if !strings.Contains(got, want) {
			t.Errorf("tool list %q missing %q", got, want)
		}
	}

	out, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "wx__get_weather"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if text := out.Content[0].(*mcp.TextContent).Text; text != "echo:get_weather" {
		t.Errorf("got %q, want echo:get_weather", text)
	}
	if out.Meta[metaUpstream] != "weather" {
		t.Errorf("missing upstream meta tag, got %v", out.Meta)
	}

	// Unknown namespace answers the gateway's routing error.
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "nope__x"}); err == nil {
		t.Error("expected error for unknown namespace")
	}

	// Prompts federate with namespaces too.
	prompts, err := session.ListPrompts(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListPrompts: %v", err)
	}
	if len(prompts.Prompts) != 2 {
		t.Errorf("want 2 namespaced prompts, got %d", len(prompts.Prompts))
	}
	if _, err := session.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "wx__greeting"}); err != nil {
		t.Errorf("GetPrompt: %v", err)
	}
}

func TestPassthroughMode(t *testing.T) {
	up, _ := newUpstreamServer(t, "solo")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "only", URL: up.URL},
	}})
	session := connect(t, ts.URL, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := strings.Join(toolNames(res), ","); got != "solo" {
		t.Errorf("passthrough must not namespace: got %q", got)
	}
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "solo"}); err != nil {
		t.Errorf("CallTool: %v", err)
	}
}

func TestPolicyFilteringAndDenial(t *testing.T) {
	up1, _ := newUpstreamServer(t, "get_thing", "delete_thing")
	up2, _ := newUpstreamServer(t, "search")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{
			{ID: "things", URL: up1.URL, Namespace: "things"},
			{ID: "search", URL: up2.URL, Namespace: "search"},
		},
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID: "readers",
				Allow: []config.PolicyAllow{
					{Server: "things", Methods: []string{"tools/call"}, Names: []string{"get_*"}},
				},
			}},
		},
	})
	session := connect(t, ts.URL, nil)

	// List filtering: callers never see tools they cannot call.
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := strings.Join(toolNames(res), ","); got != "things__get_thing" {
		t.Errorf("filtered list = %q, want only things__get_thing", got)
	}

	// Call denial is the other half of the enforcement pair.
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "things__delete_thing"}); err == nil {
		t.Error("expected policy denial")
	} else if !strings.Contains(err.Error(), "policy denied") {
		t.Errorf("unexpected error: %v", err)
	}
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "things__get_thing"}); err != nil {
		t.Errorf("allowed call failed: %v", err)
	}
}

func TestPartialFailure(t *testing.T) {
	up, _ := newUpstreamServer(t, "alive")
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(dead.Close)

	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "alive", URL: up.URL, Namespace: "a"},
		{ID: "dead", URL: dead.URL, Namespace: "d", Timeouts: &config.Timeouts{ConnectMs: 500}},
	}})
	session := connect(t, ts.URL, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools should degrade gracefully, got: %v", err)
	}
	if got := strings.Join(toolNames(res), ","); got != "a__alive" {
		t.Errorf("tool list = %q, want a__alive", got)
	}
	pf, ok := res.Meta[metaPartialFailure].(map[string]any)
	if !ok {
		t.Fatalf("missing %s meta: %v", metaPartialFailure, res.Meta)
	}
	failed := fmt.Sprint(pf["failedUpstreams"])
	if !strings.Contains(failed, "dead") {
		t.Errorf("failedUpstreams = %v, want to contain dead", failed)
	}
}

func TestStaticUpstreamCredentials(t *testing.T) {
	up, lastAuth := newUpstreamServer(t, "tool")
	t.Setenv("TEST_UPSTREAM_KEY", "sekret")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "u", URL: up.URL, Auth: &config.UpstreamAuth{Strategy: "static", SecretRef: "TEST_UPSTREAM_KEY"}},
	}})
	session := connect(t, ts.URL, nil)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := lastHeader(lastAuth, "Authorization"); got != "Bearer sekret" {
		t.Errorf("upstream Authorization = %q, want Bearer sekret", got)
	}
}

func TestGlobalRateLimit(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "u", URL: up.URL}},
		Server:    &config.ServerSection{RateLimit: &config.RateLimit{RequestsPerMinute: 3}},
	})
	// Consume the budget with raw POSTs (the MCP handshake also counts).
	var last int
	for range 6 {
		resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		last = resp.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("want 429 after budget exhausted, got %d", last)
	}
}

func TestUpstreamRateLimitAndBreaker(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "u", URL: up.URL, RateLimit: &config.RateLimit{RequestsPerMinute: 2}, CacheTTLMs: -1},
	}})
	session := connect(t, ts.URL, nil)

	var rateLimited bool
	for range 4 {
		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil {
			if strings.Contains(err.Error(), "rate limit") {
				rateLimited = true
			}
		}
	}
	if !rateLimited {
		t.Error("expected per-upstream rate limit to trip")
	}
}

func TestHealthEndpoint(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "u", URL: up.URL, Owner: &config.Owner{Org: "acme", Team: "devex"}},
	}})
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	var body struct {
		Status    string           `json:"status"`
		Upstreams []upstreamHealth `json:"upstreams"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "ok" || len(body.Upstreams) != 1 || !body.Upstreams[0].Connected {
		t.Errorf("unexpected health: %+v", body)
	}
	if body.Upstreams[0].Owner == nil || body.Upstreams[0].Owner.Org != "acme" {
		t.Errorf("owner not surfaced in health: %+v", body.Upstreams[0])
	}
}

func TestHostValidation(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{{ID: "u", URL: up.URL}}})

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader("{}"))
	req.Host = "evil.example.com"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("want 403 for forbidden host, got %d", resp.StatusCode)
	}
}
