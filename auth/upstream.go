package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fold-run/fold-go/config"
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

	mu     sync.Mutex
	tokens map[string]*cachedToken // cache key → token ("" for client-credentials)
}

type cachedToken struct {
	value   string
	expires time.Time
}

// NewUpstreamCredentials builds the credential injector for one upstream.
// A nil cfg (or strategy "none") attaches nothing.
func NewUpstreamCredentials(cfg *config.UpstreamAuth, client *http.Client) *UpstreamCredentials {
	if client == nil {
		client = http.DefaultClient
	}
	return &UpstreamCredentials{cfg: cfg, client: client, tokens: map[string]*cachedToken{}}
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
		tok, err := c.cachedFetch(ctx, p.Subject, func() (url.Values, error) {
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

func (c *UpstreamCredentials) cachedFetch(ctx context.Context, key string, form func() (url.Values, error)) (string, error) {
	c.mu.Lock()
	if t := c.tokens[key]; t != nil && time.Now().Before(t.expires) {
		v := t.value
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

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
	c.mu.Unlock()
	return tok, nil
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
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
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
