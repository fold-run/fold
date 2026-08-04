package gateway

import (
	"testing"

	"github.com/fold-run/fold/config"
)

// FuzzResolve checks the naming round trip at the heart of federation: for
// any (separator, namespace, bare name) combination that config validation
// accepts, resolve(public(name)) must return the same upstream and the same
// bare name — and resolve must never panic on arbitrary input.
func FuzzResolve(f *testing.F) {
	f.Add("__", "github", "search")
	f.Add("::", "team-a", "tool__x")
	f.Add("-", "my-ns", "t") // separator colliding with the namespace alphabet
	f.Add("", "ns", "name")  // empty separator falls back to the default
	f.Add("_", "ns0", "a_b")
	f.Add("__", "", "plain") // no namespace: passthrough mode

	f.Fuzz(func(t *testing.T, sep, ns, bare string) {
		cfg := &config.Config{
			Upstreams: []config.Upstream{{ID: "up", URL: "https://example.com/mcp", Namespace: ns}},
			Routing:   &config.Routing{NamespaceSeparator: sep},
		}
		if err := cfg.Validate(); err != nil {
			return // invalid combinations are the parser's problem, not routing's
		}

		u := &upstream{cfg: cfg.Upstreams[0]}
		g := &Gateway{
			sep:         cfg.NamespaceSeparator(),
			passthrough: cfg.Passthrough(),
			upstreams:   []*upstream{u},
			byNamespace: map[string]*upstream{},
		}
		if ns != "" {
			g.byNamespace[ns] = u
		}

		pub := g.public(u, bare)
		got, gotBare, err := g.resolve(pub)
		if err != nil {
			t.Fatalf("resolve(public(%q)) = err %v (sep %q, ns %q, public %q)", bare, err, g.sep, ns, pub)
		}
		if got != u || gotBare != bare {
			t.Fatalf("resolve(public(%q)) = (%v, %q), want (up, %q) (sep %q, ns %q, public %q)",
				bare, got.cfg.ID, gotBare, bare, g.sep, ns, pub)
		}

		// Arbitrary names must never panic, whatever they resolve to.
		_, _, _ = g.resolve(bare)
		_, _, _ = g.resolve(sep + bare + sep)
	})
}
