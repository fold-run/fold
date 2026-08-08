package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// pagedFixture starts two namespaced upstreams (4 + 3 tools) behind a
// gateway configured with the given page size.
func pagedFixture(t *testing.T, pageSize int) *mcp.ClientSession {
	t.Helper()
	echo := func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil
	}
	mkServer := func(prefix string, n int) *httptest.Server {
		s := mcp.NewServer(&mcp.Implementation{Name: prefix, Version: "1.0"}, nil)
		for i := range n {
			s.AddTool(&mcp.Tool{
				Name:        fmt.Sprintf("%s_tool_%d", prefix, i),
				InputSchema: json.RawMessage(`{"type":"object"}`),
			}, echo)
		}
		srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
		t.Cleanup(srv.Close)
		return srv
	}
	upA, upB := mkServer("alpha", 4), mkServer("beta", 3)

	ts, _ := startGateway(t, &config.Config{
		Upstreams: []config.Upstream{
			{ID: "a", URL: upA.URL, Namespace: "a"},
			{ID: "b", URL: upB.URL, Namespace: "b"},
		},
		Routing: &config.Routing{PageSize: pageSize},
	})
	return connect(t, ts.URL, nil)
}

func TestPaginatedListWalk(t *testing.T) {
	session := pagedFixture(t, 3)

	// First page: bounded, with a cursor to the rest.
	first, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Tools) != 3 {
		t.Errorf("first page has %d tools, want 3", len(first.Tools))
	}
	if first.NextCursor == "" {
		t.Fatal("first page carries no nextCursor")
	}

	// The SDK iterator walks fold's cursors transparently; every tool must
	// appear exactly once, namespaced, in deterministic order.
	var names []string
	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	want := []string{
		"a__alpha_tool_0", "a__alpha_tool_1", "a__alpha_tool_2", "a__alpha_tool_3",
		"b__beta_tool_0", "b__beta_tool_1", "b__beta_tool_2",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("walked %v, want %v", names, want)
	}
}

func TestCursorRejections(t *testing.T) {
	session := pagedFixture(t, 3)

	first, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	valid, ok := decodeCursor(first.NextCursor)
	if !ok {
		t.Fatalf("gateway minted an undecodable cursor %q", first.NextCursor)
	}

	tampered := func(mutate func(*listCursor)) string {
		c := valid
		mutate(&c)
		return encodeCursor(c)
	}
	cases := map[string]string{
		"garbage":             "not-a-cursor",
		"wrong kind":          tampered(func(c *listCursor) { c.Kind = "prompts" }),
		"stale generation":    tampered(func(c *listCursor) { c.Gen = "deadbeef0000" }),
		"foreign principal":   tampered(func(c *listCursor) { c.Principal = "deadbeef0000" }),
		"offset out of range": tampered(func(c *listCursor) { c.Offset = 99 }),
		"zero offset":         tampered(func(c *listCursor) { c.Offset = 0 }),
	}
	for name, cursor := range cases {
		_, err := session.ListTools(context.Background(), &mcp.ListToolsParams{Cursor: cursor})
		if err == nil {
			t.Errorf("%s cursor accepted", name)
			continue
		}
		var wire *jsonrpc.Error
		if !asWireError(err, &wire) || wire.Code != jsonrpc.CodeInvalidParams {
			t.Errorf("%s cursor: error %v, want -32602", name, err)
		}
	}

	// The untampered cursor still works after all the rejected attempts.
	rest, err := session.ListTools(context.Background(), &mcp.ListToolsParams{Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("valid cursor rejected: %v", err)
	}
	if len(rest.Tools) != 3 {
		t.Errorf("second page has %d tools, want 3", len(rest.Tools))
	}
}

func TestPaginationDisabled(t *testing.T) {
	session := pagedFixture(t, -1)

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 7 || res.NextCursor != "" {
		t.Errorf("disabled pagination: %d tools, nextCursor=%q; want 7 in one page", len(res.Tools), res.NextCursor)
	}
	// fold never minted a cursor in this mode, so any cursor is invalid.
	if _, err := session.ListTools(context.Background(), &mcp.ListToolsParams{Cursor: "anything"}); err == nil {
		t.Error("cursor accepted while pagination is disabled")
	}
}

// The fingerprint's byte stream is wire-visible: it is baked into every
// cursor a client holds. Hashing the names straight off the items must
// produce exactly what building a []string first produced, or a client
// paging across a gateway upgrade would be told to restart the list.
func TestSnapshotGenByteStreamUnchanged(t *testing.T) {
	names := []string{"", "alpha", "ns__beta", "with\x00nul", "üñî"}

	// The pre-existing formulation, kept here as the reference.
	reference := func(kind string, names []string) string {
		h := sha256.New()
		h.Write([]byte(kind))
		for _, n := range names {
			h.Write([]byte{0})
			h.Write([]byte(n))
		}
		return hex.EncodeToString(h.Sum(nil)[:6])
	}

	for _, kind := range []string{"tools", "prompts", "resources", "resourceTemplates"} {
		for i := range names {
			subset := names[:i]
			want := reference(kind, subset)
			got := snapshotGen(kind, subset, func(s string) string { return s })
			if got != want {
				t.Errorf("snapshotGen(%q, %q) = %s, want %s", kind, subset, got, want)
			}
		}
	}
}
