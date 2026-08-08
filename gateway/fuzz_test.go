package gateway

import (
	"slices"
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

		// An upstream carries the gateway's separator (newWiredUpstream), so
		// the fixture must too — public and resolve are only inverses when
		// they agree on it.
		u := &upstream{cfg: cfg.Upstreams[0], sep: cfg.NamespaceSeparator()}
		g := &Gateway{sep: cfg.NamespaceSeparator()}
		rt := &routes{
			passthrough: cfg.Passthrough(),
			upstreams:   []*upstream{u},
			byNamespace: map[string]*upstream{},
		}
		if ns != "" {
			rt.byNamespace[ns] = u
		}

		pub := g.public(u, bare)
		got, gotBare, err := g.resolve(rt, pub)
		if err != nil {
			t.Fatalf("resolve(public(%q)) = err %v (sep %q, ns %q, public %q)", bare, err, g.sep, ns, pub)
		}
		if got != u || gotBare != bare {
			t.Fatalf("resolve(public(%q)) = (%v, %q), want (up, %q) (sep %q, ns %q, public %q)",
				bare, got.cfg.ID, gotBare, bare, g.sep, ns, pub)
		}

		// Arbitrary names must never panic, whatever they resolve to.
		_, _, _ = g.resolve(rt, bare)
		_, _, _ = g.resolve(rt, sep+bare+sep)
	})
}

// FuzzListCursor hammers the pagination cursor decoder with arbitrary
// client-supplied cursors — the one fully attacker-controlled input on the
// list path. Invariants: never panic; a rejected cursor yields nothing; an
// accepted cursor yields a bounded, in-order slice of the snapshot; a
// minted continuation cursor is honored on the same snapshot.
func FuzzListCursor(f *testing.F) {
	items := []string{"a", "b", "c", "d", "e", "f", "g"}
	name := func(s string) string { return s }

	// Seed with a genuinely minted cursor and mutations of it.
	_, minted, _ := paginate(items, name, "tools", "", 3, nil)
	f.Add(minted)
	f.Add(minted + "x")
	f.Add("")
	f.Add("!!!not-base64!!!")
	f.Add("eyJrIjoidG9vbHMiLCJvIjo5OTksImciOiIwMDAwMDAiLCJwIjoiMDAwMDAwIn0") // forged offset/gen
	f.Add("bnVsbA")                                                          // base64("null")

	f.Fuzz(func(t *testing.T, raw string) {
		page, next, err := paginate(items, name, "tools", raw, 3, nil)
		if err != nil {
			if page != nil || next != "" {
				t.Fatalf("rejected cursor %q still returned page=%v next=%q", raw, page, next)
			}
			return
		}
		if len(page) > 3 {
			t.Fatalf("page of %d items exceeds size 3", len(page))
		}
		// An accepted page is a contiguous window of the snapshot.
		if len(page) > 0 {
			start := slices.Index(items, page[0])
			if start < 0 || start+len(page) > len(items) || !slices.Equal(page, items[start:start+len(page)]) {
				t.Fatalf("page %v is not a window of %v", page, items)
			}
		}
		// A minted continuation must work against the same snapshot.
		if next != "" {
			if _, _, err := paginate(items, name, "tools", next, 3, nil); err != nil {
				t.Fatalf("minted cursor %q rejected: %v", next, err)
			}
		}
	})
}

// FuzzDiscoveryDoc drives arbitrary bytes through the discovery-document
// parser and merged-config validation — the path a compromised registry
// would attack. Neither step may panic, whatever the document contains.
func FuzzDiscoveryDoc(f *testing.F) {
	f.Add([]byte(`{"upstreams":[{"id":"b","url":"http://x.test","namespace":"b"}]}`))
	f.Add([]byte(`{"upstreams":[]}`))
	f.Add([]byte(`{"upstreams":null}`))
	f.Add([]byte(`{`))
	f.Add([]byte(`{"nope":1}`))
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"http://x.test"}]}`)) // collides with the fuzz base
	f.Add([]byte(`{"upstreams":[{"id":"b","urls":["http://x.test","http://x.test"],"namespace":"b"}]}`))

	base := &config.Config{Upstreams: []config.Upstream{{ID: "a", URL: "http://a.test", Namespace: "a"}}}
	f.Fuzz(func(t *testing.T, data []byte) {
		ups, err := parseDiscoveryDoc(data)
		if err != nil {
			return
		}
		// Mirror applyLocked's merge; Validate decides accept/reject — the
		// fuzz property is only that neither outcome panics.
		merged := *base
		merged.Upstreams = append(append([]config.Upstream{}, base.Upstreams...), ups...)
		_ = merged.Validate()
	})
}
