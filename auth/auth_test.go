package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/fold-run/fold/config"
)

const resource = "https://gw.example.com/mcp"

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func signToken(t *testing.T, key *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	raw, err := tok.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func baseClaims(issuer string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": issuer,
		"sub": "user-1",
		"aud": resource,
		"exp": time.Now().Add(time.Hour).Unix(),
	}
}

func TestPrincipalContextRoundTrip(t *testing.T) {
	if got := PrincipalFromContext(context.Background()); got != nil {
		t.Errorf("empty context yielded principal %v", got)
	}
	p := &Principal{Subject: "user-1", Issuer: "https://idp.example.com"}
	got := PrincipalFromContext(WithPrincipal(context.Background(), p))
	if got != p {
		t.Errorf("round trip lost the principal: %v", got)
	}
}

func TestVerifyLocalKey(t *testing.T) {
	const issuer = "https://fold.example.com"
	key := newKey(t)
	v := NewVerifier(&config.Auth{Resource: resource}, http.DefaultClient)
	v.TrustLocal(issuer, &key.PublicKey)

	claims := baseClaims(issuer)
	claims["groups"] = []any{"eng", "sre", 42} // non-strings are skipped
	p, err := v.Verify(context.Background(), signToken(t, key, "", claims))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if p.Subject != "user-1" || p.Issuer != issuer {
		t.Errorf("principal = %+v", p)
	}
	if len(p.Groups) != 2 || p.Groups[0] != "eng" || p.Groups[1] != "sre" {
		t.Errorf("groups = %v, want [eng sre]", p.Groups)
	}
	if p.Expiry.IsZero() {
		t.Error("expiry not captured")
	}
}

func TestVerifyRejections(t *testing.T) {
	const issuer = "https://idp.example.com"
	key := newKey(t)
	v := NewVerifier(&config.Auth{
		Resource: resource,
		Issuers: []config.Issuer{
			{Issuer: issuer},
			{Issuer: "https://jag.example.com", Mode: "exchange"},
		},
	}, http.DefaultClient)
	v.TrustLocal(issuer, &key.PublicKey) // reuse local-key path; JWKS is tested separately

	expired := baseClaims(issuer)
	expired["exp"] = time.Now().Add(-time.Hour).Unix()
	noExp := baseClaims(issuer)
	delete(noExp, "exp")
	wrongAud := baseClaims(issuer)
	wrongAud["aud"] = "https://other.example.com"
	noSub := baseClaims(issuer)
	delete(noSub, "sub")
	otherKey := newKey(t)

	cases := map[string]string{
		"malformed":            "not-a-jwt",
		"untrusted issuer":     signToken(t, key, "", baseClaims("https://rogue.example.com")),
		"exchange-mode issuer": signToken(t, key, "", baseClaims("https://jag.example.com")),
		"expired":              signToken(t, key, "", expired),
		"missing exp":          signToken(t, key, "", noExp),
		"wrong audience":       signToken(t, key, "", wrongAud),
		"missing sub":          signToken(t, key, "", noSub),
		"wrong signing key":    signToken(t, otherKey, "", baseClaims(issuer)),
	}
	for name, raw := range cases {
		if _, err := v.Verify(context.Background(), raw); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}

	// Symmetric algorithms are categorically rejected — an HS256 token
	// "signed" with public material must never verify.
	hs := jwt.NewWithClaims(jwt.SigningMethodHS256, baseClaims(issuer))
	raw, err := hs.SignedString([]byte("guessable"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), raw); err == nil {
		t.Error("HS256 token accepted")
	}
}

