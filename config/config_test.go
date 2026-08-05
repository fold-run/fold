package config

import (
	"fmt"
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

func TestMultiEndpointUpstream(t *testing.T) {
	cfg, err := Parse([]byte(`{"upstreams":[{"id":"a","urls":["http://x.test","http://y.test"]}]}`))
	if err != nil {
		t.Fatalf("urls config rejected: %v", err)
	}
	if got := cfg.Upstreams[0].Endpoints(); len(got) != 2 || got[0] != "http://x.test" {
		t.Errorf("Endpoints() = %v", got)
	}
	single := Upstream{ID: "a", URL: "http://x.test"}
	if got := single.Endpoints(); len(got) != 1 || got[0] != "http://x.test" {
		t.Errorf("single-url Endpoints() = %v", got)
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
		{"no url at all", `{"upstreams":[{"id":"a"}]}`, "url (or urls) is required"},
		{"url and urls", `{"upstreams":[{"id":"a","url":"http://x.test","urls":["http://y.test"]}]}`, "not both"},
		{"bad urls entry", `{"upstreams":[{"id":"a","urls":["http://x.test","nope"]}]}`, "http(s)"},
		{"duplicate urls entry", `{"upstreams":[{"id":"a","urls":["http://x.test","http://x.test"]}]}`, "duplicate endpoint"},
		{"bad health interval", `{"upstreams":[{"id":"a","url":"http://x.test","healthCheck":{"intervalMs":0}}]}`, "healthCheck.intervalMs"},
		{"tracing without endpoint", `{"upstreams":[{"id":"a","url":"http://x.test"}],"tracing":{}}`, "otlpEndpoint"},
		{"discovery without url", `{"upstreams":[{"id":"a","url":"http://x.test"}],"discovery":{}}`, "discovery url"},
		{"discovery cleartext url", `{"upstreams":[{"id":"a","url":"http://x.test"}],"discovery":{"url":"http://registry.internal/doc"}}`, "https"},
		{"discovery negative interval", `{"upstreams":[{"id":"a","url":"http://x.test"}],"discovery":{"url":"https://r.test/doc","intervalMs":-1}}`, "intervalMs"},
		{"discovery bad strategy", `{"upstreams":[{"id":"a","url":"http://x.test"}],"discovery":{"url":"https://r.test/doc","allowedAuthStrategies":["nope"]}}`, "unknown strategy"},
		{"discovery empty secret ref", `{"upstreams":[{"id":"a","url":"http://x.test"}],"discovery":{"url":"https://r.test/doc","allowedSecretRefs":[""]}}`, "must not be empty"},
		{"tracing bad ratio", `{"upstreams":[{"id":"a","url":"http://x.test"}],"tracing":{"otlpEndpoint":"http://c.test:4318","sampleRatio":1.5}}`, "sampleRatio"},
		{"auth without resource", `{"upstreams":[{"id":"a","url":"http://x.test"}],"auth":{"mode":"required","issuers":[{"issuer":"https://idp.test"}]}}`, "resource"},
		{"auth without issuers", `{"upstreams":[{"id":"a","url":"http://x.test"}],"auth":{"mode":"required","resource":"https://gw.test"}}`, "issuer"},
		{"static without secret", `{"upstreams":[{"id":"a","url":"http://x.test","auth":{"strategy":"static"}}]}`, "secretRef"},
		{"exchange without audience", `{"upstreams":[{"id":"a","url":"http://x.test","auth":{"strategy":"token-exchange","tokenEndpoint":"https://t.test","clientId":"c","clientAuth":{"type":"client_secret_post","secretRef":"S"}}}]}`, "audience"},
		{"policy unknown server", `{"upstreams":[{"id":"a","url":"http://x.test"}],"policy":{"rules":[{"id":"r","allow":[{"server":"nope"}]}]}}`, "unknown server"},
		{"claims object value", `{"upstreams":[{"id":"a","url":"http://x.test"}],"policy":{"rules":[{"id":"r","subjects":{"claims":{"dept":{"nested":true}}},"allow":[{"server":"a"}]}]}}`, "JSON scalar"},
		{"claims array value", `{"upstreams":[{"id":"a","url":"http://x.test"}],"policy":{"rules":[{"id":"r","subjects":{"claims":{"dept":["eng"]}},"allow":[{"server":"a"}]}]}}`, "JSON scalar"},
		{"webhook without url", `{"upstreams":[{"id":"a","url":"http://x.test"}],"audit":{"sinks":[{"type":"webhook"}]}}`, "url"},
		{"unknown field", `{"upstreams":[{"id":"a","url":"http://x.test"}],"nope":true}`, "nope"},
		{"passthrough without auth", `{"upstreams":[{"id":"a","url":"http://x.test","auth":{"strategy":"passthrough"}}]}`, `auth.mode "required"`},
		{"exchange without auth", `{"upstreams":[{"id":"a","url":"http://x.test","auth":{"strategy":"token-exchange","tokenEndpoint":"https://t.test","clientId":"c","clientAuth":{"type":"client_secret_post","secretRef":"S"},"audience":"aud"}}]}`, `auth.mode "required"`},
		{"negative body cap", `{"upstreams":[{"id":"a","url":"http://x.test"}],"server":{"maxBodyBytes":-1}}`, "maxBodyBytes"},
		{"bad issuer mode", `{"upstreams":[{"id":"a","url":"http://x.test"}],"auth":{"mode":"required","resource":"https://gw.test","issuers":[{"issuer":"https://idp.test","mode":"nope"}]}}`, "direct"},
		{"ema without required auth", `{"upstreams":[{"id":"a","url":"http://x.test"}],"auth":{"mode":"disabled","ema":{"idpIssuer":"https://idp.test","signingKeyRef":"K"}}}`, `mode "required"`},
		{"ema without signing key", `{"upstreams":[{"id":"a","url":"http://x.test"}],"auth":{"mode":"required","resource":"https://gw.test","issuers":[{"issuer":"https://idp.test"}],"ema":{"idpIssuer":"https://idp.test"}}}`, "signingKeyRef"},
		{"ema http idp", `{"upstreams":[{"id":"a","url":"http://x.test"}],"auth":{"mode":"required","resource":"https://gw.test","issuers":[{"issuer":"https://idp.test"}],"ema":{"idpIssuer":"http://idp.evil","signingKeyRef":"K"}}}`, "https"},
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

func TestRequireHTTPSForSecurityEndpoints(t *testing.T) {
	cases := []struct{ name, json, wantErr string }{
		{"http token endpoint", `{"upstreams":[{"id":"a","url":"https://x.test/mcp","auth":{"strategy":"token-exchange","tokenEndpoint":"http://idp.evil/token","clientId":"c","clientAuth":{"type":"client_secret_post","secretRef":"S"},"audience":"aud"}}]}`, "https"},
		{"http issuer", `{"upstreams":[{"id":"a","url":"https://x.test/mcp"}],"auth":{"mode":"required","resource":"https://gw.test","issuers":[{"issuer":"http://idp.evil"}]}}`, "https"},
		{"http jwksUri", `{"upstreams":[{"id":"a","url":"https://x.test/mcp"}],"auth":{"mode":"required","resource":"https://gw.test","issuers":[{"issuer":"https://idp.test","jwksUri":"http://idp.evil/keys"}]}}`, "https"},
	}
	for _, c := range cases {
		if _, err := Parse([]byte(c.json)); err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: got %v, want error mentioning %q", c.name, err, c.wantErr)
		}
	}
	// Loopback token endpoints are exempt for local development.
	if _, err := Parse([]byte(`{"upstreams":[{"id":"a","url":"https://x.test/mcp","auth":{"strategy":"client-credentials","tokenEndpoint":"http://localhost:9000/token","clientId":"c","clientAuth":{"type":"client_secret_post","secretRef":"S"}}}]}`)); err != nil {
		t.Errorf("loopback token endpoint should be allowed: %v", err)
	}
}

// TestDiscoveryCredentialPairing: permitting credentials without bounding
// their destination is the exfiltration path the allowlists exist to close,
// so validation refuses the half-configured combination.
func TestDiscoveryCredentialPairing(t *testing.T) {
	base := `{"upstreams":[{"id":"a","url":"http://x.test"}],"discovery":{"url":"https://r.test/doc"%s}}`
	if _, err := Parse([]byte(fmt.Sprintf(base, `,"allowedAuthStrategies":["static"]`))); err == nil {
		t.Error("strategies without credential hosts should be rejected")
	}
	if _, err := Parse([]byte(fmt.Sprintf(base, `,"allowedSecretRefs":["K"]`))); err == nil {
		t.Error("secret refs without credential hosts should be rejected")
	}
	// Paired is fine; so is neither.
	if _, err := Parse([]byte(fmt.Sprintf(base, `,"allowedAuthStrategies":["static"],"allowedSecretRefs":["K"],"allowedCredentialHosts":["*.svc.cluster.local"]`))); err != nil {
		t.Errorf("paired config rejected: %v", err)
	}
	if _, err := Parse([]byte(fmt.Sprintf(base, ``))); err != nil {
		t.Errorf("bare discovery rejected: %v", err)
	}
	// Only "none" does not count as permitting credentials.
	if _, err := Parse([]byte(fmt.Sprintf(base, `,"allowedAuthStrategies":["none"]`))); err != nil {
		t.Errorf("none-only strategies rejected: %v", err)
	}
}

func TestHostAllowed(t *testing.T) {
	pats := []string{"exact.internal", "*.svc.cluster.local"}
	for _, c := range []struct {
		host string
		want bool
	}{
		{"exact.internal", true},
		{"exact.internal:8080", true},
		{"mcp.svc.cluster.local", true},
		{"svc.cluster.local", false}, // apex is not granted by "*."
		{"evil.example", false},
		{"exact.internal.evil.example", false},
		{"Kxact.internal", false}, // Kelvin sign folds to "k" in Unicode
		{"", false},
	} {
		if got := HostAllowed(pats, c.host); got != c.want {
			t.Errorf("HostAllowed(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
