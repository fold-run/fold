package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/state"
)

// mutableUpstreamServer runs a real SDK server whose tool set the test can
// change mid-flight, so the decoded-list memo can be checked against an
// upstream that actually moves.
func mutableUpstreamServer(t *testing.T, names ...string) (*httptest.Server, *mcp.Server) {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	for _, name := range names {
		addFixtureTool(srv, name)
	}
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(ts.Close)
	return ts, srv
}

func addFixtureTool(srv *mcp.Server, name string) {
	srv.AddTool(&mcp.Tool{
		Name:        name,
		Description: "fixture tool " + name,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
}

func toolNamesOf(tools []*mcp.Tool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}

// A warm cache hit must not re-decode: the second call returns the very same
// slice the first one did. This is the whole point of the memo — decoding is
// linear in list size and was previously paid on every request, per upstream.
func TestCachedListMemoSkipsRedecode(t *testing.T) {
	ts, _ := mutableUpstreamServer(t, "alpha", "beta")
	u := newUpstream(config.Upstream{ID: "up", URL: ts.URL}, state.NewMemory())
	t.Cleanup(u.Close)
	ctx := context.Background()

	first, err := u.listTools(ctx)
	if err != nil {
		t.Fatalf("first listTools: %v", err)
	}
	second, err := u.listTools(ctx)
	if err != nil {
		t.Fatalf("second listTools: %v", err)
	}
	if len(first) != 2 {
		t.Fatalf("expected 2 tools, got %v", toolNamesOf(first))
	}
	if &first[0] != &second[0] {
		t.Error("warm cache hit re-decoded the list instead of reusing the memoized parse")
	}
}

// The memo is keyed by the identity of the cached bytes, so an invalidation
// (list_changed, TTL expiry, reload) must produce a fresh parse rather than
// serving the previous generation.
func TestCachedListMemoRefreshesAfterInvalidate(t *testing.T) {
	ts, srv := mutableUpstreamServer(t, "alpha")
	u := newUpstream(config.Upstream{ID: "up", URL: ts.URL}, state.NewMemory())
	t.Cleanup(u.Close)
	ctx := context.Background()

	before, err := u.listTools(ctx)
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if got := toolNamesOf(before); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("expected [alpha], got %v", got)
	}

	addFixtureTool(srv, "gamma")
	u.lists.Invalidate(ctx, "tools")

	after, err := u.listTools(ctx)
	if err != nil {
		t.Fatalf("listTools after invalidate: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("expected the refilled list to hold 2 tools, got %v", toolNamesOf(after))
	}
	if &before[0] == &after[0] {
		t.Error("served the stale memoized parse after the cache was invalidated")
	}
}

// Caching is disabled either explicitly or because the upstream's credential
// is caller-derived, where the list may differ per principal. Nothing may be
// memoized in that state either — every call must reach the upstream afresh,
// or the memo would reintroduce exactly the cross-principal leak the disabled
// cache exists to prevent.
func TestCachedListNotMemoizedWhenCachingDisabled(t *testing.T) {
	ts, srv := mutableUpstreamServer(t, "alpha")
	u := newUpstream(config.Upstream{ID: "up", URL: ts.URL, CacheTTLMs: -1}, state.NewMemory())
	t.Cleanup(u.Close)
	ctx := context.Background()

	if u.cacheTTL != 0 {
		t.Fatalf("expected caching to be disabled, got ttl %s", u.cacheTTL)
	}
	if _, err := u.listTools(ctx); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	// No invalidation: with caching off, the next call must still observe the
	// upstream's new tool.
	addFixtureTool(srv, "gamma")
	after, err := u.listTools(ctx)
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if len(after) != 2 {
		t.Fatalf("expected an uncached upstream to report 2 tools, got %v", toolNamesOf(after))
	}
	u.decodedMu.Lock()
	n := len(u.decoded)
	u.decodedMu.Unlock()
	if n != 0 {
		t.Errorf("memoized %d list(s) for an upstream with caching disabled", n)
	}
}

// Concurrent readers share one memoized parse; under -race this also pins
// that the shared slice is published safely.
func TestCachedListMemoConcurrentReaders(t *testing.T) {
	ts, _ := mutableUpstreamServer(t, "alpha", "beta", "gamma")
	u := newUpstream(config.Upstream{ID: "up", URL: ts.URL}, state.NewMemory())
	t.Cleanup(u.Close)
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			tools, err := u.listTools(ctx)
			if err != nil {
				t.Errorf("listTools: %v", err)
				return
			}
			if len(tools) != 3 {
				t.Errorf("expected 3 tools, got %v", toolNamesOf(tools))
			}
		})
	}
	wg.Wait()
}

