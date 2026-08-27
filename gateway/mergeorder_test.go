package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// Federated list order is upstream configuration order, then each upstream's
// own list order. That is not a nicety: fold's list cursors are offsets into
// the merged, filtered snapshot, fingerprinted by the *sequence* of names
// (snapshotGen, paginate.go), so a merge that varied with which upstream
// answered first would either refuse every continuation or — on a fingerprint
// collision — serve a window into a differently ordered list, showing a client
// one item twice and another never. The 2026-07-28 revision's "SHOULD return
// tools in a deterministic order" is the weaker of the two reasons.
//
// fanOut writes results[i] by upstream index rather than appending on
// completion, which is what makes the property hold. These tests invert
// completion order deliberately so that a change to that would be red here
// rather than flaky elsewhere: TestPaginatedListWalk asserts the same order
// with two equally fast upstreams, where a completion-ordered merge would fail
// only sometimes.

// orderFixture starts n namespaced upstreams whose list replies are chained in
// reverse: upstream i does not answer a list until upstream i+1 already has.
// The last upstream in configuration order therefore always answers first.
// There are no sleeps and no timing assumptions, so the inversion is certain
// rather than likely.
type orderFixture struct {
	servers []*httptest.Server
	names   [][]string // tools each upstream registered, in its own list order
}

func newOrderFixture(t *testing.T, n, toolsEach int) *orderFixture {
	t.Helper()
	echo := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}

	// answered[i] closes once upstream i has served a tools/list.
	answered := make([]chan struct{}, n)
	once := make([]sync.Once, n)
	for i := range answered {
		answered[i] = make(chan struct{})
	}

	f := &orderFixture{servers: make([]*httptest.Server, n), names: make([][]string, n)}
	for i := range n {
		prefix := fmt.Sprintf("up%d", i)
		s := mcp.NewServer(&mcp.Implementation{Name: prefix, Version: "1.0"}, nil)
		for j := range toolsEach {
			name := fmt.Sprintf("%s_tool_%d", prefix, j)
			f.names[i] = append(f.names[i], name)
			s.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)}, echo)
		}
		s.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
			return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
				if method == "tools/list" && i+1 < n {
					select {
					case <-answered[i+1]:
					case <-time.After(10 * time.Second):
						t.Errorf("upstream %d waited out its predecessor: fanOut appears to be "+
							"listing upstreams sequentially, so this test is no longer inverting "+
							"completion order and proves nothing", i)
					}
				}
				res, err := next(ctx, method, req)
				if method == "tools/list" {
					once[i].Do(func() { close(answered[i]) })
				}
				return res, err
			}
		})
		srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
		t.Cleanup(srv.Close)
		f.servers[i] = srv
	}
	return f
}

// want returns the merged, namespaced tool names in the order fold must serve.
func (f *orderFixture) want() []string {
	var out []string
	for i, names := range f.names {
		for _, n := range names {
			out = append(out, fmt.Sprintf("up%d__%s", i, n))
		}
	}
	return out
}

func (f *orderFixture) upstreams() []config.Upstream {
	ups := make([]config.Upstream, len(f.servers))
	for i, s := range f.servers {
		ups[i] = config.Upstream{ID: fmt.Sprintf("up%d", i), URL: s.URL, Namespace: fmt.Sprintf("up%d", i)}
	}
	return ups
}

func listedToolNames(t *testing.T, session *mcp.ClientSession) []string {
	t.Helper()
	var names []string
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	return names
}

func TestFederatedListOrderIsConfigOrderNotCompletionOrder(t *testing.T) {
	f := newOrderFixture(t, 4, 3)
	ts, _ := startGateway(t, &config.Config{Upstreams: f.upstreams()})
	session := connect(t, ts.URL, nil)

	got := listedToolNames(t, session)
	want := f.want()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tools/list came back in completion order, not configuration order:\n got %v\nwant %v\n"+
			"fold's pagination cursors (paginate.go) are offsets into this merged snapshot, so a "+
			"non-deterministic merge order does not merely miss the 2026-07-28 SHOULD — it makes a "+
			"client walking pages see an item twice or never. fanOut (router.go) must keep writing "+
			"results[i] by upstream index; do not switch it to appending on completion.", got, want)
	}
}

func TestListOrderStableAcrossCacheGenerations(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cacheTTLMs int
	}{
		{"cached", 0},    // 0 → the 30s default
		{"uncached", -1}, // negative disables caching: no list cache, no decode memo
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOrderFixture(t, 3, 3)
			ups := f.upstreams()
			for i := range ups {
				ups[i].CacheTTLMs = tc.cacheTTLMs
			}
			ts, _ := startGateway(t, &config.Config{Upstreams: ups})
			session := connect(t, ts.URL, nil)

			first := listedToolNames(t, session)
			second := listedToolNames(t, session)
			if strings.Join(first, ",") != strings.Join(second, ",") {
				t.Errorf("list order changed between generations:\nfirst  %v\nsecond %v\n"+
					"a cached list and a freshly fetched one must merge identically, or a cursor "+
					"minted against one is a window into the other", first, second)
			}
			if want := f.want(); strings.Join(first, ",") != strings.Join(want, ",") {
				t.Errorf("merged order %v, want %v", first, want)
			}
		})
	}
}

