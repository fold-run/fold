package config

import (
	"strings"
	"testing"
)

func TestMinimalConfig(t *testing.T) {
	cfg, err := Parse([]byte(`{"upstreams":[{"id":"github","url":"https://mcp.example.com/mcp","namespace":"github"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MCPPath() != "/mcp" || cfg.NamespaceSeparator() != "__" {
		t.Error("defaults not applied")
	}
	if cfg.Passthrough() {
		t.Error("namespaced single upstream is not passthrough")
	}
}

func TestPassthroughDetection(t *testing.T) {
	cfg, err := Parse([]byte(`{"upstreams":[{"id":"solo","url":"http://x.test/mcp"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Passthrough() {
		t.Error("single un-namespaced upstream should be passthrough")
	}
}

func TestValidationErrors(t *testing.T) {
	cases := []struct {
		name, json, wantErr string
	}{
		{"no upstreams", `{"upstreams":[]}`, "at least one upstream"},
		{"bad id", `{"upstreams":[{"id":"Bad_ID","url":"http://x.test"}]}`, "lowercase"},
		{"dup id", `{"upstreams":[{"id":"a","url":"http://x.test","namespace":"a"},{"id":"a","url":"http://y.test","namespace":"b"}]}`, "duplicate id"},
		{"missing namespace", `{"upstreams":[{"id":"a","url":"http://x.test","namespace":"a"},{"id":"b","url":"http://y.test"}]}`, "namespace is required"},
		{"dup namespace", `{"upstreams":[{"id":"a","url":"http://x.test","namespace":"n"},{"id":"b","url":"http://y.test","namespace":"n"}]}`, "duplicate namespace"},
		{"bad url", `{"upstreams":[{"id":"a","url":"not-a-url"}]}`, "http(s)"},
		{"auth without resource", `{"upstreams":[{"id":"a","url":"http://x.test"}],"auth":{"mode":"required","issuers":[{"issuer":"https://idp.test"}]}}`, "resource"},
		{"auth without issuers", `{"upstreams":[{"id":"a","url":"http://x.test"}],"auth":{"mode":"required","resource":"https://gw.test"}}`, "issuer"},
		{"static without secret", `{"upstreams":[{"id":"a","url":"http://x.test","auth":{"strategy":"static"}}]}`, "secretRef"},
		{"exchange without audience", `{"upstreams":[{"id":"a","url":"http://x.test","auth":{"strategy":"token-exchange","tokenEndpoint":"https://t.test","clientId":"c","clientAuth":{"type":"client_secret_post","secretRef":"S"}}}]}`, "audience"},
		{"policy unknown server", `{"upstreams":[{"id":"a","url":"http://x.test"}],"policy":{"rules":[{"id":"r","allow":[{"server":"nope"}]}]}}`, "unknown server"},
		{"webhook without url", `{"upstreams":[{"id":"a","url":"http://x.test"}],"audit":{"sinks":[{"type":"webhook"}]}}`, "url"},
		{"unknown field", `{"upstreams":[{"id":"a","url":"http://x.test"}],"nope":true}`, "nope"},
	}
	for _, c := range cases {
		_, err := Parse([]byte(c.json))
		if err == nil {
			t.Errorf("%s: expected error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: error %q does not mention %q", c.name, err, c.wantErr)
		}
	}
}

func TestFederatedExample(t *testing.T) {
	// The federated multi-org config from fold's README.
	_, err := Parse([]byte(`{
	  "upstreams": [
	    {
	      "id": "github-tools",
	      "url": "https://mcp.platform.acme.com/mcp",
	      "namespace": "gh",
	      "owner": { "org": "acme-platform", "team": "devex" }
	    },
	    {
	      "id": "ml-search",
	      "url": "https://mcp.ml.acquired-co.com/mcp",
	      "namespace": "search",
	      "owner": { "org": "acquired-co", "team": "ml" },
	      "rateLimit": { "requestsPerMinute": 600 },
	      "circuitBreaker": { "failureThreshold": 5, "halfOpenAfterMs": 30000 }
	    }
	  ],
	  "server": { "rateLimit": { "requestsPerMinute": 6000 } }
	}`))
	if err != nil {
		t.Fatal(err)
	}
}
