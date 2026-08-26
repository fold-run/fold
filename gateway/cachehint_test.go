package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/auth"
	"github.com/fold-run/fold/config"
)

// MCP's caching rules (specification 2026-07-28, "Caching") define exactly
// two values for cacheScope, "public" and "private". fold assembles its
// federated list results itself, so it never reaches the SDK handlers that
// stamp those fields for an ordinary server, and every list went out with
// `"cacheScope": ""` — neither permitted value, and read by a client applying
// the "absent means public" default as an invitation to share a list fold had
// filtered per principal.
//
// The assertions here are made on the bytes rather than on a decoded struct
// on purpose: the defect is invisible after unmarshalling, because "" and an
// absent field decode identically into the same Go zero value. Only the wire
// shows it.

// cacheHintUpstream runs a real SDK MCP server carrying one of each listable
// kind — tool, prompt, resource, and resource template — so all four of
// fold's list construction sites have something to federate.
func cacheHintUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        "read_thing",
		Description: "fixture tool",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "echo"}}}, nil
	})
	server.AddPrompt(&mcp.Prompt{Name: "greeting"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: "hello"}},
		}}, nil
	})
	read := func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "body"}}}, nil
	}
	server.AddResource(&mcp.Resource{URI: "file:///thing.txt", Name: "thing", MIMEType: "text/plain"}, read)
	server.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "file:///things/{id}.txt",
		Name:        "thing-by-id",
		MIMEType:    "text/plain",
	}, read)

	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

// listMethods are the four results fold builds itself, and therefore the four
// that the SDK's own defaults never reach.
var listMethods = []string{
	"tools/list",
	"prompts/list",
	"resources/list",
	"resources/templates/list",
}

// cacheHints reads one list result off the wire and returns its cacheScope
// and ttlMs exactly as they were serialized — with a third value reporting
// whether cacheScope was present at all, since an absent field and an empty
// one are different documents and only one of them is a specification
// violation.
func cacheHints(t *testing.T, endpoint, token, method string) (scope string, ttl int, present bool) {
	t.Helper()
	c := &consoleWire{t: t, endpoint: endpoint, token: token}
	initRaw, rpcErr := c.rpc("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "cachehint-probe", "version": "1"},
	}, false)
	if rpcErr != nil {
		t.Fatalf("initialize: JSON-RPC %d: %s", rpcErr.Code, rpcErr.Message)
	}
	var initRes struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := json.Unmarshal(initRaw, &initRes); err != nil {
		t.Fatalf("initialize result: %v", err)
	}
	c.protocol = initRes.ProtocolVersion
	c.rpc("notifications/initialized", nil, true)

	raw, rpcErr := c.rpc(method, map[string]any{}, false)
	if rpcErr != nil {
		t.Fatalf("%s: JSON-RPC %d: %s", method, rpcErr.Code, rpcErr.Message)
	}
	// The raw result object, not a typed struct: "" and absent decode the
	// same way into mcp.Cacheable, which is the whole reason this reads bytes.
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("%s: result is not an object: %v (%s)", method, err, truncate(string(raw), 200))
	}
	if v, ok := fields["cacheScope"]; ok {
		present = true
		if err := json.Unmarshal(v, &scope); err != nil {
			t.Fatalf("%s: cacheScope is not a string: %s", method, v)
		}
	}
	if v, ok := fields["ttlMs"]; ok {
		if err := json.Unmarshal(v, &ttl); err != nil {
			t.Fatalf("%s: ttlMs is not a number: %s", method, v)
		}
	}
	return scope, ttl, present
}

