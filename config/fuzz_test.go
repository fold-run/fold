package config_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/fold-run/fold/config"
)

// FuzzParse feeds arbitrary bytes through the config parser: it must never
// panic, and any document it accepts must survive a marshal → re-parse
// round trip (accepted configs are always re-encodable and still valid).
func FuzzParse(f *testing.F) {
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp"}]}`))
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp","namespace":"a"},{"id":"b","url":"http://b/mcp","namespace":"b"}],"routing":{"namespaceSeparator":"::"}}`))
	// The response bound: default (absent), an explicit value, and the
	// negative that disables it — a numeric field whose zero, positive, and
	// negative ranges all mean different things.
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp","maxResponseBytes":1048576}]}`))
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp","maxResponseBytes":-1}]}`))
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp","maxResponseBytes":2048}]}`)) // below the floor
	// Scope subjects, which are shared by policy rules and tenants and are
	// the one selector whose emptiness is rejected rather than ignored —
	// both the accepted shape and the refused one, so the mutator has each
	// side of that boundary to work from.
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp"}],"policy":{"defaultDecision":"deny","rules":[{"id":"r","subjects":{"scopes":["mcp:invoke","docs:read"]},"allow":[{"server":"a"}]}]}}`))
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp"}],"policy":{"rules":[{"id":"r","subjects":{"scopes":[""]},"allow":[{"server":"a"}]}]}}`))
	f.Add([]byte(`{"upstreams":[{"id":"a","url":"https://example.com/mcp"}],"tenants":[{"id":"t","subjects":{"groups":["eng"],"scopes":["admin"]}}]}`))
	f.Add([]byte(`{"upstreams":[]}`))
	f.Add([]byte(`{`))
	f.Add([]byte(`null`))
	if example, err := os.ReadFile("../fold.config.example.json"); err == nil {
		f.Add(example)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		cfg, err := config.Parse(data)
		if err != nil {
			return
		}
		if len(cfg.Upstreams) == 0 {
			t.Fatalf("accepted config with no upstreams: %q", data)
		}
		out, err := json.Marshal(cfg)
		if err != nil {
			t.Fatalf("re-marshal accepted config: %v", err)
		}
		if _, err := config.Parse(out); err != nil {
			t.Fatalf("re-parse of marshaled config failed: %v\noriginal: %q\nmarshaled: %q", err, data, out)
		}
	})
}
