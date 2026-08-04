// Package config defines the fold gateway configuration schema, loading,
// and validation. It mirrors fold's single-JSON-document configuration:
// upstreams, auth, policy, audit, routing, and server sections.
package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Config is the root configuration document.
type Config struct {
	Upstreams []Upstream     `json:"upstreams"`
	Auth      *Auth          `json:"auth,omitempty"`
	Policy    *Policy        `json:"policy,omitempty"`
	Audit     *Audit         `json:"audit,omitempty"`
	Routing   *Routing       `json:"routing,omitempty"`
	Server    *ServerSection `json:"server,omitempty"`
}

// Upstream describes one MCP server folded into the gateway.
type Upstream struct {
	ID  string `json:"id"`
	URL string `json:"url,omitempty"`

	// URLs lists multiple equivalent replicas of this upstream. The gateway
	// load-balances new sessions across them round-robin, fails over to the
	// next endpoint when one refuses connections, and rests a failed endpoint
	// for the circuit breaker's halfOpenAfterMs before retrying it. Exactly
	// one of url / urls must be set.
	URLs []string `json:"urls,omitempty"`

	Namespace string `json:"namespace,omitempty"`

	// Protocol selects the era of the upstream connection. "session" (the
	// default) negotiates the sessionful handshake, which is required for
	// server-initiated traffic (sampling, elicitation, logging, progress,
	// resource-update notifications) to bridge through the gateway.
	// "auto" lets the SDK prefer the newest protocol (2026-07-28
	// stateless), which cannot carry server-initiated requests.
	Protocol string `json:"protocol,omitempty"`

	Owner    *Owner            `json:"owner,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
	Auth     *UpstreamAuth     `json:"auth,omitempty"`
	Timeouts *Timeouts         `json:"timeouts,omitempty"`

	CircuitBreaker *CircuitBreaker `json:"circuitBreaker,omitempty"`
	RateLimit      *RateLimit      `json:"rateLimit,omitempty"`

	// HealthCheck enables active endpoint probing: every intervalMs the
	// gateway connects to each endpoint, ejecting dead replicas from the
	// balancer before client traffic hits them and restoring recovered ones
	// without waiting for a live-request retry. Absent → passive health only
	// (connect failures eject, cooldown restores).
	HealthCheck *HealthCheck `json:"healthCheck,omitempty"`

	// CacheTTLMs bounds staleness of cached list results (tools/prompts/
	// resources) for this upstream. 0 uses the gateway default (30s);
	// negative disables caching.
	CacheTTLMs int `json:"cacheTtlMs,omitempty"`
}

// Owner records which organization runs an upstream. Surfaces in audit and health.
type Owner struct {
	Org     string `json:"org,omitempty"`
	Team    string `json:"team,omitempty"`
	Contact string `json:"contact,omitempty"`
}

// UpstreamAuth selects the credential strategy used toward an upstream.
type UpstreamAuth struct {
	// Strategy is one of "none", "static", "passthrough",
	// "client-credentials", or "token-exchange".
	Strategy string `json:"strategy"`

	// static
	SecretRef string `json:"secretRef,omitempty"` // env var holding the secret
	Header    string `json:"header,omitempty"`    // default "Authorization"
	Scheme    string `json:"scheme,omitempty"`    // default "Bearer" when header is Authorization

	// client-credentials / token-exchange
	TokenEndpoint string      `json:"tokenEndpoint,omitempty"`
	ClientID      string      `json:"clientId,omitempty"`
	ClientAuth    *ClientAuth `json:"clientAuth,omitempty"`
	Scopes        []string    `json:"scopes,omitempty"`
	Resource      string      `json:"resource,omitempty"` // client-credentials (RFC 8707)
	Audience      string      `json:"audience,omitempty"` // token-exchange (RFC 8693)
}

// ClientAuth describes how the gateway authenticates to a token endpoint.
type ClientAuth struct {
	Type      string `json:"type"` // "client_secret_post" | "client_secret_basic"
	SecretRef string `json:"secretRef"`
}

// Timeouts bounds upstream I/O.
type Timeouts struct {
	ConnectMs    int `json:"connectMs,omitempty"`    // default 5000
	RequestMs    int `json:"requestMs,omitempty"`    // default 60000
	StreamIdleMs int `json:"streamIdleMs,omitempty"` // default 120000
}

// HealthCheck configures active endpoint probing.
type HealthCheck struct {
	IntervalMs int `json:"intervalMs"`
}

// CircuitBreaker short-circuits an unhealthy upstream.
type CircuitBreaker struct {
	FailureThreshold int `json:"failureThreshold,omitempty"` // default 5
	HalfOpenAfterMs  int `json:"halfOpenAfterMs,omitempty"`  // default 30000
}

// RateLimit is a fixed-window request budget.
type RateLimit struct {
	RequestsPerMinute int `json:"requestsPerMinute"`
	// PerPrincipalPerMinute additionally caps each authenticated principal
	// on its own bucket, so one tenant's flood cannot consume the shared
	// budget and starve every other tenant. Server-level only; requires
	// auth (anonymous callers are governed by the global budget alone).
	PerPrincipalPerMinute int `json:"perPrincipalPerMinute,omitempty"`
}

// Auth configures the gateway's OAuth 2.0 resource server.
type Auth struct {
	Mode     string   `json:"mode,omitempty"` // "disabled" (default) | "required"
	Resource string   `json:"resource,omitempty"`
	Issuers  []Issuer `json:"issuers,omitempty"`

	// EMA enables Enterprise-Managed Authorization: fold's embedded
	// one-grant token endpoint exchanging enterprise-IdP ID-JAGs for
	// fold-signed access tokens.
	EMA *EMAConfig `json:"ema,omitempty"`
}

// Issuer is a trusted token issuer.
type Issuer struct {
	Issuer      string `json:"issuer"`
	JWKSURI     string `json:"jwksUri,omitempty"`     // default {issuer}/.well-known/jwks.json
	GroupsClaim string `json:"groupsClaim,omitempty"` // default "groups"

	// Mode is "direct" (default): clients present this issuer's access
	// tokens straight to fold — or "exchange": this issuer's tokens are
	// ID-JAGs redeemable only at the EMA token endpoint; presenting one
	// directly as a fold token is rejected.
	Mode string `json:"mode,omitempty"`
}

// EMAConfig is the Enterprise-Managed Authorization section.
type EMAConfig struct {
	// IdpIssuer is the enterprise IdP that issues ID-JAGs.
	IdpIssuer  string `json:"idpIssuer"`
	IdpJWKSURI string `json:"idpJwksUri,omitempty"` // default {idpIssuer}/.well-known/jwks.json

	// SigningKeyRef names an environment variable holding fold's ES256
	// private key (PKCS#8 PEM) for signing minted access tokens.
	SigningKeyRef string `json:"signingKeyRef"`

	TokenTTLSec int `json:"tokenTtlSec,omitempty"` // minted-token lifetime; default 600

	// TokenRateLimitPerMinute caps the unauthenticated /oauth/token
	// endpoint (anti-amplification). Default 600.
	TokenRateLimitPerMinute int `json:"tokenRateLimitPerMinute,omitempty"`
}

// ResolvedTokenTTLSec returns the minted-token lifetime (default 600).
func (e *EMAConfig) ResolvedTokenTTLSec() int {
	if e.TokenTTLSec > 0 {
		return e.TokenTTLSec
	}
	return 600
}

// ResolvedTokenRateLimit returns the /oauth/token cap (default 600/min).
func (e *EMAConfig) ResolvedTokenRateLimit() int {
	if e.TokenRateLimitPerMinute > 0 {
		return e.TokenRateLimitPerMinute
	}
	return 600
}

// Policy is the deny-by-default allowlist engine configuration.
type Policy struct {
	DefaultDecision string       `json:"defaultDecision,omitempty"` // "deny" (default) | "allow"
	Rules           []PolicyRule `json:"rules,omitempty"`
}

// PolicyRule allows a set of principals a set of invocations.
type PolicyRule struct {
	ID       string          `json:"id"`
	Subjects *PolicySubjects `json:"subjects,omitempty"` // omit → any principal
	Allow    []PolicyAllow   `json:"allow"`
}

// PolicySubjects matches principals by group membership and/or subject,
// optionally scoped to specific token issuers. Because subjects and groups
// are only unique within an issuer, scope rules to an issuer whenever more
// than one issuer is trusted.
type PolicySubjects struct {
	Groups  []string `json:"groups,omitempty"`
	Subs    []string `json:"subs,omitempty"`
	Issuers []string `json:"issuers,omitempty"`
}

// PolicyAllow grants methods/names on one upstream. Names support "*" globs.
type PolicyAllow struct {
	Server  string   `json:"server"`
	Methods []string `json:"methods,omitempty"` // omit → all methods
	Names   []string `json:"names,omitempty"`   // omit → all names
}

// Audit configures audit event emission.
type Audit struct {
	Sinks []AuditSink `json:"sinks"`
}

// AuditSink is one audit destination.
type AuditSink struct {
	Type    string            `json:"type"` // "stdout" | "webhook"
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// Routing tunes name federation.
type Routing struct {
	NamespaceSeparator string `json:"namespaceSeparator,omitempty"` // default "__"

	// PageSize bounds federated list results (tools/prompts/resources/
	// templates) per page. 0 uses the default (200); negative disables
	// pagination and returns the full merged list as a single page.
	// tasks/list is always a single merged page.
	PageSize int `json:"pageSize,omitempty"`
}

// ServerSection configures the gateway's own HTTP server.
type ServerSection struct {
	MCPPath      string     `json:"mcpPath,omitempty"`      // default "/mcp"
	AllowedHosts []string   `json:"allowedHosts,omitempty"` // default localhost set; ["*"] disables
	RateLimit    *RateLimit `json:"rateLimit,omitempty"`    // global, across all upstreams

	// MaxBodyBytes caps request body size (413 beyond it), bounding the
	// memory one request can pin. Default 1 MiB.
	MaxBodyBytes int64 `json:"maxBodyBytes,omitempty"`

	// RedisURL shares cache, rate-limit, and circuit-breaker state across
	// gateway instances (redis:// URL). Defaults to the REDIS_URL
	// environment variable; absent → in-process state.
	RedisURL string `json:"redisUrl,omitempty"`
}

var idRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// sepCollisionRE matches namespace-alphabet characters. A separator
// containing any of them is ambiguous: names are split at the separator's
// first occurrence, so a namespace could swallow or shed characters (e.g.
// separator "-" with namespace "my-ns" routes "my-ns-tool" to "my").
var sepCollisionRE = regexp.MustCompile(`[a-z0-9-]`)

// requireSecureEndpoint rejects a security-critical endpoint reachable over
// cleartext HTTP (token endpoints carry client secrets; issuer/JWKS URLs are
// the inbound trust anchor). Loopback hosts are exempted for local
// development. what labels the endpoint in the error.
func requireSecureEndpoint(what, endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%s: %q is not a valid URL", what, endpoint)
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	if parsed.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1") {
		return nil
	}
	return fmt.Errorf("%s: must use https (got %q) — it is security-critical", what, endpoint)
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the operator-supplied config location
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse validates a raw JSON config document.
func Parse(data []byte) (*Config, error) {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Validate checks cross-field invariants.
func (c *Config) Validate() error {
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("config: at least one upstream is required")
	}
	seenID := map[string]bool{}
	seenNS := map[string]bool{}
	for i := range c.Upstreams {
		u := &c.Upstreams[i]
		if !idRE.MatchString(u.ID) {
			return fmt.Errorf("upstream %q: id must be lowercase alphanumeric + hyphens", u.ID)
		}
		if seenID[u.ID] {
			return fmt.Errorf("upstream %q: duplicate id", u.ID)
		}
		seenID[u.ID] = true
		if u.URL != "" && len(u.URLs) > 0 {
			return fmt.Errorf("upstream %q: set url or urls, not both", u.ID)
		}
		eps := u.Endpoints()
		if len(eps) == 0 {
			return fmt.Errorf("upstream %q: url (or urls) is required", u.ID)
		}
		seenEp := map[string]bool{}
		for _, ep := range eps {
			parsed, err := url.Parse(ep)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return fmt.Errorf("upstream %q: %q must be an absolute http(s) URL", u.ID, ep)
			}
			if seenEp[ep] {
				return fmt.Errorf("upstream %q: duplicate endpoint %q", u.ID, ep)
			}
			seenEp[ep] = true
		}
		if u.Namespace != "" {
			if !idRE.MatchString(u.Namespace) {
				return fmt.Errorf("upstream %q: namespace must be lowercase alphanumeric + hyphens", u.ID)
			}
			if seenNS[u.Namespace] {
				return fmt.Errorf("upstream %q: duplicate namespace %q", u.ID, u.Namespace)
			}
			seenNS[u.Namespace] = true
		}
		if u.HealthCheck != nil && u.HealthCheck.IntervalMs <= 0 {
			return fmt.Errorf("upstream %q: healthCheck.intervalMs must be positive", u.ID)
		}
		switch u.Protocol {
		case "", "session", "auto", "2026-07-28":
		default:
			return fmt.Errorf("upstream %q: protocol must be %q, %q, or %q", u.ID, "session", "auto", "2026-07-28")
		}
		if err := u.validateAuth(); err != nil {
			return err
		}
		// Per-principal credential strategies are meaningless without a
		// verified caller identity: passthrough would forward whatever
		// header an anonymous caller supplied, and token-exchange has no
		// subject to exchange for (its cache would pool tokens across
		// callers).
		if u.Auth != nil && (u.Auth.Strategy == "passthrough" || u.Auth.Strategy == "token-exchange") && !c.AuthRequired() {
			return fmt.Errorf("upstream %q: auth strategy %q requires auth.mode %q", u.ID, u.Auth.Strategy, "required")
		}
	}
	if len(c.Upstreams) > 1 {
		for i := range c.Upstreams {
			if c.Upstreams[i].Namespace == "" {
				return fmt.Errorf("upstream %q: namespace is required when multiple upstreams are configured", c.Upstreams[i].ID)
			}
		}
	}
	if c.Routing != nil && c.Routing.NamespaceSeparator != "" && sepCollisionRE.MatchString(c.Routing.NamespaceSeparator) {
		return fmt.Errorf("routing: namespaceSeparator %q must not contain lowercase letters, digits, or hyphens — those can appear in namespaces, making name splitting ambiguous", c.Routing.NamespaceSeparator)
	}
	if c.Server != nil && c.Server.MaxBodyBytes < 0 {
		return fmt.Errorf("server: maxBodyBytes must be positive")
	}
	if c.Auth != nil {
		switch c.Auth.Mode {
		case "", "disabled":
		case "required":
			if c.Auth.Resource == "" {
				return fmt.Errorf(`auth: "resource" is required when mode is "required"`)
			}
			if len(c.Auth.Issuers) == 0 {
				return fmt.Errorf(`auth: at least one issuer is required when mode is "required"`)
			}
			for _, iss := range c.Auth.Issuers {
				if _, err := url.Parse(iss.Issuer); err != nil || iss.Issuer == "" {
					return fmt.Errorf("auth: issuer %q is not a valid URL", iss.Issuer)
				}
				// The issuer and its JWKS are the inbound trust anchor —
				// forging a principal only requires substituting the key set,
				// so both must be fetched over TLS (loopback exempt for dev).
				if err := requireSecureEndpoint("auth issuer", iss.Issuer); err != nil {
					return err
				}
				if iss.JWKSURI != "" {
					if err := requireSecureEndpoint("auth issuer jwksUri", iss.JWKSURI); err != nil {
						return err
					}
				}
				switch iss.Mode {
				case "", "direct", "exchange":
				default:
					return fmt.Errorf("auth: issuer %q: mode must be %q or %q", iss.Issuer, "direct", "exchange")
				}
			}
		default:
			return fmt.Errorf("auth: mode must be %q or %q", "disabled", "required")
		}
		if ema := c.Auth.EMA; ema != nil {
			if !c.AuthRequired() {
				return fmt.Errorf(`auth: ema requires mode "required"`)
			}
			if ema.IdpIssuer == "" || ema.SigningKeyRef == "" {
				return fmt.Errorf("auth: ema requires idpIssuer and signingKeyRef")
			}
			if err := requireSecureEndpoint("auth.ema idpIssuer", ema.IdpIssuer); err != nil {
				return err
			}
			if ema.IdpJWKSURI != "" {
				if err := requireSecureEndpoint("auth.ema idpJwksUri", ema.IdpJWKSURI); err != nil {
					return err
				}
			}
			if ema.TokenTTLSec < 0 || ema.TokenRateLimitPerMinute < 0 {
				return fmt.Errorf("auth: ema tokenTtlSec and tokenRateLimitPerMinute must be positive")
			}
		}
	}
	if c.Policy != nil {
		switch c.Policy.DefaultDecision {
		case "", "deny", "allow":
		default:
			return fmt.Errorf("policy: defaultDecision must be %q or %q", "deny", "allow")
		}
		for _, r := range c.Policy.Rules {
			if r.ID == "" {
				return fmt.Errorf("policy: every rule needs an id")
			}
			if len(r.Allow) == 0 {
				return fmt.Errorf("policy rule %q: allow must not be empty", r.ID)
			}
			for _, a := range r.Allow {
				if a.Server != "" && a.Server != "*" && !seenID[a.Server] {
					return fmt.Errorf("policy rule %q: unknown server %q", r.ID, a.Server)
				}
			}
		}
	}
	if c.Audit != nil {
		for _, s := range c.Audit.Sinks {
			switch s.Type {
			case "stdout":
			case "webhook":
				if s.URL == "" {
					return fmt.Errorf("audit: webhook sink requires a url")
				}
			default:
				return fmt.Errorf("audit: unknown sink type %q", s.Type)
			}
		}
	}
	return nil
}

func (u *Upstream) validateAuth() error {
	if u.Auth == nil {
		return nil
	}
	a := u.Auth
	switch a.Strategy {
	case "", "none", "passthrough":
	case "static":
		if a.SecretRef == "" {
			return fmt.Errorf("upstream %q: static auth requires secretRef", u.ID)
		}
	case "client-credentials":
		if a.TokenEndpoint == "" || a.ClientID == "" || a.ClientAuth == nil {
			return fmt.Errorf("upstream %q: client-credentials auth requires tokenEndpoint, clientId, clientAuth", u.ID)
		}
		if err := requireSecureEndpoint(fmt.Sprintf("upstream %q tokenEndpoint", u.ID), a.TokenEndpoint); err != nil {
			return err
		}
	case "token-exchange":
		if a.TokenEndpoint == "" || a.ClientID == "" || a.ClientAuth == nil || a.Audience == "" {
			return fmt.Errorf("upstream %q: token-exchange auth requires tokenEndpoint, clientId, clientAuth, audience", u.ID)
		}
		if err := requireSecureEndpoint(fmt.Sprintf("upstream %q tokenEndpoint", u.ID), a.TokenEndpoint); err != nil {
			return err
		}
	default:
		return fmt.Errorf("upstream %q: unknown auth strategy %q", u.ID, a.Strategy)
	}
	if a.ClientAuth != nil {
		switch a.ClientAuth.Type {
		case "client_secret_post", "client_secret_basic":
		default:
			return fmt.Errorf("upstream %q: clientAuth.type must be client_secret_post or client_secret_basic", u.ID)
		}
		if a.ClientAuth.SecretRef == "" {
			return fmt.Errorf("upstream %q: clientAuth.secretRef is required", u.ID)
		}
	}
	return nil
}

// Defaults, resolved at read time.

// PageSize returns the per-page bound for federated list results: the
// configured value, defaulting to 200; 0 means pagination is disabled
// (configured negative).
func (c *Config) PageSize() int {
	if c.Routing == nil || c.Routing.PageSize == 0 {
		return 200
	}
	if c.Routing.PageSize < 0 {
		return 0
	}
	return c.Routing.PageSize
}

// NamespaceSeparator returns the configured separator (default "__").
func (c *Config) NamespaceSeparator() string {
	if c.Routing != nil && c.Routing.NamespaceSeparator != "" {
		return c.Routing.NamespaceSeparator
	}
	return "__"
}

// MaxBodyBytes returns the request body cap (default 1 MiB).
func (c *Config) MaxBodyBytes() int64 {
	if c.Server != nil && c.Server.MaxBodyBytes > 0 {
		return c.Server.MaxBodyBytes
	}
	return 1 << 20
}

// MCPPath returns the path the gateway serves MCP on (default "/mcp").
func (c *Config) MCPPath() string {
	if c.Server != nil && c.Server.MCPPath != "" {
		return c.Server.MCPPath
	}
	return "/mcp"
}

// Endpoints returns the upstream's endpoint URLs: urls when set, else the
// single url. Never empty for a validated config.
func (u *Upstream) Endpoints() []string {
	if len(u.URLs) > 0 {
		return u.URLs
	}
	if u.URL != "" {
		return []string{u.URL}
	}
	return nil
}

// AuthRequired reports whether gateway authentication is enabled.
func (c *Config) AuthRequired() bool {
	return c.Auth != nil && c.Auth.Mode == "required"
}

// Passthrough reports whether the gateway runs in zero-copy passthrough mode
// (a single upstream with no namespace).
func (c *Config) Passthrough() bool {
	return len(c.Upstreams) == 1 && c.Upstreams[0].Namespace == ""
}
