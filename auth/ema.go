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
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/fold-run/fold/config"
	"github.com/fold-run/fold/internal/state"
)

// idJAGGrant is the RFC 7523 JWT-bearer grant type the EMA token endpoint
// accepts — the ID-JAG exchange.
const idJAGGrant = "urn:ietf:params:oauth:grant-type:jwt-bearer"

// accessTokenTypes are JOSE "typ" values that an OAuth 2.0 access token
// carries (RFC 9068 §2.1 makes "at+jwt" a MUST, with the media-type spelling
// permitted). Nothing legitimately presented here as an identity assertion
// has one, so refusing them costs no compatibility.
//
// SEP-990 §5.1 says an assertion MUST carry typ "oauth-id-jag+jwt", and fold
// does not require that — a positive check is the stronger control and this
// is deliberately the weaker one. The reason is that a strict requirement
// cannot be turned on by default under the v1 contract: JWT libraries stamp
// typ "JWT" unless told otherwise, and fold cannot know from here how the
// operator's IdP mints its assertions. A deny-list closes the half of the
// problem that is unambiguous while a positive requirement waits for a
// config field.
//
// What remains open, stated plainly because the guards below hold less than
// they look like they do: /oauth/token accepts any JWT signed by a key at
// idpJwksUri with iss == idpIssuer, aud == auth.resource, a sub, a jti, and
// an exp. An IdP-issued token that is not an access token — an ID token, or
// a token minted for an application whose identifier URI happens to equal
// the gateway's resource URI — satisfies every one of those. That matters
// because issuers with mode "exchange" are excluded from direct presentation
// at /mcp precisely so the IdP's own tokens are not accepted as fold tokens,
// and this endpoint is the one place that admits them again. The intended
// gate is the IdP's own admin-approved assertion grant, which fold cannot
// see. Closing it properly needs either the positive typ requirement or a
// client_id allowlist — client_id is copied into the minted token but never
// checked. Tracked, not solved here.
var accessTokenTypes = [...]string{"at+jwt", "application/at+jwt"}

// idJAGGrantProfile is the grant profile advertised in the authorization
// server metadata, so a client can tell that this token endpoint speaks the
// identity-assertion profile rather than bare RFC 7523.
const idJAGGrantProfile = "urn:ietf:params:oauth:grant-profile:id-jag"

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
	// asMetadataJSON is the RFC 8414 document, marshalled once at
	// construction like jwksJSON: it is a static document, and rebuilding it
	// per request would put a map allocation on an unauthenticated endpoint.
	asMetadataJSON []byte
	jwks           *jwksCache
	replay         state.Once
	ttl            time.Duration

	// OnExchange, when set, is told the outcome of every token exchange —
	// including the refusals. The gateway wires it into the audit trail:
	// this endpoint is an authorization server, and a detected ID-JAG
	// replay, or an attacker fuzzing assertions under the rate limit, is
	// exactly the traffic a SIEM exists to see. Set before serving; not
	// synchronized after.
	OnExchange func(TokenExchange)
}

// TokenExchange describes one terminal /oauth/token response.
type TokenExchange struct {
	// Outcome is "minted" for a successful exchange, "replayed" for an
	// ID-JAG presented twice (the security event), or the OAuth error code
	// returned otherwise ("invalid_request", "unsupported_grant_type",
	// "invalid_grant", "server_error").
	Outcome string
	Detail  string // the error_description sent to the caller ("" when minted)
	Subject string // the assertion's subject, when validation got that far
	Issuer  string // the IdP issuer the assertion was verified against
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
	m := &EMA{
		issuer:   cfg.Resource,
		cfg:      cfg.EMA,
		key:      key,
		kid:      kid,
		jwksJSON: jwksJSON,
		jwks:     newJWKSCache(client),
		replay:   replay,
		ttl:      time.Duration(cfg.EMA.ResolvedTokenTTLSec()) * time.Second,
	}
	doc, err := json.Marshal(m.AuthorizationServerMetadata())
	if err != nil {
		return nil, fmt.Errorf("ema: build authorization server metadata: %w", err)
	}
	m.asMetadataJSON = doc
	return m, nil
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
// rate-limits this handler before it runs. Every terminal response reports
// through OnExchange — refusals included — so the trail this endpoint
// produces matches the single-exit-door rule the rest of the gateway obeys.
func (m *EMA) ServeToken(w http.ResponseWriter, r *http.Request) {
	// subject is filled in once validation has extracted one, so even a
	// refusal names who the assertion claimed to be when that is knowable.
	subject := ""
	refuse := func(status int, code, description string) {
		m.report(TokenExchange{Outcome: code, Detail: description, Subject: subject, Issuer: m.cfg.IdpIssuer})
		oauthError(w, status, code, description)
	}
	if r.Method != http.MethodPost {
		refuse(http.StatusMethodNotAllowed, "invalid_request", "POST only")
		return
	}
	if err := r.ParseForm(); err != nil {
		refuse(http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	if r.PostForm.Get("grant_type") != idJAGGrant {
		refuse(http.StatusBadRequest, "unsupported_grant_type", fmt.Sprintf("only %s is supported", idJAGGrant))
		return
	}
	assertion := r.PostForm.Get("assertion")
	if assertion == "" {
		refuse(http.StatusBadRequest, "invalid_request", "missing assertion")
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
		// Refuse before fetching a key: a token of the wrong type is not a
		// key-resolution problem, and rejecting it here keeps one from
		// costing a JWKS lookup.
		if typ, ok := t.Header["typ"].(string); ok {
			for _, bad := range accessTokenTypes {
				if strings.EqualFold(typ, bad) {
					return nil, fmt.Errorf("assertion typ %q is an access token, not an authorization grant", typ)
				}
			}
		}
		kid, _ := t.Header["kid"].(string)
		return m.jwks.key(r.Context(), jwksURI, kid)
	})
	if err != nil {
		refuse(http.StatusBadRequest, "invalid_grant", "ID-JAG validation failed")
		return
	}
	sub, _ := claims.GetSubject()
	if sub == "" {
		refuse(http.StatusBadRequest, "invalid_grant", "ID-JAG has no subject")
		return
	}
	subject = sub
	jti, _ := claims["jti"].(string)
	exp, _ := claims.GetExpirationTime()
	if jti == "" || exp == nil {
		refuse(http.StatusBadRequest, "invalid_grant", "ID-JAG missing jti or exp")
		return
	}

	// Single-use: an ID-JAG must not be redeemable more than once within its
	// lifetime. Record the (hashed — jti is client-controlled) id until the
	// assertion expires and reject replays.
	ttl := max(time.Until(exp.Time), time.Second)
	if !m.replay.TryOnce(r.Context(), fmt.Sprintf("%x", sha256.Sum256([]byte(jti))), ttl) {
		// Reported as "replayed", not folded into invalid_grant: a replay is
		// a signed, valid assertion presented twice — possession by a second
		// party is one of the readings, and the one the trail must surface.
		m.report(TokenExchange{Outcome: "replayed", Detail: "ID-JAG already redeemed", Subject: subject, Issuer: m.cfg.IdpIssuer})
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
		refuse(http.StatusInternalServerError, "server_error", "token signing failed")
		return
	}

	m.report(TokenExchange{Outcome: "minted", Subject: subject, Issuer: m.cfg.IdpIssuer})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": signed,
		"token_type":   "Bearer",
		"expires_in":   int(m.ttl.Seconds()),
	})
}

