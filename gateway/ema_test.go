package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/fold-run/fold/audit"
	"github.com/fold-run/fold/config"
)

const emaResource = "https://gw.example.com"

// setEMAKey generates an ES256 signing key and exposes it via the env var
// EMA's signingKeyRef resolves.
func setEMAKey(t *testing.T) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("EMA_TEST_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})))
}

// emaConfig builds an auth-required gateway config whose only issuer is the
// enterprise IdP in exchange mode — its ID-JAGs enter via /oauth/token only.
func emaConfig(idp *fixtureIssuer, upstreams []config.Upstream) *config.Config {
	return &config.Config{
		Upstreams: upstreams,
		Auth: &config.Auth{
			Mode:     "required",
			Resource: emaResource,
			Issuers:  []config.Issuer{{Issuer: idp.server.URL, Mode: "exchange"}},
			EMA: &config.EMAConfig{
				IdpIssuer:     idp.server.URL,
				SigningKeyRef: "EMA_TEST_KEY",
			},
		},
	}
}

// idJAG mints an ID-JAG from the fixture IdP: an assertion for the fold
// audience carrying sub, jti, and groups.
func idJAG(t *testing.T, idp *fixtureIssuer, sub, jti string) string {
	t.Helper()
	return idp.mintClaims(t, jwt.MapClaims{
		"sub":    sub,
		"aud":    emaResource,
		"jti":    jti,
		"exp":    time.Now().Add(5 * time.Minute).Unix(),
		"groups": []string{"eng"},
		"email":  sub + "@example.com",
	})
}

// redeem posts an ID-JAG at the token endpoint and returns the HTTP status
// and decoded response body.
func redeem(t *testing.T, gatewayURL, assertion string) (int, map[string]any) {
	t.Helper()
	form := url.Values{
		"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"},
		"assertion":  {assertion},
	}
	resp, err := http.Post(gatewayURL+"/oauth/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := map[string]any{}
	data, _ := io.ReadAll(resp.Body)
	json.Unmarshal(data, &body)
	return resp.StatusCode, body
}

// The full EMA loop: an enterprise-IdP ID-JAG is exchanged at /oauth/token
// for a fold-minted access token that works against /mcp — while the ID-JAG
// itself, presented directly, is rejected (exchange-mode issuers are not
// trusted for straight presentation).
func TestEMAExchange(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	idp := newFixtureIssuer(t)
	setEMAKey(t)
	ts, _ := startGateway(t, emaConfig(idp, []config.Upstream{{ID: "u", URL: up.URL}}))

	status, body := redeem(t, ts.URL, idJAG(t, idp, "alice", "jag-1"))
	if status != http.StatusOK {
		t.Fatalf("exchange failed: %d %v", status, body)
	}
	access, _ := body["access_token"].(string)
	if access == "" || body["token_type"] != "Bearer" {
		t.Fatalf("malformed token response: %v", body)
	}

	// The minted token authenticates against /mcp.
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + access})
	if _, err := session.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("minted token rejected by the gateway: %v", err)
	}

	// The raw ID-JAG must not work as a fold access token.
	req, _ := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+idJAG(t, idp, "alice", "jag-direct"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("ID-JAG presented directly should be 401, got %d", resp.StatusCode)
	}
}

// An ID-JAG is single-use: redeeming the same assertion twice fails.
func TestEMAReplayRejected(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	idp := newFixtureIssuer(t)
	setEMAKey(t)
	ts, _ := startGateway(t, emaConfig(idp, []config.Upstream{{ID: "u", URL: up.URL}}))

	assertion := idJAG(t, idp, "alice", "jag-replay")
	if status, body := redeem(t, ts.URL, assertion); status != http.StatusOK {
		t.Fatalf("first redemption failed: %d %v", status, body)
	}
	status, body := redeem(t, ts.URL, assertion)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("replay should be invalid_grant 400, got %d %v", status, body)
	}
}

