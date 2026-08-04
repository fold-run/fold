package auth

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/fold-run/fold/config"
)

// allowed JWS algorithms — asymmetric only, mirroring fold.
var allowedAlgs = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "EdDSA"}

// Verifier validates gateway bearer tokens: trusted issuer (checked before
// any network I/O), signature via cached JWKS, exact audience match.
type Verifier struct {
	resource string
	issuers  map[string]config.Issuer // issuer URL → config
	local    map[string]any           // issuer URL → locally held public key
	jwks     *jwksCache
}

// NewVerifier builds a verifier from the auth config section. Only "direct"
// issuers are trusted for straight token presentation: an "exchange"
// issuer's tokens (ID-JAGs) must go through the EMA exchange — accepting
// one directly would let it stand in for a fold access token.
func NewVerifier(cfg *config.Auth, client *http.Client) *Verifier {
	v := &Verifier{
		resource: cfg.Resource,
		issuers:  map[string]config.Issuer{},
		local:    map[string]any{},
		jwks:     newJWKSCache(client),
	}
	for _, iss := range cfg.Issuers {
		if iss.Mode == "exchange" {
			continue
		}
		v.issuers[iss.Issuer] = iss
	}
	return v
}

// TrustLocal registers an issuer whose tokens verify against a locally held
// public key — fold's own EMA-minted tokens — with no JWKS fetch.
func (v *Verifier) TrustLocal(issuer string, key any) {
	v.issuers[issuer] = config.Issuer{Issuer: issuer}
	v.local[issuer] = key
}

// Verify validates a raw bearer token and returns the principal it names.
func (v *Verifier) Verify(ctx context.Context, raw string) (*Principal, error) {
	// Read the issuer without verifying, so unknown issuers are rejected
	// before any JWKS fetch.
	var pre jwt.MapClaims
	if _, _, err := jwt.NewParser().ParseUnverified(raw, &pre); err != nil {
		return nil, fmt.Errorf("malformed token: %w", err)
	}
	issuer, _ := pre.GetIssuer()
	issCfg, ok := v.issuers[issuer]
	if !ok {
		return nil, fmt.Errorf("untrusted issuer %q", issuer)
	}
	jwksURI := issCfg.JWKSURI
	if jwksURI == "" {
		jwksURI = issuer + "/.well-known/jwks.json"
	}

	claims := jwt.MapClaims{}
	_, err := jwt.NewParser(
		jwt.WithValidMethods(allowedAlgs),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(v.resource),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	).ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if key, ok := v.local[issuer]; ok {
			return key, nil
		}
		kid, _ := t.Header["kid"].(string)
		return v.jwks.key(ctx, jwksURI, kid)
	})
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	sub, _ := claims.GetSubject()
	if sub == "" {
		// A token with no subject would yield an empty UserID, which
		// disables the SDK's per-session identity binding (any other valid
		// token could then attach to the session), and would collide every
		// such caller onto one identity for policy and token-exchange
		// caching. Require a subject.
		return nil, fmt.Errorf("invalid token: missing sub claim")
	}
	exp, _ := claims.GetExpirationTime()
	var expiry time.Time
	if exp != nil {
		expiry = exp.Time
	}
	groupsClaim := issCfg.GroupsClaim
	if groupsClaim == "" {
		groupsClaim = "groups"
	}
	var groups []string
	if gs, ok := claims[groupsClaim].([]any); ok {
		for _, g := range gs {
			if s, ok := g.(string); ok {
				groups = append(groups, s)
			}
		}
	}
	return &Principal{Subject: sub, Issuer: issuer, Groups: groups, Token: raw, Expiry: expiry, Claims: claims}, nil
}