// TestFederatedListsCarryASpecifiedCacheScope is the regression guard: for
// every list method and every configuration shape, the scope on the wire is
// one of the two values the specification defines — never the empty string
// fold used to send — and it is the right one of the two.
func TestFederatedListsCarryASpecifiedCacheScope(t *testing.T) {
	cases := []struct {
		name string
		want string
		// build returns the config and, when the gateway requires auth, a
		// token minted for it.
		build func(t *testing.T, iss *fixtureIssuer, upURL string) (*config.Config, string)
	}{{
		// One list, served to everyone, from an upstream whose credential is
		// the gateway's own: shareable, and the only shape that is.
		name: "plain federation",
		want: cacheScopePublic,
		build: func(_ *testing.T, _ *fixtureIssuer, upURL string) (*config.Config, string) {
			return &config.Config{Upstreams: []config.Upstream{
				{ID: "u", URL: upURL, Namespace: "u"},
			}}, ""
		},
	}, {
		// Authentication alone does not make a list per-caller: every
		// principal still sees the same federation.
		name: "auth required, no policy",
		want: cacheScopePublic,
		build: func(t *testing.T, iss *fixtureIssuer, upURL string) (*config.Config, string) {
			cfg := authedConfig(iss, []config.Upstream{{ID: "u", URL: upURL, Namespace: "u"}}, nil)
			return cfg, iss.mint(t, "alice", "https://gw.example.com", nil)
		},
	}, {
		// A static credential is the gateway's, not the caller's, so the
		// upstream answers every caller identically.
		name: "static upstream credential",
		want: cacheScopePublic,
		build: func(t *testing.T, _ *fixtureIssuer, upURL string) (*config.Config, string) {
			t.Setenv("UPSTREAM_KEY", "static-secret")
			return &config.Config{Upstreams: []config.Upstream{{
				ID: "u", URL: upURL, Namespace: "u",
				Auth: &config.UpstreamAuth{Strategy: "static", SecretRef: "UPSTREAM_KEY"},
			}}}, ""
		},
	}, {
		// An explicit allow-by-default with no rules filters nothing.
		name: "policy allows by default with no rules",
		want: cacheScopePublic,
		build: func(t *testing.T, iss *fixtureIssuer, upURL string) (*config.Config, string) {
			cfg := authedConfig(iss, []config.Upstream{{ID: "u", URL: upURL, Namespace: "u"}},
				&config.Policy{DefaultDecision: "allow"})
			return cfg, iss.mint(t, "alice", "https://gw.example.com", nil)
		},
	}, {
		// The specification's own example of a private result: a list
		// filtered per principal.
		name: "policy rules",
		want: cacheScopePrivate,
		build: func(t *testing.T, iss *fixtureIssuer, upURL string) (*config.Config, string) {
			cfg := authedConfig(iss, []config.Upstream{{ID: "u", URL: upURL, Namespace: "u"}},
				&config.Policy{
					DefaultDecision: "allow",
					Rules: []config.PolicyRule{{
						ID:       "alice-read",
						Subjects: &config.PolicySubjects{Subs: []string{"alice"}},
						Allow:    []config.PolicyAllow{{Server: "u", Names: []string{"read_*"}}},
					}},
				})
			return cfg, iss.mint(t, "alice", "https://gw.example.com", nil)
		},
	}, {
		// A default with no rules is the same answer for everyone — nothing
		// at all — so it is shareable, exactly like the "allow" default above
		// and like the empty policy struct that means the identical thing.
		name: "policy denies by default with no rules",
		want: cacheScopePublic,
		build: func(t *testing.T, iss *fixtureIssuer, upURL string) (*config.Config, string) {
			cfg := authedConfig(iss, []config.Upstream{{ID: "u", URL: upURL, Namespace: "u"}},
				&config.Policy{DefaultDecision: "deny"})
			return cfg, iss.mint(t, "alice", "https://gw.example.com", nil)
		},
	}, {
		// Tenancy bounds visibility to an upstream subset before policy runs.
		name: "tenant declared",
		want: cacheScopePrivate,
		build: func(t *testing.T, iss *fixtureIssuer, upURL string) (*config.Config, string) {
			cfg := authedConfig(iss, []config.Upstream{{ID: "u", URL: upURL, Namespace: "u"}}, nil)
			cfg.Tenants = []config.Tenant{{
				ID:        "acme",
				Subjects:  &config.PolicySubjects{Claims: map[string]any{"org_id": "acme"}},
				Upstreams: []string{"u"},
			}}
			return cfg, iss.mintClaims(t, jwt.MapClaims{
				"sub": "alice", "aud": "https://gw.example.com", "org_id": "acme",
				"exp": time.Now().Add(time.Hour).Unix(),
			})
		},
	}, {
		// The upstream sees the caller's own credential, so it may answer
		// two callers differently no matter what fold's policy says.
		name: "passthrough upstream credential",
		want: cacheScopePrivate,
		build: func(t *testing.T, iss *fixtureIssuer, upURL string) (*config.Config, string) {
			cfg := authedConfig(iss, []config.Upstream{{
				ID: "u", URL: upURL, Namespace: "u",
				Auth: &config.UpstreamAuth{Strategy: "passthrough"},
			}}, nil)
			return cfg, iss.mint(t, "alice", "https://gw.example.com", nil)
		},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := cacheHintUpstream(t)
			iss := newFixtureIssuer(t)
			cfg, token := tc.build(t, iss, up.URL)
			ts, _ := startGateway(t, cfg)

			for _, method := range listMethods {
				scope, ttl, present := cacheHints(t, ts.URL+"/mcp", token, method)
				// The defect itself, stated as its own assertion so that a
				// future scope fold has not thought about still fails here.
				if !present {
					t.Errorf("%s: result has no cacheScope field at all", method)
				} else if scope != cacheScopePublic && scope != cacheScopePrivate {
					t.Errorf("%s: cacheScope = %q, want one of %q/%q — the specification "+
						"defines no third value", method, scope, cacheScopePublic, cacheScopePrivate)
				}
				if scope != tc.want {
					t.Errorf("%s: cacheScope = %q, want %q", method, scope, tc.want)
				}
				// Deliberate: "immediately stale". Changing it should be a
				// conscious edit, so it is pinned rather than assumed.
				if ttl != 0 {
					t.Errorf("%s: ttlMs = %d, want 0", method, ttl)
				}
			}
		})
	}
}