// report delivers one exchange outcome to OnExchange, if wired.
func (m *EMA) report(x TokenExchange) {
	if m.OnExchange != nil {
		m.OnExchange(x)
	}
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

// AuthorizationServerMetadata is the RFC 8414 document for the embedded
// authorization server.
//
// fold lists itself in `authorization_servers` of its RFC 9728 protected-
// resource metadata whenever EMA is on, which tells a client to discover this
// document — so not serving it left the advertisement pointing at a 404 and
// forced every operator to configure the token endpoint out of band. The
// document is deliberately narrow: it describes the one grant this server
// implements and claims nothing else.
func (m *EMA) AuthorizationServerMetadata() map[string]any {
	// issuer is reported exactly as configured, because RFC 8414 §3.3 makes a
	// client reject a document whose issuer is not the identifier it derived
	// the request URL from — so "normalizing" it here would break the very
	// clients this endpoint exists to serve.
	//
	// The endpoints, by contrast, are derived from the issuer's *origin*. A
	// canonical MCP resource identifier commonly carries a path
	// ("https://gw.example.com/mcp"), and fold serves the token endpoint and
	// the key set at fixed root paths regardless — so concatenating onto the
	// full issuer would advertise ".../mcp/oauth/token", which 404s.
	origin := m.issuer
	if u, err := url.Parse(m.issuer); err == nil && u.Scheme != "" && u.Host != "" {
		origin = u.Scheme + "://" + u.Host
	}
	origin = strings.TrimSuffix(origin, "/")
	return map[string]any{
		"issuer":                   m.issuer,
		"token_endpoint":           origin + "/oauth/token",
		"jwks_uri":                 origin + "/.well-known/jwks.json",
		"grant_types_supported":    []string{idJAGGrant},
		"grant_profiles_supported": []string{idJAGGrantProfile},
		// The assertion is the credential, so the endpoint authenticates no
		// client of its own. Saying so explicitly is the difference between a
		// client sending no credential and a client guessing at one.
		"token_endpoint_auth_methods_supported": []string{"none"},
		"response_types_supported":              []string{},
	}
}

// AuthorizationServerMetadataPaths returns every well-known path this document
// must be reachable at.
//
// RFC 8414 §3.1 locates the document by inserting the well-known segment
// *before* the issuer's path, so an issuer with a path is discovered at
// "/.well-known/oauth-authorization-server/{path}" rather than at the root.
// fold advertises itself in the RFC 9728 document using whatever
// auth.resource says, so whichever form that takes has to resolve — hence
// both, when they differ.
func (m *EMA) AuthorizationServerMetadataPaths() []string {
	const root = "/.well-known/oauth-authorization-server"
	paths := []string{root}
	u, err := url.Parse(m.issuer)
	if err != nil {
		return paths
	}
	if p := strings.Trim(u.Path, "/"); p != "" {
		paths = append(paths, root+"/"+p)
	}
	return paths
}

// ServeAuthorizationServerMetadata answers the RFC 8414 well-known paths.
//
// CORS mirrors what the SDK sets on the protected-resource document: a
// browser-based MCP client discovers this from script, so without the header
// it can read fold's RFC 9728 metadata and then fail on the authorization
// server it points at.
func (m *EMA) ServeAuthorizationServerMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	_, _ = w.Write(m.asMetadataJSON)
}
