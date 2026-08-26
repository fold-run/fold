package gateway

import (
	"bytes"
	"encoding/json"
	"reflect"
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

// FuzzSanitizeRawMeta feeds arbitrary bytes through the task-params
// sanitizer. Task params are the one params object fold forwards as opaque
// JSON, so this is a caller-controlled parser on the proxy path, and the
// properties it must hold are unconditional: never panic; never mutate the
// caller's bytes; hand back an unparsable blob exactly as it arrived (fold
// does not interpret this path, so it cannot start failing calls over it);
// and, for anything it does rewrite, emit a params object that still parses,
// still carries every other field, and carries none of the connection-owned
// keys.
func FuzzSanitizeRawMeta(f *testing.F) {
	f.Add([]byte(`{"taskId":"t-1"}`))
	f.Add([]byte(`{"taskId":"t-1","_meta":{"io.modelcontextprotocol/clientInfo":{"name":"x"}}}`))
	f.Add([]byte(`{"taskId":"t-1","_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","example.com/vendor":"keep"}}`))
	f.Add([]byte(`{"taskId":"t-1","_meta":{"io.modelcontextprotocol/clientCapabilities":{}}}`))
	// The continuation of a multi-round-trip retry (SEP-2322) rides in the
	// same params object as the `_meta` being stripped. fold is forbidden to
	// inspect or modify `requestState`, so the sanitizer must leave both
	// fields exactly as they arrived — including a value that looks like the
	// namespaced key it is scanning for.
	f.Add([]byte(`{"taskId":"t-1","requestState":"step=2","_meta":{"io.modelcontextprotocol/clientInfo":{"name":"x"}}}`))
	f.Add([]byte(`{"requestState":"io.modelcontextprotocol/clientInfo","inputResponses":{"confirm":{"action":"accept","content":{"ok":true}}},"_meta":{"io.modelcontextprotocol/clientCapabilities":{},"example.com/vendor":"keep"}}`))
	f.Add([]byte(`{"_meta":{"io.modelcontextprotocol/clientInfo":{"name":"x"}},"_meta":{"a":1}}`)) // duplicate key
	f.Add([]byte(`{"taskId":"t-1","_meta":"io.modelcontextprotocol/clientInfo"}`))                 // _meta is not an object
	f.Add([]byte(`{"taskId":"io.modelcontextprotocol/clientInfo"}`))                               // namespace outside _meta
	f.Add([]byte(`{"taskId":"t-1","_meta":{"io.modelcontextprotocol/clientInfo":`))                // truncated
	f.Add([]byte(`[]`))
	f.Add([]byte(``))
	f.Add([]byte(`io.modelcontextprotocol/`))

	f.Fuzz(func(t *testing.T, data []byte) {
		original := append([]byte(nil), data...)
		out := sanitizeRawMeta(data)
		if !bytes.Equal(data, original) {
			t.Fatalf("sanitizeRawMeta mutated the caller's bytes: %q became %q", original, data)
		}

		var params map[string]json.RawMessage
		if err := json.Unmarshal(data, &params); err != nil {
			if !bytes.Equal(out, data) {
				t.Fatalf("params fold cannot parse were rewritten: %q → %q", data, out)
			}
			return
		}
		var got map[string]json.RawMessage
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("sanitized params no longer parse: %v (%q → %q)", err, data, out)
		}
		var meta map[string]json.RawMessage
		if raw, ok := got["_meta"]; ok && json.Unmarshal(raw, &meta) == nil {
			for _, k := range connectionRequestMetaKeys {
				if _, bad := meta[k]; bad {
					t.Fatalf("connection key %q survived: %q → %q", k, data, out)
				}
			}
			// An already-empty `_meta` is the caller's to send; only one
			// fold emptied itself must be dropped rather than left behind.
			if len(meta) == 0 && !bytes.Equal(out, data) {
				t.Fatalf("an emptied _meta was kept rather than removed: %q → %q", data, out)
			}
		}
		for k, want := range params {
			if k == "_meta" {
				continue
			}
			var wantAny, gotAny any
			if err := json.Unmarshal(want, &wantAny); err != nil {
				continue // a value fold's own decoder rejects is not the property under test
			}
			raw, ok := got[k]
			if !ok {
				t.Fatalf("field %q was dropped: %q → %q", k, data, out)
			}
			if err := json.Unmarshal(raw, &gotAny); err != nil || !reflect.DeepEqual(wantAny, gotAny) {
				t.Fatalf("field %q changed: %q → %q", k, data, out)
			}
		}
	})
}

// FuzzListCacheScope feeds arbitrary config documents through the caching
// hint fold stamps on every list it builds. The property is the one the
// specification states and the one fold got wrong: the scope is "public" or
// "private" and nothing else — not the empty string, and not a value derived
// from anything a document happens to contain. Nothing about a configuration
// can talk fold into a third answer.
func FuzzListCacheScope(f *testing.F) {
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp"}]}`))
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp"}],"policy":{"defaultDecision":"deny"}}`))
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp"}],"policy":{"defaultDecision":"allow"}}`))
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp"}],"policy":{"defaultDecision":"allow","rules":[{"id":"r","allow":[{"server":"a"}]}]}}`))
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp"}],"tenants":[{"id":"t","subjects":{"groups":["eng"]}}]}`))
	// A decision fold does not define: neither "deny" nor "allow" may become
	// a third scope.
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp"}],"policy":{"defaultDecision":"public"}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		var cfg config.Config
		if err := json.Unmarshal(data, &cfg); err != nil {
			return
		}
		// Both an unvalidated document and one fold would accept: the hint is
		// read off the snapshot on the request path, so the property has to
		// hold before validation has had a say as well as after.
		for _, got := range []string{
			listCacheScope(&cfg, nil),
			listCacheScope(&cfg, []*upstream{{}}),
		} {
			if got != cacheScopePublic && got != cacheScopePrivate {
				t.Fatalf("listCacheScope = %q for %q, which is not a scope the specification defines", got, data)
			}
		}
	})
}
