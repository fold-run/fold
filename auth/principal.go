// Package auth implements the gateway's OAuth 2.0 resource server (Bearer
// JWT validation against an issuer allowlist via cached JWKS, with exact
// audience matching) and the upstream credential strategies.
package auth

import (
	"context"
	"time"
)

// Principal is the authenticated caller of a gateway request.
type Principal struct {
	Subject string    // "sub" claim
	Issuer  string    // "iss" claim
	Groups  []string  // from the issuer's configured groups claim
	Token   string    // the raw bearer token (for passthrough / token-exchange)
	Expiry  time.Time // token expiration

	// Claims is the verified token's full claim set, for attribute-based
	// policy (policy subjects' "claims" matcher). Values are as decoded
	// from JSON: string, float64, bool, nil, []any, map[string]any.
	Claims map[string]any
}

type principalKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFromContext returns the authenticated principal, or nil when the
// gateway runs with auth disabled.
func PrincipalFromContext(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey{}).(*Principal)
	return p
}
