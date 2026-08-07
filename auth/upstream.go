package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fold-run/fold/config"
)

// UpstreamCredentials attaches credentials to requests bound for one
// upstream, according to its configured strategy:
//
//	none               — nothing attached
//	static             — an API key from the secret store (env vars)
//	passthrough        — the caller's bearer token, forwarded as-is
//	client-credentials — a service-identity token (OAuth 2.0 CC grant),
//	                     cached until 60s before expiry
//	token-exchange     — RFC 8693: the caller's token exchanged for an
//	                     upstream-audience token, cached per subject
type UpstreamCredentials struct {
	cfg    *config.UpstreamAuth
	client *http.Client

	mu      sync.Mutex
	tokens  map[string]*cachedToken // cache key → token ("" for client-credentials)
	fetchMu map[string]*sync.Mutex  // per-key fetch serialization
}

type cachedToken struct {
	value   string
	expires time.Time
}

// maxTokenResponseBytes bounds a token-endpoint response body. It is the
// last unbounded read fold performs against a remote party (JWKS and the
// discovery document are already capped): a compromised or broken token
// endpoint could otherwise stream until the gateway runs out of memory.
const maxTokenResponseBytes = 1 << 20 // 1 MiB

// maxCachedTokens bounds the per-upstream exchanged-token cache. Under
// token-exchange the cache is keyed per (issuer, subject), so it grows with
// the number of distinct callers and never shrinks on its own — an eviction
// only costs a re-exchange.
const maxCachedTokens = 4096

// NewUpstreamCredentials builds the credential injector for one upstream.
// A nil cfg (or strategy "none") attaches nothing.
//
// The caller's client is wrapped so token-endpoint requests never follow a
// redirect. Those requests carry the most sensitive material fold handles —
// the client secret under client-credentials, and the caller's own bearer
// token as subject_token under token-exchange — and Go replays a POST body
// verbatim on 307/308, so a token endpoint that redirects (compromised,
// misconfigured, or hosting an open redirect) would otherwise hand both to
// whatever host it names. Refusing every redirect is safe here: token
// endpoints answer 200 with the token, and a redirect is never a legitimate
// step in the grant.
func NewUpstreamCredentials(cfg *config.UpstreamAuth, client *http.Client) *UpstreamCredentials {
	if client == nil {
		client = http.DefaultClient
	}
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &UpstreamCredentials{
		cfg:     cfg,
		client:  &noRedirect,
		tokens:  map[string]*cachedToken{},
		fetchMu: map[string]*sync.Mutex{},
	}
}

// Apply sets credential headers on hdr for a request running under ctx.
func (c *UpstreamCredentials) Apply(ctx context.Context, hdr http.Header) error {
	if c == nil || c.cfg == nil {
		return nil
	}
	switch c.cfg.Strategy {
	case "", "none":
		return nil
	case "static":
		secret := os.Getenv(c.cfg.SecretRef)
		if secret == "" {
			return fmt.Errorf("secret %q is not set", c.cfg.SecretRef)
		}
		header := c.cfg.Header
		if header == "" {
			header = "Authorization"
		}
		scheme := c.cfg.Scheme
		if scheme == "" && strings.EqualFold(header, "Authorization") {
			scheme = "Bearer"
		}
		if scheme != "" {
			hdr.Set(header, scheme+" "+secret)
		} else {
			hdr.Set(header, secret)
		}
		return nil
	case "passthrough":
		p := PrincipalFromContext(ctx)
		if p == nil || p.Token == "" {
			return fmt.Errorf("passthrough auth requires an authenticated caller")
		}
		hdr.Set("Authorization", "Bearer "+p.Token)
		return nil
	case "client-credentials":
		tok, err := c.cachedFetch(ctx, "", c.clientCredentialsForm)
		if err != nil {
			return err
		}
		hdr.Set("Authorization", "Bearer "+tok)
		return nil
	case "token-exchange":
		p := PrincipalFromContext(ctx)
		if p == nil || p.Token == "" {
			return fmt.Errorf("token-exchange auth requires an authenticated caller")
		}
		if p.Subject == "" || p.Issuer == "" {
			// Never share one cache entry across subjects: an empty subject
			// would collide every such caller onto one exchanged token.
			return fmt.Errorf("token-exchange auth requires a token with iss and sub claims")
		}
		// Key per (issuer, subject): sub is only unique within an issuer, and
		// the gateway may trust several. \x00 cannot appear in either claim.
		cacheKey := p.Issuer + "\x00" + p.Subject
		tok, err := c.cachedFetch(ctx, cacheKey, func() (url.Values, error) {
			return c.tokenExchangeForm(p.Token)
		})
		if err != nil {
			return err
		}
		hdr.Set("Authorization", "Bearer "+tok)
		return nil
	}
	return fmt.Errorf("unknown auth strategy %q", c.cfg.Strategy)
}

