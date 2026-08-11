package gateway

import (
	"io"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/fold-run/fold/config"
)

// introspectionConfig serves the read APIs with no console page.
func introspectionConfig(upstreams []config.Upstream) *config.Config {
	return &config.Config{
		Upstreams: upstreams,
		Server:    &config.ServerSection{Introspection: &config.Introspection{Enabled: true}},
	}
}

// The API and the page are separately configurable, which is the whole point
// of splitting server.console into server.introspection + server.console. An
// operator scripting against the federation snapshot should not have to serve
// a browser page to get it. This is the test that proves the split is real
// rather than a rename.
func TestIntrospectionWithoutConsole(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, introspectionConfig([]config.Upstream{{ID: "u", URL: up.URL}}))

	for _, path := range []string{"/api/federation", "/api/auth-hint"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s with introspection on: want 200, got %d", path, resp.StatusCode)
		}
		// The assets are embedded unconditionally, so reporting their commit
		// on a deployment that serves no page would name a build for
		// something that 404s.
		if path == "/api/federation" && strings.Contains(string(body), "consoleSource") {
			t.Errorf("/api/federation reports consoleSource with no console served: %s", body)
		}
	}
	for _, path := range []string{"/console/", "/console"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s with console off: want 404, got %d", path, resp.StatusCode)
		}
	}
}

// The v1.9 renames are a clean break, not an aliased deprecation: the old
// paths must be gone. Without this test nothing records that an alias was
// considered and declined, and a future contributor would reasonably add one.
func TestLegacyConsoleAPIPathsAreGone(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, consoleConfig([]config.Upstream{{ID: "u", URL: up.URL}}))

	for _, path := range []string{"/console/api/state", "/console/api/auth"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		resp.Body.Close()
		// The console asset handler serves /console/, so a stale path lands
		// there and 404s out of the embedded FS rather than the mux — either
		// way it must not answer.
		if resp.StatusCode == http.StatusOK {
			t.Errorf("GET %s: still answering 200 — the v1.9 rename is meant to be a clean break", path)
		}
	}
}

// The console assets are vendored from fold-run/fold-console, so what lands
// in gateway/console/ is decided upstream. //go:embed takes the whole
// directory, so a sloppy sync would ship that repo's README, LICENSE, or
// .github/ into every operator's binary and serve it under /console/. This is
// the in-repo veto: adding a file to the shipped set is a reviewed change
// here, not a unilateral one upstream.
func TestConsoleEmbedManifest(t *testing.T) {
	want := map[string]bool{
		"console/index.html":                        true,
		"console/app.js":                            true,
		"console/style.css":                         true,
		"console/fonts/IBMPlexSans-400-latin.woff2": true,
		"console/fonts/IBMPlexSans-600-latin.woff2": true,
		"console/fonts/GeistMono-400-latin.woff2":   true,
		"console/fonts/GeistMono-600-latin.woff2":   true,
		// OFL 1.1 requires the licence accompany the Font Software, and the
		// subsets above are embedded in every binary — so this file shipping
		// is a compliance property, not housekeeping.
		"console/fonts/OFL.txt": true,
	}

	got := map[string]bool{}
	err := fs.WalkDir(consoleFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			got[path] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	for p := range want {
		if !got[p] {
			t.Errorf("embedded console is missing %s", p)
		}
	}
	for p := range got {
		if !want[p] {
			t.Errorf("embedded console ships an unexpected file: %s — it would be served at /%s", p, p)
		}
	}
}

// The vendored console can skew against the gateway: it is pinned at a commit
// and bumped by a separate PR, so a bump to a build that still talks to the
// pre-v1.9 paths would leave a dashboard that silently never loads. No other
// Go test can see that — the assets are opaque bytes to the rest of the suite
// — which is what earns a string-grep test its place here.
func TestConsoleAssetsTargetCurrentAPI(t *testing.T) {
	b, err := consoleFS.ReadFile("console/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)

	for _, want := range []string{"api/federation", "api/auth-hint"} {
		if !strings.Contains(js, want) {
			t.Errorf("vendored app.js never references %q — is the console pin older than the v1.9 rename?", want)
		}
	}
	for _, stale := range []string{`fetch("api/state")`, `fetch("api/auth")`} {
		if strings.Contains(js, stale) {
			t.Errorf("vendored app.js still calls %s — the pre-v1.9 path", stale)
		}
	}
	// Config keys the page names in its own error text. A stale one here is
	// worse than a stale path: fold parses config with DisallowUnknownFields,
	// so an operator who follows the hint writes a config that refuses to
	// start. The 403 banner is shown by exactly the surface the allowlist
	// gates, which makes it the likeliest place for this to rot.
	for _, stale := range []string{"server.console.groups"} {
		if strings.Contains(js, stale) {
			t.Errorf("vendored app.js names %q, a config key that no longer exists", stale)
		}
	}
	// The fetches are relative to the page base so the console survives a
	// prefix a fronting proxy adds; an absolute /api/... would resolve to the
	// origin root and break every path-prefixed deployment.
	if strings.Contains(js, `fetch("/api/`) {
		t.Error(`vendored app.js uses an absolute fetch("/api/...") — must be "../api/..." to survive a proxy path prefix`)
	}
}
