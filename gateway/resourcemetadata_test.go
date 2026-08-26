package gateway

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

// The RFC 9728 document as a client reads it. §2 lists scopes_supported as
// recommended, and until fold answered it a client had no way to learn which
// scopes the policy requires except by being refused: authorize with whatever
// scopes it guessed, connect, call a tool, and read the requirement out of the
// denial. These pin the other half of that exchange — the requirement stated
// up front, before the first call.

// scopeRoutes is a snapshot carrying only what requiredScopes reads.
func scopeRoutes(pol *config.Policy, tenants []config.Tenant) *routes {
	return &routes{cfg: &config.Config{Policy: pol, Tenants: tenants}}
}

func scopedRule(id string, scopes ...string) config.PolicyRule {
	return config.PolicyRule{
		ID:       id,
		Subjects: &config.PolicySubjects{Scopes: scopes},
		Allow:    []config.PolicyAllow{{Server: "docs"}},
	}
}

func scopedTenant(id string, scopes ...string) config.Tenant {
	return config.Tenant{ID: id, Subjects: &config.PolicySubjects{Scopes: scopes}}
}

func TestRequiredScopes(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   *routes
		want []string
	}{
		{"nil snapshot", nil, nil},
		{"snapshot without a config", &routes{}, nil},
		{"neither policy nor tenants", scopeRoutes(nil, nil), nil},
		{"empty policy and tenants", scopeRoutes(&config.Policy{DefaultDecision: "deny"}, []config.Tenant{}), nil},
		{
			"rules that name no scope",
			scopeRoutes(&config.Policy{Rules: []config.PolicyRule{
				{ID: "eng", Subjects: &config.PolicySubjects{Groups: []string{"engineering"}}},
				{ID: "everyone"}, // no subjects at all
			}}, nil),
			nil,
		},
		{
			"one scoped rule",
			scopeRoutes(&config.Policy{Rules: []config.PolicyRule{scopedRule("readers", "docs:read")}}, nil),
			[]string{"docs:read"},
		},
		{
			// The overlap case: two rules requiring the same credential
			// describe one thing to obtain, not two.
			"a scope named by two rules appears once",
			scopeRoutes(&config.Policy{Rules: []config.PolicyRule{
				scopedRule("readers", "docs:read"),
				scopedRule("writers", "docs:read", "docs:write"),
			}}, nil),
			[]string{"docs:read", "docs:write"},
		},
		{
			"tenants alone",
			// Tenant selectors are deliberately not published: this endpoint
			// is unauthenticated and a tenant scope is usually a customer's
			// name. See requiredScopes.
			scopeRoutes(nil, []config.Tenant{scopedTenant("acme", "tenant:acme")}),
			nil,
		},
		{
			"policy and tenants merged",
			scopeRoutes(
				&config.Policy{Rules: []config.PolicyRule{scopedRule("readers", "docs:read")}},
				[]config.Tenant{scopedTenant("acme", "tenant:acme")},
			),
			[]string{"docs:read"},
		},
		{
			// Deliberately unsorted, across rules and tenants and within a
			// single selector: the document is fetched repeatedly and may be
			// diffed or cached, so its order must come from the scopes rather
			// than from the order an operator happened to write them in.
			"output is sorted regardless of input order",
			scopeRoutes(
				&config.Policy{Rules: []config.PolicyRule{
					scopedRule("z-rule", "z:last", "a:first"),
					scopedRule("m-rule", "m:middle", "z:last"),
					scopedRule("b-rule", "b:second"),
				}},
				// "n:tenant" is named only by a tenant and so is absent;
				// "a:first" survives because a rule names it too.
				[]config.Tenant{scopedTenant("t2", "n:tenant"), scopedTenant("t1", "a:first")},
			),
			[]string{"a:first", "b:second", "m:middle", "z:last"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := requiredScopes(tc.rt)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("requiredScopes = %v, want %v", got, tc.want)
			}
			if tc.want == nil && got != nil {
				t.Errorf("requiredScopes = %#v, want nil so the key is omitted entirely", got)
			}
		})
	}
}

// advertisedScopes reads scopes_supported off the published document,
// reporting whether the key was there at all: an absent key ("fold does not
// say") and an empty array ("fold requires no scopes") are different claims to
// a client, so the two cannot be collapsed into one.
func advertisedScopes(t *testing.T, base string) ([]string, bool) {
	t.Helper()
	status, doc := getJSON(t, base+"/.well-known/oauth-protected-resource")
	if status != http.StatusOK {
		t.Fatalf("protected resource metadata = %d, want 200", status)
	}
	return scopesFrom(t, doc)
}

func scopesFrom(t *testing.T, doc map[string]any) ([]string, bool) {
	t.Helper()
	raw, present := doc["scopes_supported"]
	if !present {
		return nil, false
	}
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("scopes_supported = %#v, want an array", raw)
	}
	out := []string{}
	for _, entry := range list {
		s, ok := entry.(string)
		if !ok {
			t.Fatalf("scopes_supported = %v, want strings", list)
		}
		out = append(out, s)
	}
	return out, true
}

