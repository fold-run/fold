package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fold-run/fold/config"
)

// fixtureIssuer is a minimal identity provider: an RSA key served as JWKS,
// and a token minter.
type fixtureIssuer struct {
	key    *rsa.PrivateKey
	server *httptest.Server
}

func newFixtureIssuer(t *testing.T) *fixtureIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &fixtureIssuer{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jwks.json", func(w http.ResponseWriter, r *http.Request) {
		b64 := base64.RawURLEncoding.EncodeToString
		json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]any{{
				"kty": "RSA", "kid": "k1", "use": "sig",
				"n": b64(key.N.Bytes()),
				"e": b64(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	})
	iss.server = httptest.NewServer(mux)
	t.Cleanup(iss.server.Close)
	return iss
}

func (f *fixtureIssuer) mint(t *testing.T, sub, aud string, groups []string) string {
	t.Helper()
	return f.mintClaims(t, jwt.MapClaims{
		"sub":    sub,
		"aud":    aud,
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": groups,
	})
}

// mintClaims signs an arbitrary claim set (iss/iat filled in when absent),
// so tests can mint ID-JAGs and negative-case tokens.
func (f *fixtureIssuer) mintClaims(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	if _, ok := claims["iss"]; !ok {
		claims["iss"] = f.server.URL
	}
	if _, ok := claims["iat"]; !ok {
		claims["iat"] = time.Now().Unix()
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = "k1"
	signed, err := tok.SignedString(f.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func authedConfig(iss *fixtureIssuer, upstreams []config.Upstream, pol *config.Policy) *config.Config {
	return &config.Config{
		Upstreams: upstreams,
		Auth: &config.Auth{
			Mode:     "required",
			Resource: "https://gw.example.com",
			Issuers:  []config.Issuer{{Issuer: iss.server.URL}},
		},
		Policy: pol,
	}
}

func TestAuthRequired(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, authedConfig(iss, []config.Upstream{{ID: "u", URL: up.URL}}, nil))

	// No token → 401 with a WWW-Authenticate challenge.
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.Contains(got, "resource_metadata") {
		t.Errorf("WWW-Authenticate = %q, want resource_metadata challenge", got)
	}

	// Wrong audience → 401.
	bad := iss.mint(t, "mallory", "https://other.example.com", nil)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+bad)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("want 401 for wrong audience, got %d", resp.StatusCode)
	}

	// Valid token → full MCP session works.
	good := iss.mint(t, "alice", "https://gw.example.com", nil)
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + good})
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil {
		t.Errorf("authenticated CallTool: %v", err)
	}

	// Protected resource metadata is published (RFC 9728).
	resp, err = http.Get(ts.URL + "/.well-known/oauth-protected-resource")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var meta struct {
		Resource string `json:"resource"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if meta.Resource != "https://gw.example.com" {
		t.Errorf("resource metadata = %+v", meta)
	}
}

func TestPerPrincipalPolicy(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "get_thing", "delete_thing")
	ts, _ := startGateway(t, authedConfig(iss,
		[]config.Upstream{{ID: "things", URL: up.URL, Namespace: "things"}},
		&config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID:       "eng-all",
				Subjects: &config.PolicySubjects{Groups: []string{"engineering"}},
				Allow:    []config.PolicyAllow{{Server: "things"}},
			}, {
				ID:       "support-read",
				Subjects: &config.PolicySubjects{Groups: []string{"support"}},
				Allow:    []config.PolicyAllow{{Server: "things", Names: []string{"get_*"}}},
			}},
		}))

	// Engineering sees and calls everything.
	eng := connect(t, ts.URL, map[string]string{
		"Authorization": "Bearer " + iss.mint(t, "alice", "https://gw.example.com", []string{"engineering"}),
	})
	res, err := eng.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 2 {
		t.Errorf("engineering sees %d tools, want 2", len(res.Tools))
	}

	// Support sees only get_*; delete is invisible and denied.
	sup := connect(t, ts.URL, map[string]string{
		"Authorization": "Bearer " + iss.mint(t, "bob", "https://gw.example.com", []string{"support"}),
	})
	res, err = sup.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(toolNames(res), ","); got != "things__get_thing" {
		t.Errorf("support list = %q, want things__get_thing", got)
	}
	if _, err := sup.CallTool(context.Background(), &mcp.CallToolParams{Name: "things__delete_thing"}); err == nil {
		t.Error("expected denial for support calling delete_thing")
	}
}

func TestPassthroughCredentials(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, lastAuth := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, authedConfig(iss,
		[]config.Upstream{{ID: "u", URL: up.URL, Auth: &config.UpstreamAuth{Strategy: "passthrough"}, CacheTTLMs: -1}},
		nil))

	token := iss.mint(t, "carol", "https://gw.example.com", nil)
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + token})
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if got := lastHeader(lastAuth, "Authorization"); got != "Bearer "+token {
		t.Errorf("upstream did not receive the caller's token (got %q)", truncate(got, 40))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func TestRejectsTokenWithoutSubject(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, authedConfig(iss, []config.Upstream{{ID: "u", URL: up.URL}}, nil))

	// Mint a token with an empty subject — must be rejected (empty UserID
	// would disable the SDK's per-session identity binding).
	noSub := iss.mint(t, "", "https://gw.example.com", nil)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+noSub)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("token without sub should be rejected, got %d", resp.StatusCode)
	}
}

// One principal flooding its own per-principal budget is 429'd without
// consuming any other principal's — the global bucket alone let a single
// tenant starve every other tenant.
func TestPerPrincipalRateLimit(t *testing.T) {
	up, _ := newUpstreamServer(t, "tool")
	iss := newFixtureIssuer(t)
	cfg := authedConfig(iss, []config.Upstream{{ID: "u", URL: up.URL}}, nil)
	cfg.Server = &config.ServerSection{RateLimit: &config.RateLimit{PerPrincipalPerMinute: 3}}
	ts, _ := startGateway(t, cfg)

	post := func(token string) int {
		req, err := http.NewRequest("POST", ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	aliceTok := iss.mint(t, "alice", "https://gw.example.com", nil)
	bobTok := iss.mint(t, "bob", "https://gw.example.com", nil)

	last := 0
	for range 6 {
		last = post(aliceTok)
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("flooding principal should be 429'd, got %d", last)
	}
	if got := post(bobTok); got == http.StatusTooManyRequests {
		t.Error("another principal's flood starved an unrelated caller")
	}
}

// TestClaimsBasedPolicy: ABAC end to end — a rule gated on a token claim
// admits only callers whose verified JWT carries it, and list filtering
// follows the same decision.
func TestClaimsBasedPolicy(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "get_thing", "delete_thing")
	ts, _ := startGateway(t, authedConfig(iss,
		[]config.Upstream{{ID: "things", URL: up.URL, Namespace: "things"}},
		&config.Policy{
			DefaultDecision: "deny",
			Rules: []config.PolicyRule{{
				ID: "eng-dept-mfa",
				Subjects: &config.PolicySubjects{
					Claims: map[string]any{"dept": "eng", "mfa": true},
				},
				Allow: []config.PolicyAllow{{Server: "things"}},
			}},
		}))

	mint := func(sub string, extra jwt.MapClaims) string {
		claims := jwt.MapClaims{
			"sub": sub,
			"aud": "https://gw.example.com",
			"exp": time.Now().Add(time.Hour).Unix(),
		}
		for k, v := range extra {
			claims[k] = v
		}
		return iss.mintClaims(t, claims)
	}

	// Both required claims present → full access.
	eng := connect(t, ts.URL, map[string]string{
		"Authorization": "Bearer " + mint("alice", jwt.MapClaims{"dept": "eng", "mfa": true}),
	})
	res, err := eng.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 2 {
		t.Errorf("eng+mfa sees %d tools, want 2", len(res.Tools))
	}
	if _, err := eng.CallTool(context.Background(), &mcp.CallToolParams{Name: "things__get_thing"}); err != nil {
		t.Errorf("eng+mfa CallTool: %v", err)
	}

	// Right department, no MFA claim → invisible and denied.
	noMFA := connect(t, ts.URL, map[string]string{
		"Authorization": "Bearer " + mint("bob", jwt.MapClaims{"dept": "eng"}),
	})
	res, err = noMFA.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 0 {
		t.Errorf("missing-claim caller sees %d tools, want 0", len(res.Tools))
	}
	if _, err := noMFA.CallTool(context.Background(), &mcp.CallToolParams{Name: "things__get_thing"}); err == nil {
		t.Error("expected denial for caller missing the mfa claim")
	} else if !strings.Contains(err.Error(), "policy denied") {
		t.Errorf("unexpected error: %v", err)
	}

	// An array-valued claim matches by membership.
	multi := connect(t, ts.URL, map[string]string{
		"Authorization": "Bearer " + mint("carol", jwt.MapClaims{"dept": []string{"sales", "eng"}, "mfa": true}),
	})
	if _, err := multi.CallTool(context.Background(), &mcp.CallToolParams{Name: "things__get_thing"}); err != nil {
		t.Errorf("array-claim caller CallTool: %v", err)
	}
}

// The inbound trust anchor reports its fetches. Before this the IdP had no
// telemetry of its own: an outage there failed every authenticated request
// and showed up only as unauthenticated rejections, indistinguishable from a
// wave of clients presenting bad tokens.
func TestJWKSFetchesAreCounted(t *testing.T) {
	iss := newFixtureIssuer(t)
	up, _ := newUpstreamServer(t, "tool")
	ts, _ := startGateway(t, authedConfig(iss, []config.Upstream{{ID: "u", URL: up.URL}}, nil))

	token := iss.mint(t, "carol", "https://gw.example.com", nil)
	session := connect(t, ts.URL, map[string]string{"Authorization": "Bearer " + token})
	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "tool"}); err != nil {
		t.Fatalf("CallTool: %v", err)
	}

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	want := `fold_jwks_fetches_total{issuer="` + iss.server.URL + `",outcome="ok"} 1`
	if !strings.Contains(string(body), want) {
		t.Fatalf("metrics missing %q", want)
	}
}
