package auth

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/fold-run/fold-go/config"
)

// allowed JWS algorithms — asymmetric only, mirroring fold.
var allowedAlgs = []string{"RS256", "RS384", "RS512", "ES256", "ES384", "ES512", "EdDSA"}

// Verifier validates gateway bearer tokens: trusted issuer (checked before
// any network I/O), signature via cached JWKS, exact audience match.
type Verifier struct {
	resource string
	issuers  map[string]config.Issuer // issuer URL → config
	jwks     *jwksCache
}

// NewVerifier builds a verifier from the auth config section.
func NewVerifier(cfg *config.Auth, client *http.Client) *Verifier {
	v := &Verifier{
		resource: cfg.Resource,
		issuers:  map[string]config.Issuer{},
		jwks:     newJWKSCache(client),
	}
	for _, iss := range cfg.Issuers {
		v.issuers[iss.Issuer] = iss
	}
	return v
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
		kid, _ := t.Header["kid"].(string)
		return v.jwks.key(ctx, jwksURI, kid)
	})
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}

	sub, _ := claims.GetSubject()
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
	return &Principal{Subject: sub, Issuer: issuer, Groups: groups, Token: raw, Expiry: expiry}, nil
}