// What the document says for each shape of governing document.
func TestMetadataAdvertisesRequiredScopes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		policy  *config.Policy
		tenants []config.Tenant
		want    []string // nil means the key must be absent
	}{
		{name: "ungoverned gateway says nothing"},
		{
			name: "scoped policy rules",
			policy: &config.Policy{DefaultDecision: "deny", Rules: []config.PolicyRule{
				{
					ID:       "writers",
					Subjects: &config.PolicySubjects{Scopes: []string{"docs:write", "docs:read"}},
					Allow:    []config.PolicyAllow{{Server: "docs"}},
				},
				{
					ID:       "readers",
					Subjects: &config.PolicySubjects{Scopes: []string{"docs:read"}},
					Allow:    []config.PolicyAllow{{Server: "docs", Names: []string{"read_doc"}}},
				},
			}},
			want: []string{"docs:read", "docs:write"},
		},
		{
			// A caller who cannot match their tenant loses that tenant's
			// allowance and visibility, so a tenant selector's scopes are as
			// needed to use the resource as a rule's are.
			name:    "scoped tenant selectors",
			tenants: []config.Tenant{scopedTenant("acme", "tenant:acme")},
			want:    nil,
		},
		{
			name: "only policy scopes are published, deduped and sorted",
			policy: &config.Policy{DefaultDecision: "deny", Rules: []config.PolicyRule{
				{
					ID:       "writers",
					Subjects: &config.PolicySubjects{Scopes: []string{"docs:write", "tenant:acme"}},
					Allow:    []config.PolicyAllow{{Server: "docs"}},
				},
			}},
			tenants: []config.Tenant{scopedTenant("acme", "tenant:acme"), scopedTenant("globex", "docs:read")},
			// "tenant:acme" appears only because the rule names it as well —
			// its presence on a tenant contributes nothing. "docs:read" is
			// named by a tenant alone and is therefore absent, which is the
			// case that would regress if tenants were ever folded back in.
			want: []string{"docs:write", "tenant:acme"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := newUpstreamServer(t, "read_doc", "edit_doc")
			iss := newFixtureIssuer(t)
			cfg := authedConfig(iss, []config.Upstream{{ID: "docs", URL: up.URL, Namespace: "docs"}}, tc.policy)
			cfg.Tenants = tc.tenants
			ts, _ := startGateway(t, cfg)

			got, present := advertisedScopes(t, ts.URL)
			if tc.want == nil {
				if present {
					t.Fatalf("scopes_supported = %v, want the key absent — an empty array claims fold requires no scopes", got)
				}
				return
			}
			if !present {
				t.Fatal("scopes_supported absent, want the scopes the policy requires")
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("scopes_supported = %v, want %v", got, tc.want)
			}
		})
	}
}

// The EMA branch re-marshals the document through a map to attach the
// extension key, so it is the one place the scopes could be dropped on the
// way out. Both halves must survive that round-trip.
func TestMetadataScopesSurviveEMAExtension(t *testing.T) {
	up, _ := newUpstreamServer(t, "read_doc")
	idp := newFixtureIssuer(t)
	setEMAKey(t)
	cfg := emaConfig(idp, []config.Upstream{{ID: "docs", URL: up.URL, Namespace: "docs"}})
	cfg.Policy = &config.Policy{DefaultDecision: "deny", Rules: []config.PolicyRule{{
		ID:       "readers",
		Subjects: &config.PolicySubjects{Scopes: []string{"docs:write", "docs:read"}},
		Allow:    []config.PolicyAllow{{Server: "docs"}},
	}}}
	cfg.Tenants = []config.Tenant{scopedTenant("acme", "tenant:acme")}
	ts, _ := startGateway(t, cfg)

	status, doc := getJSON(t, ts.URL+"/.well-known/oauth-protected-resource")
	if status != http.StatusOK {
		t.Fatalf("protected resource metadata = %d, want 200", status)
	}
	got, present := scopesFrom(t, doc)
	if !present {
		t.Fatalf("scopes_supported lost in the EMA round-trip: %v", doc)
	}
	if want := "docs:read,docs:write"; strings.Join(got, ",") != want {
		t.Fatalf("scopes_supported = %v, want [%s]", got, want)
	}
	if _, ok := doc["io.modelcontextprotocol/enterprise-managed-authorization"]; !ok {
		t.Errorf("EMA extension key missing alongside the scopes: %v", doc)
	}
	// The rest of the document is still the RFC 9728 one, not just the two
	// keys this test cares about.
	if doc["resource"] != emaResource {
		t.Errorf("resource = %v, want %q", doc["resource"], emaResource)
	}
}

