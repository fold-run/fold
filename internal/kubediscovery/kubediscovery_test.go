package kubediscovery

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fold-run/fold/config"
)

func service(ns, name string, annotations map[string]string, ports ...int) Service {
	var s Service
	s.Metadata.Name, s.Metadata.Namespace, s.Metadata.Annotations = name, ns, annotations
	for _, p := range ports {
		s.Spec.Ports = append(s.Spec.Ports, struct {
			Name string `json:"name"`
			Port int    `json:"port"`
		}{Port: p})
	}
	return s
}

func TestMapServices(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	t.Run("defaults derive cluster DNS", func(t *testing.T) {
		ups := MapServices([]Service{service("prod", "search", nil, 8080)}, MapOptions{UnprefixedIDs: true}, log)
		if len(ups) != 1 {
			t.Fatalf("got %d upstreams", len(ups))
		}
		u := ups[0]
		if u.ID != "search" || u.Namespace != "search" ||
			u.URL != "http://search.prod.svc.cluster.local:8080/mcp" {
			t.Errorf("mapped %+v", u)
		}
	})

	t.Run("annotations override", func(t *testing.T) {
		svc := service("prod", "search-svc", map[string]string{
			AnnID:        "ml-search",
			AnnNamespace: "search",
			AnnPort:      "9000",
			AnnPath:      "/api/mcp",
			AnnScheme:    "https",
		}, 8080)
		u := MapServices([]Service{svc}, MapOptions{UnprefixedIDs: true}, log)[0]
		if u.ID != "ml-search" || u.Namespace != "search" ||
			u.URL != "https://search-svc.prod.svc.cluster.local:9000/api/mcp" {
			t.Errorf("mapped %+v", u)
		}
	})

	t.Run("mcp-named port wins over first", func(t *testing.T) {
		var svc Service
		svc.Metadata.Name, svc.Metadata.Namespace = "s", "ns"
		svc.Spec.Ports = append(svc.Spec.Ports,
			struct {
				Name string `json:"name"`
				Port int    `json:"port"`
			}{Name: "http", Port: 80},
			struct {
				Name string `json:"name"`
				Port int    `json:"port"`
			}{Name: "mcp", Port: 8080})
		if u := MapServices([]Service{svc}, MapOptions{UnprefixedIDs: true}, log)[0]; !strings.Contains(u.URL, ":8080/") {
			t.Errorf("url %q should use the mcp-named port", u.URL)
		}
	})

	t.Run("url annotation replaces derivation", func(t *testing.T) {
		svc := service("prod", "ext", map[string]string{AnnURL: "https://mcp.vendor.example/mcp"})
		if u := MapServices([]Service{svc}, MapOptions{UnprefixedIDs: true}, log)[0]; u.URL != "https://mcp.vendor.example/mcp" {
			t.Errorf("url = %q", u.URL)
		}
	})

	t.Run("config annotation carries full upstream fields", func(t *testing.T) {
		svc := service("prod", "gh", map[string]string{
			AnnConfig: `{"rateLimit":{"requestsPerMinute":600},"auth":{"strategy":"static","secretRef":"GH_KEY"}}`,
		}, 8080)
		opts := MapOptions{UnprefixedIDs: true, AllowedAuthStrategies: []string{"static"}, AllowedSecretRefs: []string{"GH_KEY"}}
		u := MapServices([]Service{svc}, opts, log)[0]
		if u.RateLimit == nil || u.RateLimit.RequestsPerMinute != 600 ||
			u.Auth == nil || u.Auth.SecretRef != "GH_KEY" {
			t.Errorf("config annotation not applied: %+v", u)
		}
		if u.ID != "gh" || u.URL == "" {
			t.Errorf("defaults not filled around annotation config: %+v", u)
		}
	})

	t.Run("invalid entries and collisions are skipped, output sorted", func(t *testing.T) {
		ups := MapServices([]Service{
			service("prod", "zeta", nil, 80),
			service("prod", "Bad_Name", nil, 80),                                  // invalid id
			service("prod", "noport", nil),                                        // no ports, no url
			service("prod", "alpha", nil, 80),                                     // fine
			service("other", "zeta", nil, 80),                                     // contests "zeta"
			service("prod", "badcfg", map[string]string{AnnConfig: `{"nope":1}`}), // unknown field
		}, MapOptions{UnprefixedIDs: true}, log)
		var ids []string
		for _, u := range ups {
			ids = append(ids, u.ID)
		}
		// Contested ids fail closed: *both* "zeta" claimants are dropped, so
		// list order cannot hand an identity to whoever sorts earlier.
		if strings.Join(ids, ",") != "alpha" {
			t.Errorf("ids = %v, want [alpha] (contested zeta dropped entirely)", ids)
		}
	})

	t.Run("passthrough validates without knowing the gateway, when allowed", func(t *testing.T) {
		svc := service("prod", "pt", map[string]string{AnnConfig: `{"auth":{"strategy":"passthrough"}}`}, 80)
		opts := MapOptions{UnprefixedIDs: true, AllowedAuthStrategies: []string{"passthrough"}}
		if ups := MapServices([]Service{svc}, opts, log); len(ups) != 1 {
			t.Fatalf("allowed passthrough entry rejected")
		}
	})

	t.Run("credentialed strategies are denied by default", func(t *testing.T) {
		// The exfiltration scenario: a Service names a gateway-held secret
		// and its own destination URL. The zero-value options refuse it.
		exfil := service("dev", "evil", map[string]string{
			AnnURL:    "https://attacker.example/mcp",
			AnnConfig: `{"auth":{"strategy":"static","secretRef":"FOLD_EMA_KEY"}}`,
		})
		if ups := MapServices([]Service{exfil}, MapOptions{UnprefixedIDs: true}, log); len(ups) != 0 {
			t.Fatalf("credentialed service passed default-deny: %+v", ups)
		}
		// Strategy allowed but the secret name is not → still denied.
		opts := MapOptions{UnprefixedIDs: true, AllowedAuthStrategies: []string{"static"}, AllowedSecretRefs: []string{"ML_KEY"}}
		if ups := MapServices([]Service{exfil}, opts, log); len(ups) != 0 {
			t.Fatalf("unlisted secretRef passed the allowlist: %+v", ups)
		}
		// clientAuth secretRefs are gated the same way.
		cc := service("dev", "cc", map[string]string{
			AnnConfig: `{"auth":{"strategy":"client-credentials","tokenEndpoint":"https://idp.example/token","clientId":"c","clientAuth":{"type":"client_secret_post","secretRef":"STOLEN"}}}`,
		}, 80)
		opts = MapOptions{UnprefixedIDs: true, AllowedAuthStrategies: []string{"client-credentials"}}
		if ups := MapServices([]Service{cc}, opts, log); len(ups) != 0 {
			t.Fatalf("clientAuth secretRef passed default-deny: %+v", ups)
		}
	})

	t.Run("credential destinations are bounded", func(t *testing.T) {
		// Naming an *allowed* secret is not enough: the destination is the
		// other half of the exposure, and the Service author picks it.
		opts := MapOptions{
			UnprefixedIDs:          true,
			AllowedAuthStrategies:  []string{"static", "client-credentials"},
			AllowedSecretRefs:      []string{"OK_KEY", "OK_CLIENT"},
			AllowedCredentialHosts: []string{"*.svc.cluster.local"},
		}
		exfil := service("dev", "exfil", map[string]string{
			AnnURL:    "https://attacker.example/mcp",
			AnnConfig: `{"auth":{"strategy":"static","secretRef":"OK_KEY"}}`,
		})
		if ups := MapServices([]Service{exfil}, opts, log); len(ups) != 0 {
			t.Errorf("allowed secret sent to a disallowed host: %+v", ups)
		}
		// The token endpoint receives the client secret — gated the same way.
		tok := service("dev", "tok", map[string]string{
			AnnConfig: `{"auth":{"strategy":"client-credentials","tokenEndpoint":"https://attacker.example/token","clientId":"c","clientAuth":{"type":"client_secret_post","secretRef":"OK_CLIENT"}}}`,
		}, 80)
		if ups := MapServices([]Service{tok}, opts, log); len(ups) != 0 {
			t.Errorf("client secret sent to a disallowed token endpoint: %+v", ups)
		}
		// In-cluster destination with an allowed secret is fine.
		ok := service("dev", "fine", map[string]string{
			AnnConfig: `{"auth":{"strategy":"static","secretRef":"OK_KEY"}}`,
		}, 80)
		if ups := MapServices([]Service{ok}, opts, log); len(ups) != 1 {
			t.Errorf("legitimate in-cluster credentialed upstream rejected")
		}
	})

	t.Run("attribution and probe rate are not self-asserted", func(t *testing.T) {
		svc := service("dev", "sneaky", map[string]string{
			AnnConfig: `{"owner":{"org":"platform-team","team":"trusted"},"healthCheck":{"intervalMs":1}}`,
		}, 80)
		u := MapServices([]Service{svc}, MapOptions{UnprefixedIDs: true}, log)[0]
		if u.Owner == nil || u.Owner.Org != "dev" || u.Owner.Team != "sneaky" {
			t.Errorf("owner not overwritten with the source namespace: %+v", u.Owner)
		}
		if u.HealthCheck == nil || u.HealthCheck.IntervalMs != 1000 {
			t.Errorf("probe interval not floored: %+v", u.HealthCheck)
		}
	})

	t.Run("reserved ids and namespaces cannot be claimed", func(t *testing.T) {
		opts := MapOptions{UnprefixedIDs: true, ReservedIDs: []string{"github"}}
		ups := MapServices([]Service{
			service("dev", "github", nil, 80),                                         // reserved id
			service("dev", "impostor", map[string]string{AnnNamespace: "github"}, 80), // reserved namespace
			service("dev", "fine", nil, 80),
		}, opts, log)
		if len(ups) != 1 || ups[0].ID != "fine" {
			t.Errorf("reserved claims not skipped: %+v", ups)
		}
	})

	t.Run("namespace-prefixed ids stop cross-namespace squatting", func(t *testing.T) {
		opts := MapOptions{} // prefixing is the default
		ups := MapServices([]Service{
			service("aaa", "search", nil, 80),                                    // → aaa-search
			service("prod", "search", nil, 80),                                   // → prod-search: coexists
			service("dev", "squat", map[string]string{AnnID: "prod-search"}, 80), // wrong id prefix
			// The MCP namespace is the routing identity clients see, so it
			// carries the same requirement — an unprefixed claim on another
			// team's tool namespace must be refused, not left to list order.
			service("dev", "nssquat", map[string]string{AnnNamespace: "prod-search"}, 80),
			service("dev", "cfgsquat", map[string]string{AnnConfig: `{"namespace":"payments"}`}, 80),
		}, opts, log)
		var ids []string
		for _, u := range ups {
			ids = append(ids, u.ID+"/"+u.Namespace)
		}
		if strings.Join(ids, ",") != "aaa-search/aaa-search,prod-search/prod-search" {
			t.Errorf("ids = %v, want [aaa-search/aaa-search prod-search/prod-search]", ids)
		}

		// A prefixed namespace within the team's own space is fine.
		ok := MapServices([]Service{
			service("dev", "svc", map[string]string{AnnNamespace: "dev-tools"}, 80),
		}, opts, log)
		if len(ok) != 1 || ok[0].Namespace != "dev-tools" {
			t.Errorf("legitimate prefixed namespace rejected: %+v", ok)
		}
	})
}

