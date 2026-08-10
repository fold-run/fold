package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// Phase 4 of docs/design-tenancy.md: tenants[].upstreams bounds what a tenant
// can see at all, evaluated before policy. Policy remains the authority on
// what may be invoked among what is left — these cover the coarser cut.

// countingUpstream is a fixture that records how many HTTP requests reached
// it, which is how "an upstream outside the subset is never asked" is checked
// as a fact rather than inferred from a filtered response.
func countingUpstream(t *testing.T, tool, resourceURI string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: tool, Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{Name: tool, InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo:" + tool}}}, nil
		})
	if resourceURI != "" {
		server.AddResource(&mcp.Resource{URI: resourceURI, Name: tool},
			func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: tool}}}, nil
			})
	}
	var hits atomic.Int64
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	return ts, &hits
}

// twoUpstreamTenants starts an authenticated gateway over upstreams "a" and
// "b" with one tenant scoped to "a", and returns a token minter.
func twoUpstreamTenants(t *testing.T, subset []string) (*httptest.Server, *Gateway, *atomic.Int64, *atomic.Int64, func(sub, org string) string) {
	t.Helper()
	upA, hitsA := countingUpstream(t, "alpha", "file:///a.txt")
	upB, hitsB := countingUpstream(t, "beta", "file:///b.txt")
	iss := newFixtureIssuer(t)
	cfg := authedConfig(iss, []config.Upstream{
		{ID: "a", URL: upA.URL, Namespace: "a"},
		{ID: "b", URL: upB.URL, Namespace: "b"},
	}, nil)
	cfg.Tenants = []config.Tenant{{
		ID:        "acme",
		Subjects:  &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}},
		Upstreams: subset,
	}}
	ts, gw := startGateway(t, cfg)
	mint := func(sub, org string) string {
		return iss.mintClaims(t, jwt.MapClaims{
			"sub": sub, "aud": "https://gw.example.com", "org_id": org,
			"exp": time.Now().Add(time.Hour).Unix(),
		})
	}
	return ts, gw, hitsA, hitsB, mint
}

// A tenant's list holds only its own upstreams — and the upstream outside the
// subset is never even asked, which is the difference between filtering the
// fan-out and filtering the result.
func TestTenantSubsetFiltersLists(t *testing.T) {
	ts, _, _, hitsB, mint := twoUpstreamTenants(t, []string{"a"})
	ctx := context.Background()

	scoped := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})
	before := hitsB.Load()
	res, err := scoped.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := strings.Join(toolNamesOf(res.Tools), ","); got != "a__alpha" {
		t.Fatalf("tools = %q, want only a__alpha", got)
	}
	if after := hitsB.Load(); after != before {
		t.Fatalf("the out-of-subset upstream was asked %d times; it must never be reached", after-before)
	}

	// A caller in no tenant still sees the whole federation.
	all := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("carol", "globex")})
	res, err = all.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := strings.Join(toolNamesOf(res.Tools), ","); got != "a__alpha,b__beta" {
		t.Fatalf("untenanted tools = %q, want the whole federation", got)
	}
}

// Invisibility and denial are one pair: a name the tenant cannot see is also
// a name it cannot call, and the refusal says which allowance is missing.
func TestTenantSubsetDeniesNamedCalls(t *testing.T) {
	ts, _, _, hitsB, mint := twoUpstreamTenants(t, []string{"a"})
	ctx := context.Background()
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "a__alpha"}); err != nil {
		t.Fatalf("call inside the subset rejected: %v", err)
	}
	before := hitsB.Load()
	_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "b__beta"})
	if err == nil {
		t.Fatal("a call outside the tenant's subset was served")
	}
	var wire *jsonrpc.Error
	if !errors.As(err, &wire) {
		t.Fatalf("error = %v (%T), want a JSON-RPC error", err, err)
	}
	if wire.Code != codeDenied {
		t.Fatalf("code = %d, want %d (the subset is a coarser cut of the same decision)", wire.Code, codeDenied)
	}
	for _, want := range []string{"acme", "subset"} {
		if !strings.Contains(wire.Message, want) {
			t.Fatalf("message = %q, want it to name %q", wire.Message, want)
		}
	}
	if after := hitsB.Load(); after != before {
		t.Fatalf("a denied call still reached the upstream %d times", after-before)
	}
}