// cachedFetch returns the cached token for key, or fetches one. Fetches are
// single-flighted per key: a burst of first-time requests for one principal
// would otherwise become a burst of token-endpoint calls, each carrying the
// client secret and (under token-exchange) that caller's own bearer token —
// the same amplification the JWKS cache already refuses to create.
func (c *UpstreamCredentials) cachedFetch(ctx context.Context, key string, form func() (url.Values, error)) (string, error) {
	if v, ok := c.cached(key); ok {
		return v, nil
	}

	c.mu.Lock()
	fm := c.fetchMu[key]
	if fm == nil {
		fm = &sync.Mutex{}
		c.fetchMu[key] = fm
	}
	c.mu.Unlock()

	fm.Lock()
	defer fm.Unlock()

	// Re-check: the goroutine we queued behind may have filled the entry.
	if v, ok := c.cached(key); ok {
		return v, nil
	}

	values, err := form()
	if err != nil {
		return "", err
	}
	tok, ttl, err := c.tokenRequest(ctx, values)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	// Cache until 60s before expiry.
	c.tokens[key] = &cachedToken{value: tok, expires: time.Now().Add(ttl - time.Minute)}
	c.pruneLocked()
	c.mu.Unlock()
	return tok, nil
}

func (c *UpstreamCredentials) cached(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t := c.tokens[key]; t != nil && time.Now().Before(t.expires) {
		return t.value, true
	}
	return "", false
}

// pruneLocked keeps the token cache bounded: expired entries always go, and
// if live entries alone still exceed the cap, entries are dropped until they
// fit. Dropping a live token costs one re-exchange, never correctness.
// Caller holds c.mu.
func (c *UpstreamCredentials) pruneLocked() {
	if len(c.tokens) <= maxCachedTokens {
		return
	}
	now := time.Now()
	for k, t := range c.tokens {
		if !now.Before(t.expires) {
			delete(c.tokens, k)
			delete(c.fetchMu, k)
		}
	}
	for k := range c.tokens {
		if len(c.tokens) <= maxCachedTokens {
			break
		}
		delete(c.tokens, k)
		delete(c.fetchMu, k)
	}
}

func (c *UpstreamCredentials) clientCredentialsForm() (url.Values, error) {
	v := url.Values{"grant_type": {"client_credentials"}}
	if len(c.cfg.Scopes) > 0 {
		v.Set("scope", strings.Join(c.cfg.Scopes, " "))
	}
	if c.cfg.Resource != "" {
		v.Set("resource", c.cfg.Resource)
	}
	return v, nil
}

func (c *UpstreamCredentials) tokenExchangeForm(subjectToken string) (url.Values, error) {
	v := url.Values{
		"grant_type":         {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":      {subjectToken},
		"subject_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"audience":           {c.cfg.Audience},
	}
	if len(c.cfg.Scopes) > 0 {
		v.Set("scope", strings.Join(c.cfg.Scopes, " "))
	}
	return v, nil
}

func (c *UpstreamCredentials) tokenRequest(ctx context.Context, form url.Values) (token string, ttl time.Duration, err error) {
	secret := os.Getenv(c.cfg.ClientAuth.SecretRef)
	if secret == "" {
		return "", 0, fmt.Errorf("secret %q is not set", c.cfg.ClientAuth.SecretRef)
	}
	if c.cfg.ClientAuth.Type == "client_secret_post" {
		form.Set("client_id", c.cfg.ClientID)
		form.Set("client_secret", secret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.cfg.ClientAuth.Type == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(secret))
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("token endpoint: status %d", resp.StatusCode)
	}
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxTokenResponseBytes)).Decode(&body); err != nil {
		return "", 0, fmt.Errorf("token endpoint: %w", err)
	}
	if body.AccessToken == "" {
		return "", 0, fmt.Errorf("token endpoint: empty access_token")
	}
	ttl = time.Hour
	if body.ExpiresIn > 0 {
		ttl = time.Duration(body.ExpiresIn) * time.Second
	}
	return body.AccessToken, ttl, nil
}

// PerRequest reports whether this strategy derives credentials from the
// caller (so they must be attached per request, not per session).
func (c *UpstreamCredentials) PerRequest() bool {
	if c == nil || c.cfg == nil {
		return false
	}
	return c.cfg.Strategy == "passthrough" || c.cfg.Strategy == "token-exchange"
}