// TestVerifyViaJWKS exercises the network path: kid lookup against a served
// key set, including refetch-on-unknown-kid (key rotation).
func TestVerifyViaJWKS(t *testing.T) {
	const issuer = "https://idp.example.com"
	key := newKey(t)

	jwks := func(kid string, pub *ecdsa.PublicKey) []byte {
		raw, err := pub.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		b64 := base64.RawURLEncoding.EncodeToString
		doc, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "kid": kid, "use": "sig",
			"x": b64(raw[1:33]), "y": b64(raw[33:65]),
		}}})
		return doc
	}
	served := jwks("k1", &key.PublicKey)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(served)
	}))
	t.Cleanup(srv.Close)

	v := NewVerifier(&config.Auth{
		Resource: resource,
		Issuers:  []config.Issuer{{Issuer: issuer, JWKSURI: srv.URL}},
	}, http.DefaultClient)

	if _, err := v.Verify(context.Background(), signToken(t, key, "k1", baseClaims(issuer))); err != nil {
		t.Fatalf("JWKS-backed verify: %v", err)
	}
	if _, err := v.Verify(context.Background(), signToken(t, key, "unknown-kid", baseClaims(issuer))); err == nil {
		t.Error("token with unknown kid accepted")
	}

	// Rotation: a new key under a new kid appears in the served set. Within
	// 30s of the last fetch the cache refuses to refetch (unknown-kid flood
	// protection), so the token is rejected...
	key2 := newKey(t)
	served = jwks("k2", &key2.PublicKey)
	rotated := signToken(t, key2, "k2", baseClaims(issuer))
	if _, err := v.Verify(context.Background(), rotated); err == nil {
		t.Error("unknown kid within the refetch window must not trigger a fetch")
	}
	// ...and once the holdoff has passed, the same token verifies.
	v.jwks.now = func() time.Time { return time.Now().Add(time.Minute) }
	if _, err := v.Verify(context.Background(), rotated); err != nil {
		t.Fatalf("verify after rotation with stale cache: %v", err)
	}
}

// jwksFixture serves a mutable key set and counts fetches.
type jwksFixture struct {
	t      *testing.T
	srv    *httptest.Server
	doc    atomic.Pointer[[]byte]
	fail   atomic.Bool
	hits   atomic.Int64
	issuer string
}