// Malformed grants are rejected with OAuth-shaped errors.
func TestEMAGrantValidation(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	idp := newFixtureIssuer(t)
	setEMAKey(t)
	ts, _ := startGateway(t, emaConfig(idp, []config.Upstream{{ID: "u", URL: up.URL}}))

	cases := []struct {
		name      string
		assertion string
		wantErr   string
	}{
		{"missing jti", idp.mintClaims(t, jwt.MapClaims{
			"sub": "alice", "aud": emaResource, "exp": time.Now().Add(time.Minute).Unix(),
		}), "invalid_grant"},
		{"wrong audience", idp.mintClaims(t, jwt.MapClaims{
			"sub": "alice", "aud": "https://other.example.com", "jti": "j1", "exp": time.Now().Add(time.Minute).Unix(),
		}), "invalid_grant"},
		{"no expiry", idp.mintClaims(t, jwt.MapClaims{
			"sub": "alice", "aud": emaResource, "jti": "j2",
		}), "invalid_grant"},
		{"no subject", idp.mintClaims(t, jwt.MapClaims{
			"aud": emaResource, "jti": "j3", "exp": time.Now().Add(time.Minute).Unix(),
		}), "invalid_grant"},
	}
	for _, c := range cases {
		if status, body := redeem(t, ts.URL, c.assertion); status != http.StatusBadRequest || body["error"] != c.wantErr {
			t.Errorf("%s: got %d %v, want 400 %s", c.name, status, body, c.wantErr)
		}
	}

	// Wrong grant type.
	resp, err := http.Post(ts.URL+"/oauth/token", "application/x-www-form-urlencoded",
		strings.NewReader("grant_type=client_credentials"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unsupported grant type should be 400, got %d", resp.StatusCode)
	}
}

// The unauthenticated token endpoint is rate-limited (anti-amplification).
func TestEMATokenRateLimit(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	idp := newFixtureIssuer(t)
	setEMAKey(t)
	cfg := emaConfig(idp, []config.Upstream{{ID: "u", URL: up.URL}})
	cfg.Auth.EMA.TokenRateLimitPerMinute = 3
	ts, _ := startGateway(t, cfg)

	var last int
	var lastBody map[string]any
	for i := range 6 {
		last, lastBody = redeem(t, ts.URL, idJAG(t, idp, "alice", fmt.Sprintf("jag-rl-%d", i)))
	}
	if last != http.StatusTooManyRequests || lastBody["error"] != "slow_down" {
		t.Errorf("want 429 slow_down past the cap, got %d %v", last, lastBody)
	}
}

// Metadata advertises fold itself (not the exchange issuer) as the
// authorization server and announces the EMA extension; the JWKS endpoint
// serves the minting key.
func TestEMADiscovery(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	idp := newFixtureIssuer(t)
	setEMAKey(t)
	ts, _ := startGateway(t, emaConfig(idp, []config.Upstream{{ID: "u", URL: up.URL}}))

	get := func(path string) map[string]any {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		doc := map[string]any{}
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		return doc
	}

	meta := get("/.well-known/oauth-protected-resource")
	servers, _ := meta["authorization_servers"].([]any)
	if len(servers) != 1 || servers[0] != emaResource {
		t.Errorf("authorization_servers = %v, want [%s] (exchange issuer must not be advertised)", servers, emaResource)
	}
	if _, ok := meta["io.modelcontextprotocol/enterprise-managed-authorization"]; !ok {
		t.Errorf("EMA extension missing from metadata: %v", meta)
	}

	jwks := get("/.well-known/jwks.json")
	keys, _ := jwks["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("jwks = %v", jwks)
	}
	key, _ := keys[0].(map[string]any)
	if key["kty"] != "EC" || key["crv"] != "P-256" || key["kid"] == "" {
		t.Errorf("unexpected minting key document: %v", key)
	}
}

// TestEMAExchangeIsAudited: the token endpoint is an authorization server,
// and its terminal responses — the mint, the replay, the bad assertion —
// each leave exactly one audit record. The replay is the one that matters:
// before this, an attacker replaying ID-JAGs under the rate limit was
// invisible to the trail.
func TestEMAExchangeIsAudited(t *testing.T) {
	setEMAKey(t)
	idp := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	cfg := emaConfig(idp, []config.Upstream{{ID: "a", Namespace: "a", URL: up.URL}})
	auditPath := t.TempDir() + "/audit.jsonl"
	cfg.Audit = &config.Audit{Sinks: []config.AuditSink{{Type: "file", Path: auditPath}}}
	ts, _ := startGateway(t, cfg)

	assertion := idJAG(t, idp, "alice", "jag-audited")
	if status, body := redeem(t, ts.URL, assertion); status != http.StatusOK {
		t.Fatalf("first redeem: %d %v", status, body)
	}
	if status, _ := redeem(t, ts.URL, assertion); status != http.StatusBadRequest {
		t.Fatalf("replay should be refused, got %d", status)
	}
	if status, _ := redeem(t, ts.URL, "not-a-jwt"); status != http.StatusBadRequest {
		t.Fatalf("garbage assertion should be refused, got %d", status)
	}

	events := readAuditEvents(t, auditPath, "oauth/token")
	if len(events) != 3 {
		t.Fatalf("oauth/token audit events = %d, want 3: %+v", len(events), events)
	}
	mint, replay, garbage := events[0], events[1], events[2]
	if mint.Outcome != audit.OutcomeOK || mint.Decision != "minted" || mint.Principal != "alice" || mint.Issuer != idp.server.URL {
		t.Fatalf("mint event = %+v", mint)
	}
	// The replay is alertable on the structured decision field, not only by
	// substring-matching the error text.
	if replay.Outcome != audit.OutcomeUnauthenticated || replay.Decision != "replayed" || replay.Principal != "alice" {
		t.Fatalf("replay event = %+v", replay)
	}
	if garbage.Outcome != audit.OutcomeUnauthenticated || garbage.Decision != "invalid_grant" {
		t.Fatalf("garbage event = %+v", garbage)
	}
}
