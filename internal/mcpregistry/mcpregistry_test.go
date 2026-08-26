package mcpregistry

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fold-run/fold/config"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeRegistry serves the per-name latest endpoint from a fixture map.
func fakeRegistry(t *testing.T, byName map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v0.1/servers/"
		if !strings.HasPrefix(r.URL.Path, prefix) || !strings.HasSuffix(r.URL.Path, "/versions/latest") {
			http.NotFound(w, r)
			return
		}
		// r.URL.Path is already unescaped, so the reverse-DNS name with its
		// slash arrives whole.
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), "/versions/latest")
		body, ok := byName[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serverDoc renders a registry entry the way the real API does.
func serverDoc(name, status string, latest bool, remotes string) string {
	return `{"server":{"name":"` + name + `","description":"d","version":"1.0.0","remotes":[` + remotes + `]},` +
		`"_meta":{"io.modelcontextprotocol.registry/official":{"status":"` + status + `","isLatest":` +
		map[bool]string{true: "true", false: "false"}[latest] + `}}}`
}

const httpRemote = `{"type":"streamable-http","url":"https://mcp.example.com/mcp"}`

func fetchAll(t *testing.T, c *Client, list *Allowlist) map[string]*Record {
	t.Helper()
	out := map[string]*Record{}
	for _, e := range list.Servers {
		got, err := c.Latest(context.Background(), e.Name)
		if err != nil {
			continue
		}
		out[e.Name] = got
	}
	return out
}

func TestRegistryEntryBecomesAnUpstream(t *testing.T) {
	reg := fakeRegistry(t, map[string]string{
		"io.github.acme/tools": serverDoc("io.github.acme/tools", "active", true, httpRemote),
	})
	c, err := NewClient(reg.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	list := &Allowlist{Servers: []Entry{{Name: "io.github.acme/tools"}}}
	ups := Map(fetchAll(t, c, list), list, MapOptions{}, testLog())
	if len(ups) != 1 {
		t.Fatalf("got %d upstreams, want 1", len(ups))
	}
	u := ups[0]
	if u.ID != "io-github-acme-tools" {
		t.Errorf("id = %q, want the flattened registry name", u.ID)
	}
	if u.Namespace != u.ID {
		t.Errorf("namespace = %q, want it to default to the id", u.Namespace)
	}
	if u.URL != "https://mcp.example.com/mcp" {
		t.Errorf("url = %q", u.URL)
	}
	if u.Owner == nil || u.Owner.Org != "io.github.acme/tools" {
		t.Errorf("owner does not carry the registry name: %+v", u.Owner)
	}
	// The producer never invents a credential.
	if u.Auth != nil {
		t.Errorf("upstream carries auth it was never given: %+v", u.Auth)
	}
}

func TestNamespaceOverrideIsHonoured(t *testing.T) {
	reg := fakeRegistry(t, map[string]string{
		"io.github.github/github-mcp-server": serverDoc("io.github.github/github-mcp-server", "active", true, httpRemote),
	})
	c, _ := NewClient(reg.URL, "")
	list := &Allowlist{Servers: []Entry{{
		Name: "io.github.github/github-mcp-server", ID: "github", Namespace: "gh",
	}}}
	ups := Map(fetchAll(t, c, list), list, MapOptions{}, testLog())
	if len(ups) != 1 || ups[0].ID != "github" || ups[0].Namespace != "gh" {
		t.Fatalf("override ignored: %+v", ups)
	}
}

// The entries a registry publishes that fold cannot federate, and the reason
// each one is a skip rather than a failure.
func TestUnfederatableEntriesAreSkipped(t *testing.T) {
	cases := map[string]string{
		"retired upstream":     serverDoc("x/a", "deleted", true, httpRemote),
		"deprecated upstream":  serverDoc("x/a", "deprecated", true, httpRemote),
		"package-only entry":   serverDoc("x/a", "active", true, ""),
		"sse-only transport":   serverDoc("x/a", "active", true, `{"type":"sse","url":"https://mcp.example.com/sse"}`),
		"two http remotes":     serverDoc("x/a", "active", true, httpRemote+`,{"type":"streamable-http","url":"https://other.example.com/mcp"}`),
		"secret header needed": serverDoc("x/a", "active", true, `{"type":"streamable-http","url":"https://mcp.example.com/mcp","headers":[{"name":"Authorization","isSecret":true}]}`),
		"unusable url":         serverDoc("x/a", "active", true, `{"type":"streamable-http","url":"not-a-url"}`),
	}
	for name, doc := range cases {
		reg := fakeRegistry(t, map[string]string{"x/a": doc})
		c, _ := NewClient(reg.URL, "")
		list := &Allowlist{Servers: []Entry{{Name: "x/a"}}}
		if ups := Map(fetchAll(t, c, list), list, MapOptions{}, testLog()); len(ups) != 0 {
			t.Errorf("%s: expected no upstreams, got %+v", name, ups)
		}
	}
}

// The secret-header skip is a default, not a prohibition: an operator who has
// arranged the credential another way can say so.
func TestSecretHeaderEntryFederatesWhenAllowed(t *testing.T) {
	doc := serverDoc("x/a", "active", true,
		`{"type":"streamable-http","url":"https://mcp.example.com/mcp","headers":[{"name":"Authorization","isSecret":true}]}`)
	reg := fakeRegistry(t, map[string]string{"x/a": doc})
	c, _ := NewClient(reg.URL, "")
	list := &Allowlist{Servers: []Entry{{Name: "x/a"}}}
	ups := Map(fetchAll(t, c, list), list, MapOptions{AllowSecretHeaders: true}, testLog())
	if len(ups) != 1 {
		t.Fatalf("got %d upstreams, want 1", len(ups))
	}
	if ups[0].Auth != nil {
		t.Error("allowing the header must not make the producer emit a credential")
	}
}

func TestReservedIdentitiesAreRefused(t *testing.T) {
	reg := fakeRegistry(t, map[string]string{"x/a": serverDoc("x/a", "active", true, httpRemote)})
	c, _ := NewClient(reg.URL, "")
	list := &Allowlist{Servers: []Entry{{Name: "x/a", ID: "internal", Namespace: "internal"}}}
	if ups := Map(fetchAll(t, c, list), list, MapOptions{ReservedIDs: []string{"internal"}}, testLog()); len(ups) != 0 {
		t.Fatalf("reserved id was federated: %+v", ups)
	}
}

// Contested identities drop every claimant: whichever entry won would
// otherwise depend on the order lines appear in a file.
func TestContestedNamespaceDropsBothClaimants(t *testing.T) {
	reg := fakeRegistry(t, map[string]string{
		"x/a": serverDoc("x/a", "active", true, httpRemote),
		"y/b": serverDoc("y/b", "active", true, httpRemote),
	})
	c, _ := NewClient(reg.URL, "")
	list := &Allowlist{Servers: []Entry{
		{Name: "x/a", Namespace: "shared"},
		{Name: "y/b", Namespace: "shared"},
	}}
	if ups := Map(fetchAll(t, c, list), list, MapOptions{}, testLog()); len(ups) != 0 {
		t.Fatalf("expected both claimants dropped, got %+v", ups)
	}
}

// One unusable entry never takes the rest of the document down.
func TestOneBadEntryDoesNotSinkTheDocument(t *testing.T) {
	reg := fakeRegistry(t, map[string]string{
		"x/good": serverDoc("x/good", "active", true, httpRemote),
		"x/bad":  serverDoc("x/bad", "deleted", true, httpRemote),
	})
	c, _ := NewClient(reg.URL, "")
	list := &Allowlist{Servers: []Entry{{Name: "x/bad"}, {Name: "x/good"}, {Name: "x/missing"}}}
	ups := Map(fetchAll(t, c, list), list, MapOptions{}, testLog())
	if len(ups) != 1 || ups[0].ID != "x-good" {
		t.Fatalf("got %+v, want just x-good", ups)
	}
}

// The document a gateway will consume has to be a document a gateway accepts.
func TestRenderedDocumentValidatesAsConfig(t *testing.T) {
	reg := fakeRegistry(t, map[string]string{
		"io.github.acme/tools": serverDoc("io.github.acme/tools", "active", true, httpRemote),
		"io.github.acme/more":  serverDoc("io.github.acme/more", "active", true, httpRemote),
	})
	c, _ := NewClient(reg.URL, "")
	list := &Allowlist{Servers: []Entry{
		{Name: "io.github.acme/tools", Namespace: "tools"},
		{Name: "io.github.acme/more", Namespace: "more"},
	}}
	ups := Map(fetchAll(t, c, list), list, MapOptions{}, testLog())
	doc, err := Document(ups)
	if err != nil {
		t.Fatal(err)
	}
	var parsed config.Config
	if err := json.Unmarshal(doc, &parsed); err != nil {
		t.Fatalf("document does not parse as a fold config: %v", err)
	}
	if err := parsed.Validate(); err != nil {
		t.Fatalf("document does not validate: %v", err)
	}
	// Byte-stable: the gateway's change detection compares documents, so an
	// unchanged registry must render identically.
	again, _ := Document(Map(fetchAll(t, c, list), list, MapOptions{}, testLog()))
	if string(again) != string(doc) {
		t.Error("document is not byte-stable across syncs")
	}
}

func TestIdentifierFlattensRegistryNames(t *testing.T) {
	cases := map[string]string{
		"io.github.github/github-mcp-server": "io-github-github-github-mcp-server",
		"ac.inference.sh/mcp":                "ac-inference-sh-mcp",
		"Weird..Name//X":                     "weird-name-x",
	}
	for in, want := range cases {
		if got := Identifier(in); got != want {
			t.Errorf("Identifier(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAllowlistParsing(t *testing.T) {
	if _, err := ParseAllowlist([]byte(`{"servers":[{"name":"x/a"}]}`)); err != nil {
		t.Fatalf("valid allowlist rejected: %v", err)
	}
	bad := map[string]string{
		"unknown field": `{"servers":[{"name":"x/a","nmespace":"typo"}]}`,
		"no servers":    `{"servers":[]}`,
		"missing name":  `{"servers":[{"namespace":"a"}]}`,
		"duplicate":     `{"servers":[{"name":"x/a"},{"name":"x/a"}]}`,
	}
	for name, doc := range bad {
		if _, err := ParseAllowlist([]byte(doc)); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

// The base URL decides which registry is authoritative; a redirect would let
// that registry hand the decision — and any bearer — to a host nobody named.
func TestRegistryRedirectsAreRefused(t *testing.T) {
	elsewhere := fakeRegistry(t, map[string]string{"x/a": serverDoc("x/a", "active", true, httpRemote)})
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	c, _ := NewClient(redirector.URL, "secret-token")
	if _, err := c.Latest(context.Background(), "x/a"); err == nil {
		t.Fatal("expected the redirect to be refused")
	}
}

func TestNewClientRejectsNonHTTPBase(t *testing.T) {
	for _, base := range []string{"registry:8080", "ftp://registry", ""} {
		if _, err := NewClient(base, ""); err == nil {
			t.Errorf("NewClient(%q) should have failed", base)
		}
	}
}

// Before the first successful sync the producer answers 503: serving an empty
// document would tell the gateway to retire every discovered upstream.
func TestProducerServesNothingBeforeFirstSync(t *testing.T) {
	c, _ := NewClient("https://registry.invalid", "")
	p := &Producer{Client: c, Allowlist: &Allowlist{Servers: []Entry{{Name: "x/a"}}}, Log: testLog()}
	ts := httptest.NewServer(p.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("document status = %d, want 503", resp.StatusCode)
	}
	hresp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = hresp.Body.Close()
	if hresp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("health status = %d, want 503", hresp.StatusCode)
	}
}

func TestProducerServesAndGatesTheDocument(t *testing.T) {
	reg := fakeRegistry(t, map[string]string{"x/a": serverDoc("x/a", "active", true, httpRemote)})
	c, _ := NewClient(reg.URL, "")
	p := &Producer{
		Client:    c,
		Allowlist: &Allowlist{Servers: []Entry{{Name: "x/a"}}},
		Bearer:    "sync-token",
		Log:       testLog(),
	}
	p.sync(context.Background())

	ts := httptest.NewServer(p.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated fetch = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	req.Header.Set("Authorization", "Bearer sync-token")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authenticated fetch = %d, want 200", resp2.StatusCode)
	}
	var parsed config.Config
	if err := json.NewDecoder(resp2.Body).Decode(&parsed); err != nil {
		t.Fatalf("served document does not parse: %v", err)
	}
	if len(parsed.Upstreams) != 1 || parsed.Upstreams[0].ID != "x-a" {
		t.Fatalf("served document = %+v", parsed.Upstreams)
	}
}

// A registry outage keeps the last good document rather than publishing an
// empty one — which the gateway would read as "retire everything".
func TestTotalRegistryFailureKeepsTheLastDocument(t *testing.T) {
	fixtures := map[string]string{"x/a": serverDoc("x/a", "active", true, httpRemote)}
	var down bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v0.1/servers/"), "/versions/latest")
		body, ok := fixtures[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	c, _ := NewClient(srv.URL, "")
	p := &Producer{Client: c, Allowlist: &Allowlist{Servers: []Entry{{Name: "x/a"}}}, Log: testLog()}
	p.sync(context.Background())
	before := string(p.doc)
	if !strings.Contains(before, "x-a") {
		t.Fatalf("first sync did not produce the upstream: %s", before)
	}

	down = true
	p.sync(context.Background())
	if string(p.doc) != before {
		t.Errorf("document changed during a registry outage: %s", p.doc)
	}
	if p.lastErr == nil {
		t.Error("an outage should be recorded in health")
	}
}

// A single failing entry is not an outage: the rest of the allowlist still
// publishes, and the failed one drops out until it comes back.
func TestOnePartialFailureStillPublishes(t *testing.T) {
	reg := fakeRegistry(t, map[string]string{"x/a": serverDoc("x/a", "active", true, httpRemote)})
	c, _ := NewClient(reg.URL, "")
	p := &Producer{
		Client:    c,
		Allowlist: &Allowlist{Servers: []Entry{{Name: "x/a"}, {Name: "x/absent"}}},
		Log:       testLog(),
	}
	p.sync(context.Background())
	if p.upstreams != 1 {
		t.Fatalf("upstreams = %d, want 1", p.upstreams)
	}
	if p.lastErr != nil {
		t.Errorf("a partial failure is not a sync failure: %v", p.lastErr)
	}
}

func TestBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Stream far past the cap; the read must stop rather than follow.
		chunk := strings.Repeat("a", 1<<16)
		for range 200 {
			if _, err := io.WriteString(w, chunk); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	c, _ := NewClient(srv.URL, "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := c.Latest(ctx, "x/a"); err == nil {
		t.Fatal("expected the oversized body to fail parsing rather than be consumed whole")
	}
}
