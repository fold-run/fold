package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/stdiobridge"
)

// These tests federate a stdio MCP server through the shim, per the design in
// docs/design-stdio.md: a real SDK stdio server behind stdiobridge behind the
// gateway, driven by a real SDK client. The point is that the gateway needs no
// special case — a shimmed server is an ordinary http upstream — so everything
// asserted here is ordinary gateway behaviour reaching a process.
//
// The stdio server is this test binary re-execed with stdioServerEnv set.

const stdioServerEnv = "FOLD_GATEWAY_TEST_STDIO_SERVER"

func TestMain(m *testing.M) {
	if os.Getenv(stdioServerEnv) == "" {
		os.Exit(m.Run())
	}
	if err := runTestStdioServer(); err != nil {
		fmt.Fprintln(os.Stderr, "stdio server:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// runTestStdioServer serves an SDK MCP server over stdin/stdout.
func runTestStdioServer() error {
	s := mcp.NewServer(&mcp.Implementation{Name: "local-stdio", Version: "1.0"}, nil)
	for _, name := range []string{"read_file", "write_file"} {
		s.AddTool(&mcp.Tool{
			Name:        name,
			Description: "stdio tool " + name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "stdio:" + req.Params.Name}},
			}, nil
		})
	}
	return s.Run(context.Background(), &mcp.StdioTransport{})
}

// startShim runs the bridge in front of the test stdio server and returns its
// URL, which is what the gateway federates as an ordinary http upstream.
func startShim(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("executable: %v", err)
	}
	b, err := stdiobridge.New(stdiobridge.Options{
		Command: []string{exe},
		Env:     []string{stdioServerEnv + "=1"},
	})
	if err != nil {
		t.Fatalf("new bridge: %v", err)
	}
	ts := httptest.NewServer(b)
	t.Cleanup(func() {
		ts.Close()
		_ = b.Close()
	})
	return ts.URL
}

// A stdio server federated through the shim must namespace and call exactly
// like a native http upstream — no gateway special case.
func TestStdioUpstreamFederates(t *testing.T) {
	shim := startShim(t)
	http1, _ := newUpstreamServer(t, "get_weather")
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "files", URL: shim, Namespace: "files"},
		{ID: "weather", URL: http1.URL, Namespace: "wx"},
	}})
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := toolNames(res)
	for _, want := range []string{"files__read_file", "files__write_file", "wx__get_weather"} {
		if !containsStr(names, want) {
			t.Fatalf("tools = %v, want %q — stdio upstream did not federate", names, want)
		}
	}

	out, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "files__read_file"})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	text := out.Content[0].(*mcp.TextContent).Text
	if text != "stdio:read_file" {
		t.Fatalf("call result = %q, want %q — the bare name must reach the process", text, "stdio:read_file")
	}
}

// Policy must govern a shimmed server exactly as it governs any other: the
// denied tool is invisible in the list and denied on call. This is the
// enforcement pair, reaching a local process.
func TestStdioUpstreamObeysPolicy(t *testing.T) {
	shim := startShim(t)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "files", URL: shim, Namespace: "files"}},
		Policy: &config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:    "readonly",
				Allow: []config.PolicyAllow{{Server: "files", Methods: []string{"tools/list", "tools/call"}, Names: []string{"read_file"}}},
			}},
		},
	})
	session := connect(t, ts.URL, nil)
	ctx := context.Background()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	names := toolNames(res)
	if !containsStr(names, "files__read_file") {
		t.Fatalf("tools = %v, want the allowed tool", names)
	}
	if containsStr(names, "files__write_file") {
		t.Fatalf("tools = %v — a denied tool must be invisible", names)
	}

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "files__write_file"}); err == nil {
		t.Fatal("denied tool call succeeded, want -31042")
	} else if !strings.Contains(err.Error(), "-31042") && !strings.Contains(err.Error(), "denied") {
		t.Fatalf("error = %v, want a policy denial", err)
	}
}

// Audit is the single exit door, and a call reaching a subprocess is no
// exception: it must produce one event naming the upstream. The events are
// collected through the real webhook sink rather than an injected one, so this
// exercises the path an operator actually configures.
func TestStdioUpstreamIsAudited(t *testing.T) {
	shim := startShim(t)

	var mu sync.Mutex
	var events []audit.Event
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []audit.Event
		if err := json.NewDecoder(r.Body).Decode(&batch); err == nil {
			mu.Lock()
			events = append(events, batch...)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(collector.Close)

	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "files", URL: shim, Namespace: "files"}},
		Audit:     &config.Audit{Sinks: []config.AuditSink{{Type: "webhook", URL: collector.URL}}},
	})

	session := connect(t, ts.URL, nil)
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "files__read_file"}); err != nil {
		t.Fatalf("call: %v", err)
	}

	// The sink is async by design (it must not add latency to the request
	// path), so poll rather than assume delivery has happened.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		found := findCallEvent(events, "files__read_file")
		mu.Unlock()
		if found != nil {
			if found.Upstream != "files" {
				t.Fatalf("audit upstream = %q, want %q", found.Upstream, "files")
			}
			if found.Outcome != audit.OutcomeOK {
				t.Fatalf("audit outcome = %q, want ok", found.Outcome)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("no audit event for the stdio call; got %d events", len(events))
}

func findCallEvent(events []audit.Event, name string) *audit.Event {
	for i, e := range events {
		if e.Method == "tools/call" && e.Name == name {
			return &events[i]
		}
	}
	return nil
}

func containsStr(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}