// End to end: the federated list a client sees must still track an upstream
// that changes, with the memo in the path.
func TestFederatedListTracksUpstreamChange(t *testing.T) {
	ts, srv := mutableUpstreamServer(t, "alpha")
	gwServer, gw := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{{ID: "up", Namespace: "up", URL: ts.URL}},
	})
	session := connect(t, gwServer.URL, nil)
	ctx := context.Background()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := toolNames(res); len(got) != 1 || got[0] != "up__alpha" {
		t.Fatalf("expected [up__alpha], got %v", got)
	}

	addFixtureTool(srv, "gamma")
	for _, u := range gw.rt().upstreams {
		u.lists.Invalidate(ctx, "tools")
	}

	res, err = session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools after change: %v", err)
	}
	if got := toolNames(res); len(got) != 2 {
		t.Fatalf("expected both tools after the upstream changed, got %v", got)
	}
}

// The namespaced view is memoized alongside the decoded list, so it must
// follow the same generation: a refilled list rebuilds it rather than serving
// names from the previous one.
func TestNamespacedViewFollowsCacheGeneration(t *testing.T) {
	ts, srv := mutableUpstreamServer(t, "alpha")
	u := newUpstream(config.Upstream{ID: "up", Namespace: "ns", URL: ts.URL}, state.NewMemory())
	u.sep = "__"
	t.Cleanup(u.Close)
	ctx := context.Background()

	bare, err := u.listTools(ctx)
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	first := u.namespacedTools(context.Background(), bare)
	if got := toolNamesOf(first); len(got) != 1 || got[0] != "ns__alpha" {
		t.Fatalf("expected [ns__alpha], got %v", got)
	}
	// A second call on the same generation is the memoized view.
	if second := u.namespacedTools(context.Background(), bare); &second[0] != &first[0] {
		t.Error("rebuilt the namespaced view for an unchanged list")
	}
	// The bare list must not have been rewritten in place.
	if bare[0].Name != "alpha" {
		t.Errorf("namespacing mutated the shared bare list: got %q", bare[0].Name)
	}

	addFixtureTool(srv, "gamma")
	u.lists.Invalidate(ctx, "tools")
	bare, err = u.listTools(ctx)
	if err != nil {
		t.Fatalf("listTools after invalidate: %v", err)
	}
	after := u.namespacedTools(context.Background(), bare)
	if len(after) != 2 {
		t.Fatalf("expected 2 namespaced tools, got %v", toolNamesOf(after))
	}
	for _, name := range toolNamesOf(after) {
		if !strings.HasPrefix(name, "ns__") {
			t.Errorf("tool %q lost its namespace after the list refilled", name)
		}
	}
}

// An upstream with no namespace runs passthrough: the view is the bare list
// itself, with nothing copied and nothing rewritten.
func TestNamespacedViewPassthroughIsIdentity(t *testing.T) {
	ts, _ := mutableUpstreamServer(t, "alpha")
	u := newUpstream(config.Upstream{ID: "up", URL: ts.URL}, state.NewMemory())
	t.Cleanup(u.Close)

	bare, err := u.listTools(context.Background())
	if err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if public := u.namespacedTools(context.Background(), bare); &public[0] != &bare[0] {
		t.Error("passthrough upstream copied its list instead of passing it through")
	}
}