// TestPlainFederationMatchesTheUpstreamsOwnCacheHints holds the invisibility
// rule to the byte: for a federation with nothing per-caller about it, the
// hints fold mints itself are the ones the upstream would have sent had the
// client talked to it directly.
func TestPlainFederationMatchesTheUpstreamsOwnCacheHints(t *testing.T) {
	up := cacheHintUpstream(t)
	ts, _ := startGateway(t, &config.Config{Upstreams: []config.Upstream{
		{ID: "u", URL: up.URL, Namespace: "u"},
	}})

	for _, method := range listMethods {
		directScope, directTTL, directPresent := cacheHints(t, up.URL, "", method)
		gwScope, gwTTL, gwPresent := cacheHints(t, ts.URL+"/mcp", "", method)
		if gwScope != directScope || gwTTL != directTTL || gwPresent != directPresent {
			t.Errorf("%s: through the gateway (scope=%q ttl=%d present=%v) differs from "+
				"the upstream directly (scope=%q ttl=%d present=%v)",
				method, gwScope, gwTTL, gwPresent, directScope, directTTL, directPresent)
		}
	}
}

// TestListCacheScope covers the decision itself, including the shapes that
// are awkward to stand a gateway up for.
func TestListCacheScope(t *testing.T) {
	callerDerived := func(strategy string) []*upstream {
		return []*upstream{{creds: auth.NewUpstreamCredentials(
			&config.UpstreamAuth{Strategy: strategy, TokenEndpoint: "https://idp.example.com/token",
				ClientID: "fold", Audience: "https://api.example.com"}, nil)}}
	}
	configured := func(strategy string) []*upstream {
		return []*upstream{{creds: auth.NewUpstreamCredentials(
			&config.UpstreamAuth{Strategy: strategy, SecretRef: "UPSTREAM_KEY"}, nil)}}
	}

	cases := []struct {
		name      string
		cfg       *config.Config
		upstreams []*upstream
		want      string
	}{
		{"nil config", nil, nil, cacheScopePublic},
		{"empty config", &config.Config{}, nil, cacheScopePublic},
		{"no policy, no tenants", &config.Config{Upstreams: []config.Upstream{{ID: "u"}}}, nil, cacheScopePublic},
		{"empty policy struct", &config.Config{Policy: &config.Policy{}}, nil, cacheScopePublic},
		{"policy allows by default, no rules", &config.Config{
			Policy: &config.Policy{DefaultDecision: "allow"}}, nil, cacheScopePublic},
		// Behaviourally identical to the empty struct above — both are the
		// deny-all engine — and identical for every caller, so both are
		// shareable. A default alone never makes a list vary by who asks.
		{"policy denies by default, no rules", &config.Config{
			Policy: &config.Policy{DefaultDecision: "deny"}}, nil, cacheScopePublic},
		{"policy with one rule", &config.Config{Policy: &config.Policy{
			DefaultDecision: "allow",
			Rules:           []config.PolicyRule{{ID: "r"}},
		}}, nil, cacheScopePrivate},
		{"empty tenant slice", &config.Config{Tenants: []config.Tenant{}}, nil, cacheScopePublic},
		{"one tenant", &config.Config{Tenants: []config.Tenant{{ID: "acme"}}}, nil, cacheScopePrivate},
		{"upstream with no credential", &config.Config{}, []*upstream{{}}, cacheScopePublic},
		{"upstream with no auth section", &config.Config{}, configured("none"), cacheScopePublic},
		{"static upstream credential", &config.Config{}, configured("static"), cacheScopePublic},
		{"client-credentials upstream", &config.Config{}, callerDerived("client-credentials"), cacheScopePublic},
		{"passthrough upstream", &config.Config{}, callerDerived("passthrough"), cacheScopePrivate},
		{"token-exchange upstream", &config.Config{}, callerDerived("token-exchange"), cacheScopePrivate},
		{"one caller-derived upstream among several", &config.Config{}, append(
			configured("static"), callerDerived("passthrough")...), cacheScopePrivate},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := listCacheScope(tc.cfg, tc.upstreams)
			if got != tc.want {
				t.Errorf("listCacheScope = %q, want %q", got, tc.want)
			}
			if got != cacheScopePublic && got != cacheScopePrivate {
				t.Errorf("listCacheScope = %q, which is not a value the specification defines", got)
			}
		})
	}
}