// A refusal is audited like any other denial — audit is the single exit door,
// and a tenant kept out of an upstream has to be visible in the record.
func TestTenantSubsetDenialIsAudited(t *testing.T) {
	upA, _ := countingUpstream(t, "alpha", "")
	upB, _ := countingUpstream(t, "beta", "")

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

	iss := newFixtureIssuer(t)
	cfg := authedConfig(iss, []config.Upstream{
		{ID: "a", URL: upA.URL, Namespace: "a"},
		{ID: "b", URL: upB.URL, Namespace: "b"},
	}, nil)
	cfg.Tenants = []config.Tenant{{
		ID:        "acme",
		Subjects:  &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}},
		Upstreams: []string{"a"},
	}}
	cfg.Audit = &config.Audit{Sinks: []config.AuditSink{{Type: "webhook", URL: collector.URL}}}
	ts, _ := startGateway(t, cfg)

	token := iss.mintClaims(t, jwt.MapClaims{
		"sub": "alice", "aud": "https://gw.example.com", "org_id": "acme",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + token})
	_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "b__beta"})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		for _, e := range events {
			if e.Outcome == audit.OutcomeDenied && e.Tenant == "acme" && e.Name == "b__beta" {
				mu.Unlock()
				return
			}
		}
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("no denial audited for the out-of-subset call; got %d events", len(events))
}

// Resource URIs are opaque and the ownership index is shared across
// principals, so a URI another tenant listed must not become reachable by
// affinity — nor by probing, which is the path a guessed URI takes.
func TestTenantSubsetGuardsResources(t *testing.T) {
	ts, _, _, hitsB, mint := twoUpstreamTenants(t, []string{"a"})
	ctx := context.Background()

	// An untenanted caller lists both, which is what populates the shared
	// URI-ownership index that the scoped caller must not be able to use.
	all := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("carol", "globex")})
	if _, err := all.ListResources(ctx, nil); err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	if _, err := all.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///b.txt"}); err != nil {
		t.Fatalf("untenanted read rejected: %v", err)
	}

	scoped := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})
	res, err := scoped.ListResources(ctx, nil)
	if err != nil {
		t.Fatalf("ListResources: %v", err)
	}
	for _, r := range res.Resources {
		if r.URI == "file:///b.txt" {
			t.Fatal("a resource from outside the subset appeared in the tenant's list")
		}
	}
	before := hitsB.Load()
	if _, err := scoped.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///b.txt"}); err == nil {
		t.Fatal("a resource outside the tenant's subset was served")
	}
	if after := hitsB.Load(); after != before {
		t.Fatalf("the out-of-subset upstream was probed %d times for a URI it owns", after-before)
	}
	// Its own resource still reads.
	if _, err := scoped.ReadResource(ctx, &mcp.ReadResourceParams{URI: "file:///a.txt"}); err != nil {
		t.Fatalf("read inside the subset rejected: %v", err)
	}
}

// An empty subset means "every upstream", not "none" — the field is optional,
// and a tenant declared for its budget alone must not lose the federation.
func TestTenantWithoutSubsetSeesEverything(t *testing.T) {
	ts, _, _, _, mint := twoUpstreamTenants(t, nil)
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})
	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := strings.Join(toolNamesOf(res.Tools), ","); got != "a__alpha,b__beta" {
		t.Fatalf("tools = %q, want the whole federation", got)
	}
}

// The subset is snapshot state like everything else reloadable: widening it
// takes effect without a restart, which is the point of tenants living in the
// snapshot rather than being construction-wired.
func TestReloadWidensTheTenantSubset(t *testing.T) {
	ts, gw, _, _, mint := twoUpstreamTenants(t, []string{"a"})
	ctx := context.Background()
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + mint("alice", "acme")})

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "b__beta"}); err == nil {
		t.Fatal("a call outside the subset was served before the reload")
	}

	reloaded := *gw.rt().cfg
	reloaded.Tenants = []config.Tenant{{
		ID:        "acme",
		Subjects:  &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}},
		Upstreams: []string{"a", "b"},
	}}
	if err := gw.Reload(&reloaded); err != nil {
		t.Fatalf("reload: %v", err)
	}

	if _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "b__beta"}); err != nil {
		t.Fatalf("call rejected after the subset was widened: %v", err)
	}
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if got := strings.Join(toolNamesOf(res.Tools), ","); got != "a__alpha,b__beta" {
		t.Fatalf("tools = %q after widening, want the whole federation", got)
	}
}
