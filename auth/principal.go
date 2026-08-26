// Package auth implements the gateway's OAuth 2.0 resource server (Bearer
// JWT validation against an issuer allowlist via cached JWKS, with exact
// audience matching) and the upstream credential strategies.
package auth

import (
	"context"
	"strings"
	"time"
)

// Principal is the authenticated caller of a gateway request.
type Principal struct {
	Subject string    // "sub" claim
	Issuer  string    // "iss" claim
	Groups  []string  // from the issuer's configured groups claim
	Scopes  []string  // OAuth scopes, from the "scope" or "scp" claim
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

// ScopesFromClaims reads the OAuth scopes a verified token carries.
//
// There is no single spelling to read, so both in use are accepted. RFC 6749
// §3.3 and RFC 9068 §2.2.3 make "scope" a *space-delimited string*, which is
// why a policy could not previously express a scope requirement at all: the
// claims matcher compares whole values, and "write" is not equal to
// "read write admin". Entra spells the same thing "scp", and some issuers
// send either as a JSON array. All four shapes are read here so a rule can be
// written once against the concept rather than against an issuer's spelling.
//
// "scope" is read first and "scp" only when it yields nothing, rather than
// merging the two: an issuer that sends both sends the same set twice, and
// merging would silently union two claims that disagreed.
func ScopesFromClaims(claims map[string]any) []string {
	for _, key := range [...]string{"scope", "scp"} {
		if got := scopeValues(claims[key]); len(got) > 0 {
			return got
		}
	}
	return nil
}

// scopeValues normalizes one claim value into a scope list. A string is split
// on whitespace per RFC 6749; an array contributes its string members. A shape
// that is neither yields nothing, which denies rather than grants — the same
// fail-closed reading the groups claim already takes for a non-array value.
func scopeValues(v any) []string {
	switch t := v.(type) {
	case string:
		return strings.Fields(t)
	case []any:
		var out []string
		for _, e := range t {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