// fakeKubeAPI serves a ServiceList; store services to change the answer,
// flip failing to simulate an API outage.
func fakeKubeAPI(t *testing.T) (ts *httptest.Server, services *atomic.Value, failing *atomic.Bool) {
	t.Helper()
	services = &atomic.Value{}
	services.Store([]Service{})
	failing = &atomic.Bool{}
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": services.Load()})
	}))
	t.Cleanup(ts.Close)
	return ts, services, failing
}

func TestProducerServesAndFailsSafe(t *testing.T) {
	api, services, failing := fakeKubeAPI(t)
	services.Store([]Service{service("prod", "search", nil, 8080)})

	client, err := NewClient(api.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	p := &Producer{Client: client, Log: slog.New(slog.DiscardHandler), Bearer: "doc-token"}
	front := httptest.NewServer(p.Handler())
	t.Cleanup(front.Close)

	get := func() (int, string) {
		req, _ := http.NewRequest(http.MethodGet, front.URL+"/upstreams.json", nil)
		req.Header.Set("Authorization", "Bearer doc-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// Before the first sync: 503 (even authenticated), so a consuming
	// gateway keeps its last good state rather than swallowing an empty doc.
	if code, _ := get(); code != http.StatusServiceUnavailable {
		t.Fatalf("pre-sync status = %d, want 503", code)
	}
	// The bearer gate answers before anything else.
	resp, err := http.Get(front.URL + "/upstreams.json")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}

	p.sync(context.Background())

	code, body := get()
	if code != http.StatusOK || !strings.Contains(body, "search.prod.svc.cluster.local") {
		t.Fatalf("doc = %d %q", code, body)
	}

	// The document parses as fold discovery input.
	var doc struct {
		Upstreams []config.Upstream `json:"upstreams"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil || len(doc.Upstreams) != 1 {
		t.Fatalf("document not consumable: %v %q", err, body)
	}

	// API outage: last good document keeps serving; /health reports it.
	failing.Store(true)
	p.sync(context.Background())
	if code, body2 := get(); code != http.StatusOK || body2 != body {
		t.Errorf("outage changed the served document: %d %q", code, body2)
	}
	hresp, err := http.Get(front.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer hresp.Body.Close()
	var health struct {
		Synced bool   `json:"synced"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(hresp.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if !health.Synced || health.Error == "" {
		t.Errorf("health after outage = %+v, want synced with error", health)
	}
	// /health is unauthenticated: with the document gated by a bearer
	// token, it reports a category rather than raw kube-API error text
	// (which can name internal hosts and token paths).
	if strings.Contains(health.Error, api.URL) || health.Error != "sync failing" {
		t.Errorf("health leaked error detail: %q", health.Error)
	}
}

func TestClientSendsSelectorAndToken(t *testing.T) {
	var gotPath, gotSelector atomic.Value
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotSelector.Store(r.URL.Query().Get("labelSelector"))
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []Service{}})
	}))
	t.Cleanup(api.Close)

	tokenFile := t.TempDir() + "/token"
	if err := os.WriteFile(tokenFile, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(api.URL, tokenFile, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListServices(context.Background(), "prod", "fold.run/upstream=true"); err != nil {
		t.Fatal(err)
	}
	if gotPath.Load() != "/api/v1/namespaces/prod/services" {
		t.Errorf("path = %v", gotPath.Load())
	}
	if gotSelector.Load() != "fold.run/upstream=true" {
		t.Errorf("selector = %v", gotSelector.Load())
	}
}

// TestNamespacePrefixIsUnambiguous: a plain "<ns>-" prefix is not injective
// because hyphens are legal in namespace names — namespace "team" with
// Service "a-billing" and namespace "team-a" with Service "billing" would
// both derive "team-a-billing", letting either forge the other's identity
// or (under fail-closed collision handling) evict it. Escaping hyphens makes
// the mapping one-to-one, so both coexist.
func TestNamespacePrefixIsUnambiguous(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	ups := MapServices([]Service{
		service("team-a", "billing", nil, 80),
		service("team", "a-billing", nil, 80),
	}, MapOptions{}, log)
	var ids []string
	for _, u := range ups {
		ids = append(ids, u.ID)
	}
	if len(ups) != 2 {
		t.Fatalf("distinct namespaces collided: %v", ids)
	}
	if ids[0] == ids[1] {
		t.Errorf("identities are not injective: %v", ids)
	}

	// And a Service still cannot forge another namespace's prefix.
	forged := MapServices([]Service{
		service("team", "x", map[string]string{AnnID: "team-a-billing"}, 80),
	}, MapOptions{}, log)
	if len(forged) != 0 {
		t.Errorf("forged cross-namespace id accepted: %+v", forged)
	}
}

// The pre-v1.5 /healthz path must reach the health summary, not fall
// through to the document handler — a probe that silently starts scraping
// the upstreams document reports healthy on the wrong thing.
func TestProducerHealthzAlias(t *testing.T) {
	p := &Producer{}
	p.doc = []byte(`{"upstreams":[]}`)
	srv := httptest.NewServer(p.Handler())
	defer srv.Close()

	for _, path := range []string{"/health", "/healthz"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s status = %d", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), `"synced"`) {
			t.Errorf("%s served the document, not the health summary: %s", path, body)
		}
	}
}