// Policy hot-reloads, so the document must be built per request. A document
// marshaled once at construction would go on advertising the scopes fold
// started with — telling a client to obtain a credential the running policy no
// longer asks for, and never naming the one it now does.
func TestMetadataScopesFollowPolicyReload(t *testing.T) {
	up, _ := newUpstreamServer(t, "read_doc", "edit_doc")
	iss := newFixtureIssuer(t)
	ups := []config.Upstream{{ID: "docs", URL: up.URL, Namespace: "docs"}}
	readOnly := &config.Policy{DefaultDecision: "deny", Rules: []config.PolicyRule{{
		ID:       "readers",
		Subjects: &config.PolicySubjects{Scopes: []string{"docs:read"}},
		Allow:    []config.PolicyAllow{{Server: "docs", Names: []string{"read_doc"}}},
	}}}
	withWriters := &config.Policy{DefaultDecision: "deny", Rules: []config.PolicyRule{
		readOnly.Rules[0],
		{
			ID:       "writers",
			Subjects: &config.PolicySubjects{Scopes: []string{"docs:write"}},
			Allow:    []config.PolicyAllow{{Server: "docs"}},
		},
	}}

	ts, gw := startGateway(t, authedConfig(iss, ups, readOnly))
	if got, _ := advertisedScopes(t, ts.URL); strings.Join(got, ",") != "docs:read" {
		t.Fatalf("scopes_supported = %v, want [docs:read]", got)
	}

	// A rule arrives requiring a new credential: the document says so.
	if err := gw.Reload(authedConfig(iss, ups, withWriters)); err != nil {
		t.Fatalf("Reload adding a scoped rule: %v", err)
	}
	if got, _ := advertisedScopes(t, ts.URL); strings.Join(got, ",") != "docs:read,docs:write" {
		t.Fatalf("post-reload scopes_supported = %v, want [docs:read docs:write]", got)
	}

	// And back: a retired requirement stops being advertised, so the document
	// is a report on the live policy rather than a high-water mark.
	if err := gw.Reload(authedConfig(iss, ups, readOnly)); err != nil {
		t.Fatalf("Reload removing the scoped rule: %v", err)
	}
	if got, _ := advertisedScopes(t, ts.URL); strings.Join(got, ",") != "docs:read" {
		t.Fatalf("post-revert scopes_supported = %v, want [docs:read]", got)
	}
}

// startGatewayOnOwnOrigin starts a gateway whose auth.resource is the address
// it actually listens on, so the metadata URL fold hands an unauthenticated
// caller is one the test can follow. The handler is late-bound because the
// config needs the listener's URL and the listener needs a handler.
func startGatewayOnOwnOrigin(t *testing.T, build func(resource string) *config.Config) (*httptest.Server, *Gateway) {
	t.Helper()
	var handler atomic.Pointer[http.Handler]
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := handler.Load()
		if h == nil {
			http.Error(w, "gateway not built yet", http.StatusServiceUnavailable)
			return
		}
		(*h).ServeHTTP(w, r)
	}))
	t.Cleanup(ts.Close)
	gw, err := New(build(ts.URL))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(gw.Close)
	h := gw.Handler()
	handler.Store(&h)
	return ts, gw
}

var resourceMetadataRE = regexp.MustCompile(`resource_metadata="([^"]+)"`)

// The client bootstrap path, end to end: a call without a token is refused
// with a pointer, the pointer resolves, and the document it resolves to
// describes this resource and the scopes it wants. Every step of that chain is
// load-bearing — a pointer that 404s ends the walk and forces the operator to
// configure the client out of band.
func TestUnauthenticatedChallengePointsAtTheDocument(t *testing.T) {
	up, _ := newUpstreamServer(t, "read_doc")
	iss := newFixtureIssuer(t)
	auditCfg, snap := collectAudit(t)
	ts, _ := startGatewayOnOwnOrigin(t, func(resource string) *config.Config {
		cfg := authedConfig(iss, []config.Upstream{{ID: "docs", URL: up.URL, Namespace: "docs"}},
			&config.Policy{DefaultDecision: "deny", Rules: []config.PolicyRule{{
				ID:       "readers",
				Subjects: &config.PolicySubjects{Scopes: []string{"docs:read"}},
				Allow:    []config.PolicyAllow{{Server: "docs"}},
			}}})
		cfg.Auth.Resource = resource
		cfg.Audit = auditCfg
		return cfg
	})

	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("call without a token = %d, want 401", resp.StatusCode)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	match := resourceMetadataRE.FindStringSubmatch(challenge)
	if match == nil {
		t.Fatalf("WWW-Authenticate = %q, want a resource_metadata pointer", challenge)
	}

	// Follow the pointer exactly as given — not a URL the test rebuilt.
	status, doc := getJSON(t, match[1])
	if status != http.StatusOK {
		t.Fatalf("the advertised metadata URL %q = %d, want 200", match[1], status)
	}
	if doc["resource"] != ts.URL {
		t.Errorf("document describes resource %v, want %q", doc["resource"], ts.URL)
	}
	got, present := scopesFrom(t, doc)
	if !present || strings.Join(got, ",") != "docs:read" {
		t.Errorf("scopes_supported = %v (present=%v), want [docs:read]", got, present)
	}

	// The refusal that started the walk is still a terminal response, so it
	// leaves a record: a client bootstrapping and a client failing to
	// authenticate look identical on the wire and only differ in the trail.
	evt := awaitEvent(t, snap, func(e audit.Event) bool {
		return e.Method == "http" && e.Outcome == audit.OutcomeUnauthenticated
	})
	if evt.Error == "" {
		t.Errorf("unauthenticated event carries no reason: %+v", evt)
	}
}
