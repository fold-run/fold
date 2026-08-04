package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/state"
)

// idJAGGrant is the RFC 7523 JWT-bearer grant type the EMA token endpoint
// accepts — the ID-JAG exchange.
const idJAGGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// EMA is fold's embedded MCP Authorization Server, deliberately one grant
// wide: exchange an enterprise-IdP-issued ID-JAG (Identity Assertion JWT
// Authorization Grant) for a short-lived fold-signed access token
// (Enterprise-Managed Authorization). Everything the gateway later accepts
// has aud = fold, which keeps upstream token exchange coherent.
type EMA struct {
	issuer   string // fold's resource URI: issuer and audience of minted tokens
	cfg      *config.EMAConfig
	key      *ecdsa.PrivateKey
	kid      string // RFC 7638 thumbprint of the public key
	jwksJSON []byte // public JWK set, served at /.well-known/jwks.json
	jwks     *jwksCache
	replay   state.Once
	ttl      time.Duration
}

// NewEMA loads the ES256 signing key named by signingKeyRef (a PKCS#8 PEM in
// an environment variable) and assembles the exchange service.
func NewEMA(cfg *config.Auth, client *http.Client, replay state.Once) (*EMA, error) {
	pemStr := os.Getenv(cfg.EMA.SigningKeyRef)
	if pemStr == "" {
		return nil, fmt.Errorf("ema: secret %q is not set", cfg.EMA.SigningKeyRef)
	}
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("ema: secret %q is not a PEM document", cfg.EMA.SigningKeyRef)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("ema: parse signing key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("ema: signing key must be an ES256 (P-256 ECDSA) key")
	}
	kid, jwksJSON, err := publicJWKS(&key.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("ema: encode public key: %w", err)
	}
	return &EMA{
		issuer:   cfg.Resource,
		cfg:      cfg.EMA,
		key:      key,
		kid:      kid,
		jwksJSON: jwksJSON,
		jwks:     newJWKSCache(client),
		replay:   replay,
		ttl:      time.Duration(cfg.EMA.ResolvedTokenTTLSec()) * time.Second,
	}, nil
}

// Issuer returns the issuer string of fold-minted tokens (the resource URI).
func (m *EMA) Issuer() string { return m.issuer }

// PublicKey returns the verification key for fold-minted tokens.
func (m *EMA) PublicKey() *ecdsa.PublicKey { return &m.key.PublicKey }

// ServeJWKS answers GET /.well-known/jwks.json with the public key set.
func (m *EMA) ServeJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(m.jwksJSON)
}

// ServeToken answers POST /oauth/token — the ID-JAG exchange. The caller is
// unauthenticated by design (the assertion is the credential); the gateway
// rate-limits this handler before it runs.
func (m *EMA) ServeToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		oauthError(w, http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	if r.PostForm.Get("grant_type") != idJAGGrant {
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", fmt.Sprintf("only %s is supported", idJAGGrant))
		return
	}
	assertion := r.PostForm.Get("assertion")
	if assertion == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "missing assertion")
		return
	}

	jwksURI := m.cfg.IdpJWKSURI
	if jwksURI == "" {
		jwksURI = m.cfg.IdpIssuer + "/.well-known/jwks.json"
	}
	claims := jwt.MapClaims{}
	_, err := jwt.NewParser(
		jwt.WithValidMethods(allowedAlgs),
		jwt.WithIssuer(m.cfg.IdpIssuer),
		jwt.WithAudience(m.issuer),
		// exp bounds the assertion; jti keys single-use replay protection.
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	).ParseWithClaims(assertion, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return m.jwks.key(r.Context(), jwksURI, kid)
	})
	if err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "ID-JAG validation failed")
		return
	}
	sub, _ := claims.GetSubject()
	if sub == "" {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "ID-JAG has no subject")
		return
	}
	jti, _ := claims["jti"].(string)
	exp, _ := claims.GetExpirationTime()
	if jti == "" || exp == nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "ID-JAG missing jti or exp")
		return
	}

	// Single-use: an ID-JAG must not be redeemable more than once within its
	// lifetime. Record the (hashed — jti is client-controlled) id until the
	// assertion expires and reject replays.
	ttl := max(time.Until(exp.Time), time.Second)
	if !m.replay.TryOnce(r.Context(), fmt.Sprintf("%x", sha256.Sum256([]byte(jti))), ttl) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "ID-JAG already redeemed")
		return
	}

	now := time.Now()
	minted := jwt.MapClaims{
		"iss": m.issuer,
		"aud": m.issuer,
		"sub": sub,
		"iat": now.Unix(),
		"exp": now.Add(m.ttl).Unix(),
	}
	// Carry the identity claims policy and audit consume.
	if email, ok := claims["email"].(string); ok {
		minted["email"] = email
	}
	if groups, ok := claims["groups"].([]any); ok {
		minted["groups"] = groups
	}
	if clientID, ok := claims["client_id"].(string); ok {
		minted["client_id"] = clientID
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, minted)
	tok.Header["kid"] = m.kid
	signed, err := tok.SignedString(m.key)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "token signing failed")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": signed,
		"token_type":   "Bearer",
		"expires_in":   int(m.ttl.Seconds()),
	})
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}

// publicJWKS builds the RFC 7638 thumbprint (the kid) and the marshaled
// public JWK set for a P-256 key.
func publicJWKS(pub *ecdsa.PublicKey) (kid string, jwksJSON []byte, err error) {
	// Uncompressed SEC1 point: 0x04 || X || Y with fixed-width 32-byte
	// coordinates for P-256.
	raw, err := pub.Bytes()
	if err != nil {
		return "", nil, err
	}
	b64 := base64.RawURLEncoding.EncodeToString
	x, y := b64(raw[1:33]), b64(raw[33:65])
	// Thumbprint input is the required members in lexicographic order.
	thumb := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":%q,"y":%q}`, x, y)
	sum := sha256.Sum256([]byte(thumb))
	kid = b64(sum[:])
	doc, _ := json.Marshal(map[string]any{
		"keys": []map[string]string{{
			"kty": "EC", "crv": "P-256", "x": x, "y": y,
			"kid": kid, "alg": "ES256", "use": "sig",
		}},
	})
	return kid, doc, nil
}
