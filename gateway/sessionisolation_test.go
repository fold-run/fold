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
)

// sessionWitness is an upstream that records which caller credentials were
// seen on each MCP session it minted. It is the only vantage point from which
// the bug this file guards is visible: the gateway's own bookkeeping looks
// correct either way, because credentials really are attached per request —
// it is the *session* they are attached to that was shared.
type sessionWitness struct {
	*httptest.Server
	mu     sync.Mutex
	tokens map[string]map[string]bool // upstream session id → caller tokens seen
}

func newSessionWitness(t *testing.T, toolNames ...string) *sessionWitness {
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

	w := &sessionWitness{tokens: map[string]map[string]bool{}}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		sid := r.Header.Get("Mcp-Session-Id")
		handler.ServeHTTP(rw, r)
		if sid == "" {
			// initialize: the id the upstream just minted is on the response.
			sid = rw.Header().Get("Mcp-Session-Id")
		}
		if sid == "" || token == "" {
			return
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if w.tokens[sid] == nil {
			w.tokens[sid] = map[string]bool{}
		}
		w.tokens[sid][token] = true
	}))
	t.Cleanup(w.Close)
	return w
}

// crossed returns the callers sharing any one upstream session.
func (w *sessionWitness) crossed() (sid string, tokens int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, seen := range w.tokens {
		if len(seen) > 1 {
			return id, len(seen)
		}
	}
	return "", 0
}

func (w *sessionWitness) sessions() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.tokens)
}

// TestCallerDerivedUpstreamsGetTheirOwnSession is the regression gate for the
// shared-root-session defect.
//
// Credentials are attached per outgoing request, so a naive reading says a
// shared session is fine. It is not. The SDK detaches the connection context
// but deliberately preserves its values ("which may be necessary for auth
// middleware"), so the caller who happened to open the session is the
// identity that established it: its initialize, its standalone SSE stream and
// every reconnect of that stream carry that first caller's credential for as
// long as the connection lives. An upstream that scopes anything to the
// session it minted — which is the normal thing for a remote MCP server that
// authenticates — would then answer one caller from another's session.
//
// The assertion is made at the upstream, because that is where the crossing
// is observable at all.
func TestCallerDerivedUpstreamsGetTheirOwnSession(t *testing.T) {
	iss := newFixtureIssuer(t)
	up := newSessionWitness(t, "tool")
	ts, _ := startGateway(t, authedConfig(iss, []config.Upstream{
		{ID: "u", URL: up.URL, Auth: &config.UpstreamAuth{Strategy: "passthrough"}, CacheTTLMs: -1},
	}, nil))

	// Every method that runs on the root session, exercised by two callers.
	for _, who := range []string{"alice", "bob"} {
		token := iss.mint(t, who, "https://gw.example.com", nil)
		session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + token})
		ctx := context.Background()
		if _, err := session.ListTools(ctx, nil); err != nil {
			t.Fatalf("%s ListTools: %v", who, err)
		}
		if _, err := session.ListResources(ctx, nil); err != nil {
			t.Fatalf("%s ListResources: %v", who, err)
		}
		if _, err := session.ListPrompts(ctx, nil); err != nil {
			t.Fatalf("%s ListPrompts: %v", who, err)
		}
	}

	if sid, n := up.crossed(); n > 0 {
		t.Fatalf("upstream session %q served %d different callers: a caller-derived "+
			"credential must not share the session it established", sid, n)
	}
	if got := up.sessions(); got < 2 {
		t.Fatalf("two callers produced %d upstream sessions, want one each", got)
	}
}

// TestConfiguredCredentialUpstreamsStillShareOneSession proves the
// partitioning is confined to caller-derived credentials. An upstream whose
// credential is the gateway's own has one identity no matter who asks, so
// splitting its session per caller would multiply upstream connections and
// break the shared list cache for no benefit.
func TestConfiguredCredentialUpstreamsStillShareOneSession(t *testing.T) {
	iss := newFixtureIssuer(t)
	up := newSessionWitness(t, "tool")
	t.Setenv("UPSTREAM_KEY", "static-secret")
	ts, _ := startGateway(t, authedConfig(iss, []config.Upstream{
		{ID: "u", URL: up.URL, Auth: &config.UpstreamAuth{
			Strategy: "static", SecretRef: "UPSTREAM_KEY",
		}},
	}, nil))

	for _, who := range []string{"alice", "bob"} {
		session := connect(t, ts.URL, map[string]string{
			"Authorization": "Bearer " + iss.mint(t, who, "https://gw.example.com", nil),
		})
		if _, err := session.ListTools(context.Background(), nil); err != nil {
			t.Fatalf("%s ListTools: %v", who, err)
		}
	}

	if got := up.sessions(); got != 1 {
		t.Errorf("a gateway-credentialed upstream opened %d sessions, want 1 shared", got)
	}
}

// TestPerCallerSessionsAreSweptWhenIdle proves the per-caller map is bounded.
// It is keyed by verified principal — an identifier the gateway does not
// choose — so without the sweep it would grow with the caller population, the
// same defect the bounded-cache rule exists to prevent.
func TestPerCallerSessionsAreSweptWhenIdle(t *testing.T) {
	iss := newFixtureIssuer(t)
	up := newSessionWitness(t, "tool")
	_, g := startGateway(t, authedConfig(iss, []config.Upstream{
		{ID: "u", URL: up.URL, Auth: &config.UpstreamAuth{Strategy: "passthrough"}, CacheTTLMs: -1},
	}, nil))

	u := g.rt().upstreams[0]
	u.mu.Lock()
	for _, key := range []string{"iss\x00alice", "iss\x00bob"} {
		u.roots[key] = &rootEntry{subscribed: map[string]bool{}} // zero lastUsed: long idle
	}
	u.roots[sharedRootKey] = &rootEntry{subscribed: map[string]bool{}}
	u.mu.Unlock()

	u.sweepBridged()

	u.mu.Lock()
	defer u.mu.Unlock()
	if _, ok := u.roots[sharedRootKey]; !ok {
		t.Error("the shared root was swept; it belongs to the gateway, not to a caller")
	}
	if len(u.roots) != 1 {
		t.Errorf("roots after sweep = %d, want only the shared one", len(u.roots))
	}
}
