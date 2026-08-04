package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	// ...and once the cached set is stale, the same token verifies.
	v.jwks.mu.Lock()
	v.jwks.sets[srv.URL].fetched = time.Now().Add(-time.Minute)
	v.jwks.mu.Unlock()
	if _, err := v.Verify(context.Background(), rotated); err != nil {
		t.Fatalf("verify after rotation with stale cache: %v", err)
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
