package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// The federated list path is the one request whose cost scales with the size
// of the federation: every upstream's list is fetched, merged, policy-filtered
// per principal, namespaced, and fingerprinted for the cursor. The added-
// latency gate measures a single upstream with a single tool, so none of that
// scaling shows up there. These benchmarks measure it gateway-side — warm
// caches, no client, no network — where the numbers are attributable to fold
// rather than to the calling SDK.
//
//	go test ./gateway -run '^$' -bench BenchmarkFederatedListTools -benchmem

func benchListUpstream(b *testing.B, namespace string, tools int) *httptest.Server {
	b.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: namespace, Version: "1.0"}, nil)
	// A representative schema: real tool lists are dominated by their input
	// schemas, which is what made decoding them per request expensive.
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"the search query to run against the corpus"},"limit":{"type":"integer"},"filters":{"type":"array","items":{"type":"string"}}},"required":["query"]}`)
	for i := range tools {
		srv.AddTool(&mcp.Tool{
			Name:        fmt.Sprintf("%s-tool-%03d", namespace, i),
			Description: "a representative tool with a representative description string",
			InputSchema: schema,
		}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{}, nil
		})
	}
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	b.Cleanup(ts.Close)
	return ts
}

// benchPolicy is a policy every listed name must be matched against: several
// non-matching rules ahead of the one that allows, with globs throughout.
// Policy filters lists per principal, so it runs once per item per request —
// the variant below is what would catch a per-item allocation creeping back
// into the engine.
func benchPolicy() *config.Policy {
	var rules []config.PolicyRule
	for i := range 8 {
		rules = append(rules, config.PolicyRule{
			ID:       fmt.Sprintf("rule-%d", i),
			Subjects: &config.PolicySubjects{Groups: []string{fmt.Sprintf("group-%d", i)}},
			Allow: []config.PolicyAllow{{
				Server: "*", Methods: []string{"tools/call"},
				Names: []string{"*-tool-*", "admin-*", "search-*"},
			}},
		})
	}
	rules = append(rules, config.PolicyRule{
		ID: "allow-anonymous",
		Allow: []config.PolicyAllow{{
			Server: "*", Methods: []string{"tools/call"}, Names: []string{"*"},
		}},
	})
	return &config.Policy{DefaultDecision: "deny", Rules: rules}
}

func BenchmarkFederatedListTools(b *testing.B) {
	for _, tc := range []struct {
		upstreams, tools int
		policy           bool
	}{{1, 10, false}, {5, 20, false}, {20, 50, false}, {20, 50, true}} {
		name := fmt.Sprintf("up%d_tools%d", tc.upstreams, tc.tools)
		if tc.policy {
			name += "_policy"
		}
		b.Run(name, func(b *testing.B) {
			cfg := &config.Config{}
			for i := range tc.upstreams {
				ns := fmt.Sprintf("ns%02d", i)
				ts := benchListUpstream(b, ns, tc.tools)
				cfg.Upstreams = append(cfg.Upstreams, config.Upstream{ID: ns, Namespace: ns, URL: ts.URL})
			}
			if tc.policy {
				cfg.Policy = benchPolicy()
			}
			g, err := New(cfg)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(g.Close)

			rt := g.rt()
			ctx := context.Background()
			req := &mcp.ListToolsRequest{Params: &mcp.ListToolsParams{}}
			if _, err := g.listTools(ctx, rt, req, nil); err != nil { // warm the list caches
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := g.listTools(ctx, rt, req, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkUpstreamListCacheHit isolates one warm cache hit: what every list
// request pays per upstream before any merging happens.
func BenchmarkUpstreamListCacheHit(b *testing.B) {
	for _, tools := range []int{10, 50, 200} {
		b.Run(fmt.Sprintf("tools%d", tools), func(b *testing.B) {
			ts := benchListUpstream(b, "up", tools)
			cfg := &config.Config{Upstreams: []config.Upstream{{ID: "up", URL: ts.URL}}}
			g, err := New(cfg)
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(g.Close)
			u := g.rt().upstreams[0]
			ctx := context.Background()
			if _, err := u.listTools(ctx); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if _, err := u.listTools(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// sink keeps the deferred-options benchmark's closure from being optimized
// away, so its allocation is measured honestly.
var sink bridgeOpts

// BenchmarkBridgeOptions separates what a named invocation pays in steady
// state from what only the connect path pays. Every tools/call and
// prompts/get used to build the full options and, on an already-open bridged
// session, throw them away unread.
func BenchmarkBridgeOptions(b *testing.B) {
	cfg := &config.Config{Upstreams: []config.Upstream{{ID: "up", URL: "http://127.0.0.1:1"}}}
	g, err := New(cfg)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(g.Close)
	ss := &mcp.ServerSession{}
	u := g.rt().upstreams[0]

	// What every named invocation pays now: just the thunk.
	b.Run("deferred", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			sink = func() *mcp.ClientOptions { return g.bridgeOptions(ss, u) }
		}
	})
	// What only a bridged-session connect pays.
	b.Run("built", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_ = g.bridgeOptions(ss, u)
		}
	})
}