func TestPaginatedWalkSurvivesInvertedCompletionOrder(t *testing.T) {
	f := newOrderFixture(t, 4, 3)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: f.upstreams(),
		Routing:   &config.Routing{PageSize: 5},
	})
	session := connect(t, ts.URL, nil)

	got := listedToolNames(t, session)
	want := f.want()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("paginated walk returned %v, want %v", got, want)
	}
	seen := map[string]int{}
	for _, n := range got {
		seen[n]++
	}
	for n, c := range seen {
		if c != 1 {
			t.Errorf("%s appeared %d times across the paginated walk; a page is a window into the "+
				"merged snapshot and the merge order must not move between pages", n, c)
		}
	}
}

// TestCursorMintedBeforeAnUpstreamDies pins the two halves of the partial-
// failure case. A cursor is an offset plus a fingerprint of the name sequence
// it was minted against, so an upstream that disappears mid-walk is caught by
// the fingerprint rather than silently shifting every later item down a slot.
// The complementary case — an upstream that contributed nothing visible — must
// keep working, because the pages the client is walking are unaffected.
func TestCursorMintedBeforeAnUpstreamDies(t *testing.T) {
	t.Run("contributed items", func(t *testing.T) {
		alive := newFlakyUpstream(t, "dies", 3)
		keep := plainUpstream(t, "keep", 4)
		ts, _ := startGateway(t, &config.Config{
			Upstreams: []config.Upstream{
				{ID: "keep", URL: keep, Namespace: "keep"},
				{ID: "dies", URL: alive.url, Namespace: "dies", CacheTTLMs: -1,
					Timeouts: &config.Timeouts{ConnectMs: 500, RequestMs: 500}},
			},
			Routing: &config.Routing{PageSize: 3},
		})
		session := connect(t, ts.URL, nil)

		first, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if first.NextCursor == "" {
			t.Fatal("first page carries no cursor; raise the fixture's tool count")
		}
		alive.kill()

		_, err = session.ListTools(context.Background(), &mcp.ListToolsParams{Cursor: first.NextCursor})
		if err == nil {
			t.Fatal("a cursor minted before an upstream died was honoured against a shorter " +
				"snapshot: every item after the dead upstream's slot shifts down, so the client " +
				"silently skips one. snapshotGen must fingerprint the name sequence.")
		}
		var jerr *jsonrpc.Error
		if !asWireError(err, &jerr) || jerr.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("stale cursor answered %v, want -32602 invalid params", err)
		}
	})

	t.Run("contributed nothing", func(t *testing.T) {
		alive := newFlakyUpstream(t, "empty", 0)
		keep := plainUpstream(t, "keep", 6)
		ts, _ := startGateway(t, &config.Config{
			Upstreams: []config.Upstream{
				{ID: "keep", URL: keep, Namespace: "keep"},
				{ID: "empty", URL: alive.url, Namespace: "empty", CacheTTLMs: -1,
					Timeouts: &config.Timeouts{ConnectMs: 500, RequestMs: 500}},
			},
			Routing: &config.Routing{PageSize: 3},
		})
		session := connect(t, ts.URL, nil)

		first, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		alive.kill()

		rest, err := session.ListTools(context.Background(), &mcp.ListToolsParams{Cursor: first.NextCursor})
		if err != nil {
			t.Fatalf("an upstream that contributed no visible items died and invalidated a cursor "+
				"anyway: the name sequence did not change, so the walk should have continued: %v", err)
		}
		if len(first.Tools)+len(rest.Tools) != 6 {
			t.Errorf("walk saw %d tools, want 6", len(first.Tools)+len(rest.Tools))
		}
	})
}

func TestPartialFailureKeepsSurvivorOrder(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	t.Cleanup(dead.Close)

	a, c, d := plainUpstream(t, "a", 2), plainUpstream(t, "c", 2), plainUpstream(t, "d", 2)
	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{
			{ID: "a", URL: a, Namespace: "a"},
			{ID: "b", URL: dead.URL, Namespace: "b", Timeouts: &config.Timeouts{ConnectMs: 500}},
			{ID: "c", URL: c, Namespace: "c"},
			{ID: "d", URL: d, Namespace: "d"},
		},
	})
	session := connect(t, ts.URL, nil)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	want := []string{"a__a_tool_0", "a__a_tool_1", "c__c_tool_0", "c__c_tool_1", "d__d_tool_0", "d__d_tool_1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("survivor order %v, want %v — a failed upstream contributes nothing and must not "+
			"reorder the upstreams around it", got, want)
	}
	if res.Meta[metaPartialFailure] == nil {
		t.Error("a partially failed list carried no partial-failure marker")
	}
}

// plainUpstream starts a namespaced upstream with n tools and returns its URL.
func plainUpstream(t *testing.T, prefix string, n int) string {
	t.Helper()
	return newFlakyUpstream(t, prefix, n).url
}

// flakyUpstream is a real SDK server behind a handler that can be made to stop
// answering, so a federation can lose an upstream mid-test.
type flakyUpstream struct {
	url  string
	kill func()
}

func newFlakyUpstream(t *testing.T, prefix string, n int) *flakyUpstream {
	t.Helper()
	echo := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	s := mcp.NewServer(&mcp.Implementation{Name: prefix, Version: "1.0"}, nil)
	for i := range n {
		s.AddTool(&mcp.Tool{
			Name:        fmt.Sprintf("%s_tool_%d", prefix, i),
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, echo)
	}
	inner := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)

	var mu sync.RWMutex
	down := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		dead := down
		mu.RUnlock()
		if dead {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return &flakyUpstream{url: srv.URL, kill: func() {
		mu.Lock()
		down = true
		mu.Unlock()
	}}
}