func newJWKSFixture(t *testing.T, issuer string) *jwksFixture {
	t.Helper()
	f := &jwksFixture{t: t, issuer: issuer}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.fail.Load() {
			http.Error(w, "idp down", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(*f.doc.Load())
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *jwksFixture) serve(kid string, pub *ecdsa.PublicKey) {
	raw, err := pub.Bytes()
	if err != nil {
		f.t.Fatal(err)
	}
	b64 := base64.RawURLEncoding.EncodeToString
	doc, _ := json.Marshal(map[string]any{"keys": []map[string]string{{
		"kty": "EC", "crv": "P-256", "kid": kid, "use": "sig",
		"x": b64(raw[1:33]), "y": b64(raw[33:65]),
	}}})
	f.doc.Store(&doc)
}

// outcomes collects what the JWKS observer reports.
type outcomes struct {
	mu sync.Mutex
	n  map[string]int
}

func (o *outcomes) observe(issuer, outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.n == nil {
		o.n = map[string]int{}
	}
	o.n[issuer+" "+outcome]++
}

func (o *outcomes) get(issuer, outcome string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n[issuer+" "+outcome]
}

// Rotation-out. A key the IdP has withdrawn from its set must stop verifying
// tokens once the cached set reaches its ttl — before this, a known kid never
// prompted a refetch, so a revoked key stayed trusted for the life of the
// process. The refresh also has to happen without the token presenting an
// unknown kid, which is what makes it a ttl and not the rotation-in path.
func TestJWKSRefreshesOnTTLSoRevokedKeysStopVerifying(t *testing.T) {
	const issuer = "https://idp.example.com"
	f := newJWKSFixture(t, issuer)
	k1, k2 := newKey(t), newKey(t)
	f.serve("k1", &k1.PublicKey)

	var seen outcomes
	v := NewVerifier(&config.Auth{
		Resource: resource,
		Issuers:  []config.Issuer{{Issuer: issuer, JWKSURI: f.srv.URL}},
	}, nil)
	v.SetJWKSObserver(seen.observe)
	clock := time.Now()
	v.jwks.now = func() time.Time { return clock }

	tok1 := signToken(t, k1, "k1", baseClaims(issuer))
	if _, err := v.Verify(context.Background(), tok1); err != nil {
		t.Fatalf("first verify: %v", err)
	}
	// Within the ttl the cached set answers: no fetch, however many calls.
	for range 5 {
		if _, err := v.Verify(context.Background(), tok1); err != nil {
			t.Fatalf("cached verify: %v", err)
		}
	}
	if f.hits.Load() != 1 {
		t.Fatalf("%d fetches for a known kid inside the ttl, want 1", f.hits.Load())
	}

	// The IdP revokes k1 and publishes k2. Nothing about the tokens changes.
	f.serve("k2", &k2.PublicKey)
	clock = clock.Add(6 * time.Minute)
	if _, err := v.Verify(context.Background(), tok1); err == nil {
		t.Fatal("token signed by a revoked key verified after the ttl elapsed")
	}
	if _, err := v.Verify(context.Background(), signToken(t, k2, "k2", baseClaims(issuer))); err != nil {
		t.Fatalf("token under the rotated key: %v", err)
	}
	if got := seen.get(issuer, "ok"); got != 2 {
		t.Fatalf("observer saw %d ok fetches, want 2 (initial + ttl refresh)", got)
	}
}

// An IdP outage is not a reason to reject every caller holding a valid token.
// When the refresh fails, the last good set keeps verifying and the failure is
// reported as "stale" — distinct from "error", which means nothing is cached
// and verification really is failing. And a failing IdP is not hammered: one
// attempt per holdoff, not one per request.
func TestJWKSServesStaleWhenTheIdPIsDown(t *testing.T) {
	const issuer = "https://idp.example.com"
	f := newJWKSFixture(t, issuer)
	k1 := newKey(t)
	f.serve("k1", &k1.PublicKey)

	var seen outcomes
	v := NewVerifier(&config.Auth{
		Resource: resource,
		Issuers:  []config.Issuer{{Issuer: issuer, JWKSURI: f.srv.URL}},
	}, nil)
	v.SetJWKSObserver(seen.observe)
	clock := time.Now()
	v.jwks.now = func() time.Time { return clock }

	tok := signToken(t, k1, "k1", baseClaims(issuer))
	if _, err := v.Verify(context.Background(), tok); err != nil {
		t.Fatalf("first verify: %v", err)
	}

	f.fail.Store(true)
	clock = clock.Add(6 * time.Minute)
	for range 10 {
		if _, err := v.Verify(context.Background(), tok); err != nil {
			t.Fatalf("verify during IdP outage: %v — the last good set must keep serving", err)
		}
	}
	if got := f.hits.Load(); got != 2 {
		t.Fatalf("%d fetches during the outage, want exactly 1 (the holdoff bounds retries)", got-1)
	}
	if got := seen.get(issuer, "stale"); got != 1 {
		t.Fatalf("observer saw stale=%d, want 1", got)
	}
	if got := seen.get(issuer, "error"); got != 0 {
		t.Fatalf("observer saw error=%d for an outage with a cached set; that outcome means nothing is cached", got)
	}

	// A cold cache against a down IdP is the hard failure, and says so.
	cold := NewVerifier(&config.Auth{
		Resource: resource,
		Issuers:  []config.Issuer{{Issuer: issuer, JWKSURI: f.srv.URL}},
	}, nil)
	cold.SetJWKSObserver(seen.observe)
	if _, err := cold.Verify(context.Background(), tok); err == nil {
		t.Fatal("cold cache verified a token with the IdP down")
	}
	if got := seen.get(issuer, "error"); got != 1 {
		t.Fatalf("observer saw error=%d after a cold-cache fetch failure, want 1", got)
	}
}

// A nil client must not fall back to http.DefaultClient: the token fetch is
// single-flighted per key, so one wedged token endpoint with no client
// timeout would hold every caller behind it for the full request timeout.
func TestUpstreamCredentialsNeverUseTheDefaultClient(t *testing.T) {
	c := NewUpstreamCredentials(&config.UpstreamAuth{Strategy: "static", SecretRef: "X"}, nil)
	if c.client.Timeout == 0 {
		t.Fatal("token-endpoint client has no timeout")
	}
	if c.client == http.DefaultClient {
		t.Fatal("token-endpoint client is http.DefaultClient")
	}
}

func TestApplyStrategies(t *testing.T) {
	ctx := context.Background()
	authed := WithPrincipal(ctx, &Principal{
		Subject: "user-1", Issuer: "https://idp.example.com", Token: "caller-token",
	})

	t.Run("nil and none set nothing", func(t *testing.T) {
		hdr := http.Header{}
		if err := (*UpstreamCredentials)(nil).Apply(ctx, hdr); err != nil || len(hdr) != 0 {
			t.Errorf("nil creds: err=%v hdr=%v", err, hdr)
		}
		c := NewUpstreamCredentials(&config.UpstreamAuth{Strategy: "none"}, nil)
		if err := c.Apply(ctx, hdr); err != nil || len(hdr) != 0 {
			t.Errorf("none: err=%v hdr=%v", err, hdr)
		}
	})

	t.Run("static", func(t *testing.T) {
		c := NewUpstreamCredentials(&config.UpstreamAuth{Strategy: "static", SecretRef: "FOLD_TEST_SECRET"}, nil)
		if err := c.Apply(ctx, http.Header{}); err == nil {
			t.Error("missing env secret must error")
		}
		t.Setenv("FOLD_TEST_SECRET", "s3cret")
		hdr := http.Header{}
		if err := c.Apply(ctx, hdr); err != nil {
			t.Fatal(err)
		}
		if got := hdr.Get("Authorization"); got != "Bearer s3cret" {
			t.Errorf("default header = %q", got)
		}
		custom := NewUpstreamCredentials(&config.UpstreamAuth{
			Strategy: "static", SecretRef: "FOLD_TEST_SECRET", Header: "X-Api-Key",
		}, nil)
		hdr = http.Header{}
		if err := custom.Apply(ctx, hdr); err != nil {
			t.Fatal(err)
		}
		if got := hdr.Get("X-Api-Key"); got != "s3cret" {
			t.Errorf("custom header = %q (no scheme expected)", got)
		}
	})

	t.Run("passthrough", func(t *testing.T) {
		c := NewUpstreamCredentials(&config.UpstreamAuth{Strategy: "passthrough"}, nil)
		if err := c.Apply(ctx, http.Header{}); err == nil {
			t.Error("anonymous passthrough must error")
		}
		hdr := http.Header{}
		if err := c.Apply(authed, hdr); err != nil {
			t.Fatal(err)
		}
		if got := hdr.Get("Authorization"); got != "Bearer caller-token" {
			t.Errorf("passthrough header = %q", got)
		}
	})

	t.Run("token-exchange requires identity", func(t *testing.T) {
		c := NewUpstreamCredentials(&config.UpstreamAuth{Strategy: "token-exchange"}, nil)
		if err := c.Apply(ctx, http.Header{}); err == nil {
			t.Error("anonymous token-exchange must error")
		}
		noSub := WithPrincipal(ctx, &Principal{Issuer: "https://idp.example.com", Token: "t"})
		if err := c.Apply(noSub, http.Header{}); err == nil {
			t.Error("subject-less token-exchange must error")
		}
	})

	t.Run("unknown strategy", func(t *testing.T) {
		c := NewUpstreamCredentials(&config.UpstreamAuth{Strategy: "quantum"}, nil)
		if err := c.Apply(ctx, http.Header{}); err == nil {
			t.Error("unknown strategy must error")
		}
	})
}

// TestClientCredentialsCachesToken proves the token endpoint is hit once
// while the token is fresh, and per-request headers carry the fetched token.
func TestClientCredentialsCachesToken(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if got := r.FormValue("grant_type"); got != "client_credentials" {
			t.Errorf("grant_type = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "upstream-token", "token_type": "Bearer", "expires_in": 3600,
		})
	}))
	t.Cleanup(srv.Close)

	t.Setenv("FOLD_TEST_CLIENT_SECRET", "cc-secret")
	c := NewUpstreamCredentials(&config.UpstreamAuth{
		Strategy:      "client-credentials",
		TokenEndpoint: srv.URL,
		ClientID:      "fold",
		ClientAuth:    &config.ClientAuth{Type: "client_secret_post", SecretRef: "FOLD_TEST_CLIENT_SECRET"},
	}, srv.Client())

	for range 3 {
		hdr := http.Header{}
		if err := c.Apply(context.Background(), hdr); err != nil {
			t.Fatal(err)
		}
		if got := hdr.Get("Authorization"); got != "Bearer upstream-token" {
			t.Errorf("Authorization = %q", got)
		}
	}
	if hits != 1 {
		t.Errorf("token endpoint hit %d times for 3 requests, want 1 (cached)", hits)
	}
}

