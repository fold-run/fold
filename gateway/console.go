package gateway

import (
	"embed"
	"io/fs"
	"net/http"
	"net/url"

	"github.com/fold-run/fold/config"
)

// The fold console page (docs/design-console.md): a read-only observability
// dashboard plus an MCP test console, served at /console when
// server.console.enabled is set. The test console is a plain MCP client
// against the gateway's own /mcp endpoint, so console traffic runs the full
// pipeline — policy, rate limits, and audit apply exactly as they do for
// any other client, and there is no privileged path to bypass them. The
// dashboard reads /api/federation like any other client of that API, which
// is why the page requires server.introspection.enabled.
//
// The assets are NOT maintained here. Their source is fold-run/fold-console;
// gateway/console/ is vendored output produced by scripts/sync-console.sh at
// the commit recorded in consoleSource, and CI reverts hand edits. Send UI
// changes upstream. They stay checked in rather than fetched at build time
// because the Go module proxy is fold's distribution channel: `go run
// github.com/fold-run/fold/cmd/fold@latest` must build from the proxy zip
// alone, which runs no generators and carries no submodules.

//go:embed console
var consoleFS embed.FS

// consoleCSP builds the console's Content-Security-Policy. Base policy pins
// everything to this origin; a configured OAuth issuer's origin is added to
// connect-src so the page can fetch the AS metadata and exchange the code
// (top-level navigation to the authorize endpoint is not CSP-gated). The
// allowance is config-derived — never a wildcard.
func consoleCSP(cfg *config.Config) string {
	connect := "'self'"
	if iss, err := cfg.ConsoleOAuthIssuer(); err == nil {
		if u, err := url.Parse(iss.Issuer); err == nil && u.Scheme != "" && u.Host != "" {
			connect += " " + u.Scheme + "://" + u.Host
		}
	}
	return "default-src 'self'; connect-src " + connect +
		"; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
}

// consoleAssetHandler serves the embedded console page under /console/.
// Assets are static and identical for every caller — no data, so no auth —
// and csp pins every fetch the page makes to this origin (plus the OAuth
// issuer's origin when sign-in is configured; see consoleCSP).
func consoleAssetHandler(csp string) http.Handler {
	sub, err := fs.Sub(consoleFS, "console")
	if err != nil {
		// The embed is part of the binary; a missing subdirectory is a
		// build defect, not a runtime condition.
		panic(err)
	}
	files := http.StripPrefix("/console/", http.FileServer(http.FS(sub)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		files.ServeHTTP(w, r)
	})
}