// TestReloadChangesListCacheScope is why the scope lives on the routing
// snapshot rather than being decided once at construction: adding a policy to
// a running gateway changes what a caller may see, and the hint describing
// that answer has to change in the same atomic swap — in both directions,
// since removing the policy makes the list shareable again.
func TestReloadChangesListCacheScope(t *testing.T) {
	up := cacheHintUpstream(t)
	iss := newFixtureIssuer(t)
	upstreams := []config.Upstream{{ID: "u", URL: up.URL, Namespace: "u"}}
	ts, gw := startGateway(t, authedConfig(iss, upstreams, nil))
	token := iss.mint(t, "alice", "https://gw.example.com", nil)

	check := func(stage, want string) {
		t.Helper()
		for _, method := range listMethods {
			scope, ttl, _ := cacheHints(t, ts.URL+"/mcp", token, method)
			if scope != want {
				t.Errorf("%s: %s cacheScope = %q, want %q", stage, method, scope, want)
			}
			if ttl != 0 {
				t.Errorf("%s: %s ttlMs = %d, want 0", stage, method, ttl)
			}
		}
	}

	check("before reload", cacheScopePublic)

	withPolicy := authedConfig(iss, upstreams, &config.Policy{
		DefaultDecision: "allow",
		Rules: []config.PolicyRule{{
			ID:       "alice-read",
			Subjects: &config.PolicySubjects{Subs: []string{"alice"}},
			Allow:    []config.PolicyAllow{{Server: "u", Names: []string{"read_*"}}},
		}},
	})
	if err := gw.Reload(withPolicy); err != nil {
		t.Fatalf("Reload adding policy: %v", err)
	}
	check("after adding policy", cacheScopePrivate)

	if err := gw.Reload(authedConfig(iss, upstreams, nil)); err != nil {
		t.Fatalf("Reload removing policy: %v", err)
	}
	check("after removing policy", cacheScopePublic)
}