// TestTokenEndpointRefusesRedirects: a token endpoint that redirects must
// not carry the request onward. Go replays POST bodies on 307/308, so
// following one would hand the client secret — and, under token-exchange,
// the caller's own bearer token — to whatever host the redirect names.
func TestTokenEndpointRefusesRedirects(t *testing.T) {
	var attackerSaw atomic.Value
	attackerSaw.Store("")
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		attackerSaw.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"attacker-minted","expires_in":3600}`)
	}))
	defer attacker.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/t", http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()

	t.Setenv("CC_SECRET", "super-secret-client-secret")
	for _, strategy := range []string{"client-credentials", "token-exchange"} {
		attackerSaw.Store("")
		cfg := &config.UpstreamAuth{
			Strategy:      strategy,
			TokenEndpoint: redirector.URL,
			ClientID:      "c",
			ClientAuth:    &config.ClientAuth{Type: "client_secret_post", SecretRef: "CC_SECRET"},
			Audience:      "https://upstream.example",
		}
		creds := NewUpstreamCredentials(cfg, nil)
		ctx := WithPrincipal(context.Background(), &Principal{
			Subject: "alice", Issuer: "https://idp.example", Token: "CALLER-BEARER-TOKEN",
		})
		hdr := http.Header{}
		err := creds.Apply(ctx, hdr)
		if err == nil {
			t.Errorf("%s: redirected token endpoint should fail, got header %q", strategy, hdr.Get("Authorization"))
		}
		if got := attackerSaw.Load().(string); got != "" {
			t.Errorf("%s: redirect target received credential material: %q", strategy, got)
		}
		if got := hdr.Get("Authorization"); strings.Contains(got, "attacker-minted") {
			t.Errorf("%s: attacker-minted token was accepted: %q", strategy, got)
		}
	}
}

// TestTrustAnchorClient: the JWKS-fetch client is bounded and refuses
// redirects — http.DefaultClient has neither property, which is why the
// gateway must never wire it for trust anchors.
func TestTrustAnchorClient(t *testing.T) {
	c := TrustAnchorClient()
	if c.Timeout <= 0 {
		t.Fatal("trust-anchor client must carry a timeout")
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	redirector := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	t.Cleanup(redirector.Close)
	_, err := c.Get(redirector.URL)
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("redirect must be refused, got %v", err)
	}
}

// An operator's resource URI may carry a trailing slash — RFC 9728 does not
// forbid one — and the endpoints derived from it must stay single-slashed, or
// the document a client discovers points at a path fold does not serve.
func TestAuthorizationServerMetadataEndpointsFromIssuer(t *testing.T) {
	for _, issuer := range []string{"https://gw.example.com", "https://gw.example.com/"} {
		m := &EMA{issuer: issuer}
		doc := m.AuthorizationServerMetadata()
		if doc["issuer"] != issuer {
			t.Errorf("issuer = %v, want %q (advertised verbatim)", doc["issuer"], issuer)
		}
		if got := doc["token_endpoint"]; got != "https://gw.example.com/oauth/token" {
			t.Errorf("issuer %q: token_endpoint = %v", issuer, got)
		}
		if got := doc["jwks_uri"]; got != "https://gw.example.com/.well-known/jwks.json" {
			t.Errorf("issuer %q: jwks_uri = %v", issuer, got)
		}
	}
}

// TestScopesFromClaims: there is no single spelling of "what this token was
// granted", so all four shapes in use are read — and everything else yields
// nothing, because a scope list nobody can parse must deny rather than admit.
func TestScopesFromClaims(t *testing.T) {
	cases := []struct {
		name   string
		claims map[string]any
		want   []string
	}{
		{"space-delimited scope (RFC 6749)", map[string]any{"scope": "read write"}, []string{"read", "write"}},
		{"single scope", map[string]any{"scope": "read"}, []string{"read"}},
		{"scope as an array", map[string]any{"scope": []any{"read", "write"}}, []string{"read", "write"}},
		{"scp as an array", map[string]any{"scp": []any{"read", "write"}}, []string{"read", "write"}},
		{"scp as a string", map[string]any{"scp": "read write"}, []string{"read", "write"}},
		{"extra whitespace and newlines", map[string]any{"scope": "  read \t write\nadmin  "}, []string{"read", "write", "admin"}},
		// The fallback is ordered, not merged: an issuer sending both sends
		// the same set twice, and unioning two claims that disagreed would
		// grant the caller the larger of them.
		{"scope wins over scp", map[string]any{"scope": "read", "scp": []any{"admin"}}, []string{"read"}},
		{"empty scope falls through to scp", map[string]any{"scope": "", "scp": []any{"admin"}}, []string{"admin"}},
		{"whitespace-only scope falls through", map[string]any{"scope": "   ", "scp": "admin"}, []string{"admin"}},
		{"empty array falls through", map[string]any{"scope": []any{}, "scp": "admin"}, []string{"admin"}},
		{"non-string array members are skipped", map[string]any{"scope": []any{"read", 42, nil, true, "write"}}, []string{"read", "write"}},
		{"empty array members are skipped", map[string]any{"scope": []any{"read", ""}}, []string{"read"}},
		// Fail closed on every shape that is neither a string nor an array,
		// the same reading the groups claim takes.
		{"number", map[string]any{"scope": float64(7)}, nil},
		{"object", map[string]any{"scope": map[string]any{"read": true}}, nil},
		{"bool", map[string]any{"scope": true}, nil},
		{"null", map[string]any{"scope": nil}, nil},
		{"absent", map[string]any{"sub": "alice"}, nil},
		{"no claims at all", nil, nil},
		{"unreadable scope and unreadable scp", map[string]any{"scope": float64(1), "scp": map[string]any{}}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ScopesFromClaims(c.claims)
			if len(got) != len(c.want) {
				t.Fatalf("ScopesFromClaims(%v) = %v, want %v", c.claims, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("ScopesFromClaims(%v) = %v, want %v", c.claims, got, c.want)
				}
			}
		})
	}
}

// The verified principal carries them, which is what policy and tenancy read.
// A token whose scope claim is unreadable yields a principal with none rather
// than a verification failure: the token is still valid, it just grants
// nothing a scope-gated rule will honour.
func TestVerifyCapturesScopes(t *testing.T) {
	const issuer = "https://scopes.example.com"
	key := newKey(t)
	v := NewVerifier(&config.Auth{Resource: resource}, http.DefaultClient)
	v.TrustLocal(issuer, &key.PublicKey)

	claims := baseClaims(issuer)
	claims["scope"] = "mcp:invoke docs:read"
	p, err := v.Verify(context.Background(), signToken(t, key, "", claims))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if strings.Join(p.Scopes, ",") != "mcp:invoke,docs:read" {
		t.Errorf("scopes = %v, want [mcp:invoke docs:read]", p.Scopes)
	}

	unreadable := baseClaims(issuer)
	unreadable["scope"] = float64(3)
	p, err = v.Verify(context.Background(), signToken(t, key, "", unreadable))
	if err != nil {
		t.Fatalf("Verify with an unreadable scope claim: %v", err)
	}
	if len(p.Scopes) != 0 {
		t.Errorf("scopes = %v, want none", p.Scopes)
	}
}
