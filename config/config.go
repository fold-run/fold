// Package config defines the fold gateway configuration schema, loading,
// and validation. It mirrors fold's single-JSON-document configuration:
// upstreams, auth, policy, audit, routing, and server sections.
package config

import (
	"encoding/json"
	"fmt"
	"net"
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
	Tracing   *Tracing       `json:"tracing,omitempty"`
	Discovery *Discovery     `json:"discovery,omitempty"`

	// Tenants group principals for governance. Reloadable: tenants change
	// when a customer signs up, and requiring a restart for that would push
	// operators toward a write control plane fold does not have.
	Tenants []Tenant `json:"tenants,omitempty"`
}

// Discovery enables dynamic upstream discovery: fold polls url for a JSON
// document {"upstreams": [...]} (same schema as the static upstreams
// section) and hot-swaps the discovered set into the federation alongside
// the statically configured upstreams. A document that fails validation —
// including id or namespace collisions with static upstreams — is rejected
// whole and the last good set keeps serving.
type Discovery struct {
	// URL of the discovery document. It decides where traffic routes and
	// where upstream credentials attach, so it must use https (loopback
	// exempt for development).
	URL string `json:"url"`

	IntervalMs int `json:"intervalMs,omitempty"` // poll interval; default 30000

	// BearerSecretRef names an environment variable whose value is sent as
	// a Bearer token when fetching the document.
	BearerSecretRef string `json:"bearerSecretRef,omitempty"`

	// AllowedAuthStrategies restricts the credential strategies discovered
	// upstreams may carry ("none" needs no listing). Whoever controls the
	// discovery source controls both an upstream's secretRef names and its
	// destination URL — an unrestricted source can point gateway-held
	// secrets (or, via passthrough, caller tokens) at any endpoint. Absent
	// → unrestricted; present → a document whose upstream carries any other
	// strategy is rejected whole, keeping the last good set.
	AllowedAuthStrategies []string `json:"allowedAuthStrategies,omitempty"`

	// AllowedSecretRefs restricts which environment variables discovered
	// upstreams may name in secretRef fields (upstream auth and client
	// auth). Absent → unrestricted; present → any other reference rejects
	// the document whole.
	AllowedSecretRefs []string `json:"allowedSecretRefs,omitempty"`

	// AllowedCredentialHosts restricts where a *credentialed* discovered
	// upstream may send secrets: both its endpoint hosts and its
	// tokenEndpoint host must match. Naming a secret is only half the
	// exposure — the destination is the other half, and a discovery source
	// chooses both. Entries are hostnames, optionally with a leading "*."
	// wildcard ("*.svc.cluster.local"); ports are ignored. Absent →
	// unrestricted; present → a violating document is rejected whole.
	// Upstreams with no credentials (strategy none/absent) are unaffected.
	AllowedCredentialHosts []string `json:"allowedCredentialHosts,omitempty"`

	// MinHealthCheckIntervalMs floors healthCheck.intervalMs on discovered
	// upstreams (default 1000). A discovery source could otherwise turn the
	// gateway into a probe flood against a host of its choosing.
	MinHealthCheckIntervalMs int `json:"minHealthCheckIntervalMs,omitempty"`
}

// MinHealthCheckIntervalResolved returns the floor applied to discovered
// upstreams' health-probe interval (default 1000 ms).
func (d *Discovery) MinHealthCheckIntervalResolved() int {
	if d.MinHealthCheckIntervalMs > 0 {
		return d.MinHealthCheckIntervalMs
	}
	return 1000
}

// HostAllowed reports whether host (possibly host:port) matches one of the
// patterns: an exact hostname, or "*.suffix" matching any subdomain.
func HostAllowed(patterns []string, host string) bool {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// ASCII-only folding: Unicode case folding maps characters like the
	// Kelvin sign to ASCII letters, which would let a non-ASCII host match
	// an ASCII pattern while resolvers dial the raw bytes.
	for i := range len(host) {
		if host[i] > 0x7F {
			return false
		}
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	for _, p := range patterns {
		p = strings.ToLower(p)
		if suffix, ok := strings.CutPrefix(p, "*."); ok {
			// Subdomains only — the apex is a separate grant.
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
			continue
		}
		if host == p {
			return true
		}
	}
	return false
}

// Tracing enables first-party OpenTelemetry spans (one server span per MCP
// request, one client span per upstream call), exported over OTLP/HTTP.
// Absent → fold only propagates the caller's W3C trace context.
type Tracing struct {
	// OTLPEndpoint is the collector's OTLP/HTTP base URL, e.g.
	// "http://otel-collector:4318". Plain http is allowed — collectors are
	// commonly cluster-local sidecars.
	OTLPEndpoint string `json:"otlpEndpoint"`

	ServiceName string `json:"serviceName,omitempty"` // default "fold"

	// SampleRatio samples traces fold roots itself at this rate (0 < r <= 1,
	// default 1). Parent-based: callers that sampled their trace stay
	// sampled regardless.
	SampleRatio float64 `json:"sampleRatio,omitempty"`
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

	// Budget caps total consumption of this upstream over a calendar period.
	// Distinct from RateLimit: a rate limit smooths a burst and forgets it,
	// a budget accumulates until the period rolls over.
	Budget *Budget `json:"budget,omitempty"`

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

// RateLimit is a sliding-window request budget over the trailing minute.
type RateLimit struct {
	RequestsPerMinute int `json:"requestsPerMinute"`
	// PerPrincipalPerMinute additionally caps each authenticated principal
	// on its own bucket, so one tenant's flood cannot consume the shared
	// budget and starve every other tenant. Server-level only; requires
	// auth (anonymous callers are governed by the global budget alone).
	PerPrincipalPerMinute int `json:"perPrincipalPerMinute,omitempty"`
}

// Budget caps total consumption over a calendar-aligned period that resets at
// the boundary — the "how much this month" question a rate limit cannot
// answer, since a sliding window forgets rather than accumulates.
//
// The unit is upstream invocations, not downstream requests: one tools/list
// fans out to every upstream in the federation, so counting client requests
// would price a list the same as a ping. See docs/design-consumption.md.
type Budget struct {
	// Period is the window: "hour", "day", or "month" (default). Boundaries
	// are UTC, so a fleet spanning zones agrees on which month it is.
	Period string `json:"period,omitempty"`
	// UpstreamCalls is the allowance. Absent or <= 0 means no budget.
	UpstreamCalls int64 `json:"upstreamCalls,omitempty"`
}

// ResolvedPeriod returns the configured period, defaulting to "month".
func (b *Budget) ResolvedPeriod() string {
	if b == nil || b.Period == "" {
		return "month"
	}
	return b.Period
}

// Allowance returns the configured allowance, or 0 for "no budget".
func (b *Budget) Allowance() int64 {
	if b == nil || b.UpstreamCalls < 0 {
		return 0
	}
	return b.UpstreamCalls
}

// validate checks a budget block. where names the owning section for errors.
func (b *Budget) validate(where string) error {
	if b == nil {
		return nil
	}
	switch b.Period {
	case "", "hour", "day", "month":
	default:
		return fmt.Errorf("%s: budget.period must be %q, %q, or %q", where, "hour", "day", "month")
	}
	// A zero allowance would silently mean "unlimited", which is the opposite
	// of what someone writing `"budget": {}` intends. Make them say a number.
	if b.UpstreamCalls <= 0 {
		return fmt.Errorf("%s: budget.upstreamCalls must be positive", where)
	}
	return nil
}

// Tenant names a set of principals and the governance that applies to them as
// a group. It is a label on identity, resolved from claims the IdP already
// asserts — never presented alongside a token, never a trust anchor. A tenant
// groups principals; it does not authenticate them. See
// docs/design-tenancy.md.
type Tenant struct {
	ID string `json:"id"`

	// Subjects selects the principals in this tenant, using the same shape
	// policy rules use. A principal belongs to at most one tenant; matching
	// more than one is a configuration error, refused at request time rather
	// than resolved by precedence.
	Subjects *PolicySubjects `json:"subjects,omitempty"`

	// Budget caps this tenant's consumption over a calendar period — the
	// dimension a per-upstream or server-wide budget cannot express.
	Budget *Budget `json:"budget,omitempty"`

	// RateLimit gives the tenant one shared bucket. Distinct from
	// server.rateLimit.perPrincipalPerMinute, which gives each *person* a
	// bucket: ten agents on one team get ten allowances there and one here.
	RateLimit *RateLimit `json:"rateLimit,omitempty"`

	// Upstreams optionally bounds which upstreams this tenant may see at
	// all, by id. Evaluated before policy, which remains the authority on
	// what may be invoked. Empty means every upstream.
	Upstreams []string `json:"upstreams,omitempty"`
}

// validateTenants checks tenant declarations.
//
// Note what it does not do: prove that no principal can match two tenants.
// That is not statically decidable for the selectors fold supports — two
// group-based tenants "overlap" whenever some principal could hold a group
// from each, which is nearly always, so rejecting on it would refuse almost
// every real document. What is checkable is caught here (duplicate ids,
// identical selectors, references to upstreams that do not exist); genuine
// ambiguity is caught at resolution, against a real principal, and refused
// there. See docs/design-tenancy.md.
func (c *Config) validateTenants() error {
	seen := map[string]bool{}
	fingerprints := map[string]string{}
	for i := range c.Tenants {
		t := &c.Tenants[i]
		if !idRE.MatchString(t.ID) {
			return fmt.Errorf("tenant %q: id must be lowercase alphanumeric + hyphens", t.ID)
		}
		if seen[t.ID] {
			return fmt.Errorf("tenant %q: duplicate id", t.ID)
		}
		seen[t.ID] = true

		// A tenant with no selector would match every principal, which is a
		// footgun rather than a shorthand: it silently captures callers the
		// operator never meant to place.
		if t.Subjects == nil {
			return fmt.Errorf("tenant %q: subjects is required — a tenant with no selector would capture every caller", t.ID)
		}
		// Identical selectors are always ambiguous, so they are worth
		// refusing statically: it is the copy-paste mistake, and no principal
		// could ever resolve between them.
		fp, err := json.Marshal(t.Subjects)
		if err != nil {
			return fmt.Errorf("tenant %q: %w", t.ID, err)
		}
		if other, dup := fingerprints[string(fp)]; dup {
			return fmt.Errorf("tenant %q: identical subjects to tenant %q — no principal could resolve between them", t.ID, other)
		}
		fingerprints[string(fp)] = t.ID

		if err := t.Budget.validate(fmt.Sprintf("tenant %q", t.ID)); err != nil {
			return err
		}
		if t.RateLimit != nil && t.RateLimit.PerPrincipalPerMinute != 0 {
			return fmt.Errorf("tenant %q: rateLimit.perPrincipalPerMinute is server-level only; a tenant's bucket is shared by its principals", t.ID)
		}
		for _, id := range t.Upstreams {
			if !c.hasUpstream(id) {
				return fmt.Errorf("tenant %q: upstreams references unknown upstream %q", t.ID, id)
			}
		}
	}
	return nil
}

// hasUpstream reports whether id names a configured upstream.
func (c *Config) hasUpstream(id string) bool {
	for i := range c.Upstreams {
		if c.Upstreams[i].ID == id {
			return true
		}
	}
	return false
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

	// MaxItems caps how many list items this rule may make visible in one
	// response — a guardrail against handing an agent a thousand tools, which
	// is context an operator pays for on every turn.
	//
	// It is a bound, not a curation: fold drops whatever falls past the cap in
	// merge order, because it has no notion of which tools matter and
	// inventing one would be the semantic tool selection this project
	// declines. A truncated list says so, in the result _meta, the audit
	// event, and a metric — a cap that hid capability silently would be worse
	// than no cap. Absent or 0 means no limit.
	MaxItems int `json:"maxItems,omitempty"`
}

// PolicySubjects matches principals by group membership and/or subject,
// optionally scoped to specific token issuers and gated on token claims.
// Because subjects, groups, and claims are only meaningful within an
// issuer, scope rules to an issuer whenever more than one is trusted.
type PolicySubjects struct {
	Groups  []string `json:"groups,omitempty"`
	Subs    []string `json:"subs,omitempty"`
	Issuers []string `json:"issuers,omitempty"`

	// Claims gates the rule on verified token claims (attribute-based
	// access control): every entry must match — the claim equals the value,
	// or, when the token claim is an array, contains it. Values must be
	// JSON scalars (string, number, bool). Combines with subs/groups as an
	// additional requirement, like issuers.
	Claims map[string]any `json:"claims,omitempty"`
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
	Type    string            `json:"type"` // "stdout" | "webhook" | "file" | "otlp-logs"
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`

	// Path is the file a "file" sink appends to, one JSON event per line.
	Path string `json:"path,omitempty"`
	// MaxSizeMb rotates the file once it exceeds this size (default 100).
	// Rotation renames in place — audit.jsonl → audit.jsonl.1 → .2 — so a
	// tail follows the live name.
	MaxSizeMb int `json:"maxSizeMb,omitempty"`
	// MaxFiles bounds how many rotated files are kept, oldest deleted first
	// (default 5). A gateway that fills a disk with its own audit trail has
	// found a novel way to stop serving.
	MaxFiles int `json:"maxFiles,omitempty"`

	// Retry governs delivery for sinks that can fail transiently — today the
	// webhook. Absent means the defaults, not "no retry": a receiver
	// restarting is the ordinary case, and losing the events it was down for
	// is the thing audit cannot afford.
	Retry *AuditRetry `json:"retry,omitempty"`

	// DeadLetterPath is where events go when delivery is finally given up
	// on, one JSON event per line. Absent, exhausted events are counted and
	// logged but not kept — set it wherever the audit trail is load-bearing.
	DeadLetterPath string `json:"deadLetterPath,omitempty"`
}

// AuditRetry tunes redelivery for a failing sink.
type AuditRetry struct {
	// MaxAttempts includes the first try (default 4, minimum 1).
	MaxAttempts int `json:"maxAttempts,omitempty"`
	// InitialBackoffMs is the first delay, doubling per attempt with jitter
	// (default 500).
	InitialBackoffMs int `json:"initialBackoffMs,omitempty"`
	// MaxBackoffMs caps the delay (default 30000).
	MaxBackoffMs int `json:"maxBackoffMs,omitempty"`
}

// validate checks a retry block and fills nothing in — the defaults live
// where the sink is built, so an absent block and an explicit one that
// matches the defaults behave identically.
func (r *AuditRetry) validate() error {
	if r == nil {
		return nil
	}
	if r.MaxAttempts < 0 || r.InitialBackoffMs < 0 || r.MaxBackoffMs < 0 {
		return fmt.Errorf("audit: retry values must not be negative")
	}
	if r.MaxBackoffMs > 0 && r.InitialBackoffMs > r.MaxBackoffMs {
		return fmt.Errorf("audit: retry.initialBackoffMs (%d) exceeds maxBackoffMs (%d)",
			r.InitialBackoffMs, r.MaxBackoffMs)
	}
	return nil
}

// Routing tunes name federation.
type Routing struct {
	NamespaceSeparator string `json:"namespaceSeparator,omitempty"` // default "__"

	// PageSize bounds federated list results (tools/prompts/resources/
	// templates/tasks) per page. 0 uses the default (200); negative disables
	// pagination and returns the full merged list as a single page.
	PageSize int `json:"pageSize,omitempty"`
}

// ServerSection configures the gateway's own HTTP server.
type ServerSection struct {
	MCPPath      string     `json:"mcpPath,omitempty"`      // default "/mcp"
	AllowedHosts []string   `json:"allowedHosts,omitempty"` // default localhost set; ["*"] disables
	RateLimit    *RateLimit `json:"rateLimit,omitempty"`    // global, across all upstreams

	// Budget caps total consumption across every upstream over a calendar
	// period. Like the rest of this section it is construction-wired: Reload
	// rejects a change to it, so a budget cannot be widened by editing config
	// under a running gateway.
	Budget *Budget `json:"budget,omitempty"`

	// MaxBodyBytes caps request body size (413 beyond it), bounding the
	// memory one request can pin. Default 1 MiB.
	MaxBodyBytes int64 `json:"maxBodyBytes,omitempty"`

	// SessionIdleTimeoutMs closes a downstream MCP session after this long
	// without a request from its client. Ending a session with DELETE is
	// optional in the protocol and clients routinely reconnect without it;
	// an unexpired session pins gateway memory — and the upstream
	// subscriptions it holds — forever. Default 30 minutes; negative
	// disables expiry (the pre-1.11 behavior). Construction-wired like the
	// rest of this section: Reload rejects a change to it.
	SessionIdleTimeoutMs int `json:"sessionIdleTimeoutMs,omitempty"`

	// RedisURL shares cache, rate-limit, and circuit-breaker state across
	// gateway instances (redis:// URL). Defaults to the REDIS_URL
	// environment variable; absent → in-process state.
	RedisURL string `json:"redisUrl,omitempty"`

	// MetricsAddr moves /metrics to its own listener ("host:port", or
	// ":9090" for every interface). Absent — the default — leaves /metrics on
	// the main mux, behind the same Host allowlist as everything else.
	//
	// Why moving it is the safer arrangement rather than exempting the path:
	// a scrape carries upstream ids, namespaces, tenant ids, and the endpoint
	// URLs of multi-endpoint upstreams. On the main mux, DNS-rebinding checks
	// are what keep a browser from reading that — and the price is that a
	// scraper arriving under any other name is answered 403, which is why a
	// ServiceMonitor cannot scrape a pod IP. A separate listener settles
	// both: it is not an origin a victim's browser can be steered to, so it
	// needs no Host allowlist, and a scraper may address it however it likes.
	// Bind it to an internal interface and keep it off the public network —
	// that network scope is what protects it.
	//
	// Construction-wired like the rest of this section: Reload rejects a
	// change to it.
	MetricsAddr string `json:"metricsAddr,omitempty"`

	// Introspection serves the gateway's read-only HTTP APIs: /api/federation
	// (the federation snapshot — upstream health, policy shape, audit sinks,
	// discovery status, the viewer's tenant governance) and /api/auth-hint
	// (the unauthenticated sign-in hint). Separate from `console` because the
	// API and the page are separate surfaces: an operator may want the data
	// without serving a browser page at all, and the API stopped being the
	// console's private detail when the console became separately versioned.
	// Off by default. See docs/design-console.md.
	//
	// Construction-wired like the rest of this section: Reload rejects a
	// change to it.
	Introspection *Introspection `json:"introspection,omitempty"`

	// Console enables the read-only fold console page at /console: an
	// observability dashboard plus an MCP test console that talks to the
	// gateway's own /mcp endpoint (fully governed — policy, rate limits,
	// and audit apply to console traffic like any other client's). The page
	// renders what /api/federation reports, so it requires
	// introspection.enabled. Off by default. See docs/design-console.md.
	Console *Console `json:"console,omitempty"`
}

// Introspection configures the read-only HTTP APIs. Like the rest of the
// server section it is construction-wired: changing it requires a restart,
// not a hot reload.
type Introspection struct {
	Enabled bool `json:"enabled"`

	// Groups restricts who may read /api/federation: when set, an
	// authenticated principal must carry at least one of these groups
	// (per its issuer's groupsClaim) or the API answers an audited 403.
	// Requires auth.mode "required". Absent → any valid principal may
	// read. Console static assets are never gated — they carry no data.
	// Group names are only unique within an issuer (the same caveat as
	// policy rules), so keep the list meaningful across every trusted
	// issuer.
	Groups []string `json:"groups,omitempty"`
}

// Console configures the read-only console page. Like the rest of the server
// section it is construction-wired: changing it requires a restart, not a
// hot reload.
type Console struct {
	Enabled bool `json:"enabled"`

	// OAuth lets the console sign users in with Authorization Code +
	// PKCE instead of a pasted token. Requires auth.mode "required".
	OAuth *ConsoleOAuth `json:"oauth,omitempty"`
}

// ConsoleOAuth configures the console's browser sign-in. The console is a
// public OAuth client: no secret exists, PKCE is the proof. Register the
// gateway's console URL ({origin}/console/) as the redirect URI at the IdP.
type ConsoleOAuth struct {
	// ClientID is the public client id registered at the IdP for the
	// console. Client ids are not secrets — every SPA ships one.
	ClientID string `json:"clientId"`

	// Issuer selects which trusted issuer the console signs in against.
	// Must match a configured auth issuer with mode "direct" (an
	// "exchange" issuer's tokens are ID-JAGs, not presentable access
	// tokens). Default: the first direct issuer.
	Issuer string `json:"issuer,omitempty"`

	// Scopes requested at authorization (default none beyond what the
	// IdP grants implicitly; the resource/audience comes from
	// auth.resource via RFC 8707).
	Scopes []string `json:"scopes,omitempty"`
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
		if err := u.Budget.validate(fmt.Sprintf("upstream %q", u.ID)); err != nil {
			return err
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
	if c.Server != nil && c.Server.MetricsAddr != "" {
		// Checked here rather than at bind time, so a typo fails
		// `fold --validate` and the chart's config-validating init container
		// instead of a rollout.
		if _, _, err := net.SplitHostPort(c.Server.MetricsAddr); err != nil {
			return fmt.Errorf(`server: metricsAddr must be host:port (":9090", "127.0.0.1:9090"): %w`, err)
		}
	}
	if c.Server != nil && c.Server.MCPPath != "" {
		// The gateway registers these on the same mux. A collision is not a
		// degraded config: http.ServeMux panics on a duplicate pattern, so
		// the process would die at startup with no useful message. Caught
		// here it fails `fold --validate` and the chart's config-validating
		// init container instead of a rollout.
		reserved := []string{
			"/health", "/metrics", "/console", "/console/",
			"/api/federation", "/api/auth-hint", "/oauth/token",
		}
		for _, p := range reserved {
			if c.Server.MCPPath == p {
				return fmt.Errorf("server: mcpPath %q is reserved by the gateway", p)
			}
		}
		if strings.HasPrefix(c.Server.MCPPath, "/.well-known/") {
			return fmt.Errorf("server: mcpPath %q is reserved by the gateway", c.Server.MCPPath)
		}
	}
	if c.Server != nil {
		if err := c.Server.Budget.validate("server"); err != nil {
			return err
		}
	}
	if err := c.validateTenants(); err != nil {
		return err
	}
	if c.Server != nil && c.Server.Introspection != nil && len(c.Server.Introspection.Groups) > 0 {
		for _, gr := range c.Server.Introspection.Groups {
			if gr == "" {
				return fmt.Errorf("server: introspection.groups entries must not be empty")
			}
		}
		// A viewer allowlist matches against verified group claims; without
		// mandatory auth there is no principal to match, and the API would
		// be reachable by anyone anyway.
		if !c.AuthRequired() {
			return fmt.Errorf(`server: introspection.groups requires auth.mode %q`, "required")
		}
	}
	if c.ConsoleEnabled() && !c.IntrospectionEnabled() {
		// The page renders what /api/federation reports and has no other
		// data source, so serving it alone is a configuration that cannot
		// work rather than a degraded one.
		return fmt.Errorf("server: console.enabled requires introspection.enabled — the console page reads /api/federation")
	}
	if c.Server != nil && c.Server.Console != nil && c.Server.Console.OAuth != nil {
		oa := c.Server.Console.OAuth
		if oa.ClientID == "" {
			return fmt.Errorf("server: console.oauth requires clientId")
		}
		// The PKCE redirect URI is {origin}/console/, so sign-in against a
		// gateway serving no console page is a flow that cannot complete —
		// advertised over an API that would hand out the client id anyway.
		if !c.ConsoleEnabled() {
			return fmt.Errorf("server: console.oauth requires console.enabled — there is no page to return to")
		}
		// Sign-in mints tokens the gateway must then accept, so the flow
		// only means something with mandatory auth and a trusted direct
		// issuer — an "exchange" issuer's tokens are ID-JAGs, which cannot
		// be presented to /mcp or the console directly.
		if !c.AuthRequired() {
			return fmt.Errorf(`server: console.oauth requires auth.mode %q`, "required")
		}
		if _, err := c.ConsoleOAuthIssuer(); err != nil {
			return err
		}
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
			if r.MaxItems < 0 {
				return fmt.Errorf("policy rule %q: maxItems must not be negative", r.ID)
			}
			for _, a := range r.Allow {
				if a.Server != "" && a.Server != "*" && !seenID[a.Server] {
					return fmt.Errorf("policy rule %q: unknown server %q", r.ID, a.Server)
				}
			}
			if r.Subjects != nil {
				for k, v := range r.Subjects.Claims {
					if k == "" {
						return fmt.Errorf("policy rule %q: claims keys must not be empty", r.ID)
					}
					switch v.(type) {
					case string, float64, bool:
					default:
						// Arrays and objects would need deep-equality
						// semantics nobody can predict from a config file;
						// scalar-or-membership is the whole contract.
						return fmt.Errorf("policy rule %q: claim %q must be a JSON scalar (string, number, or bool)", r.ID, k)
					}
				}
			}
		}
	}
	if c.Discovery != nil {
		if err := requireSecureEndpoint("discovery url", c.Discovery.URL); err != nil {
			return err
		}
		if c.Discovery.IntervalMs < 0 {
			return fmt.Errorf("discovery: intervalMs must be positive")
		}
		for _, s := range c.Discovery.AllowedAuthStrategies {
			switch s {
			case "none", "static", "passthrough", "client-credentials", "token-exchange":
			default:
				return fmt.Errorf("discovery: allowedAuthStrategies: unknown strategy %q", s)
			}
		}
		for _, ref := range c.Discovery.AllowedSecretRefs {
			if ref == "" {
				return fmt.Errorf("discovery: allowedSecretRefs entries must not be empty")
			}
		}
		for _, pat := range c.Discovery.AllowedCredentialHosts {
			if pat == "" || pat == "*" || (strings.HasPrefix(pat, "*") && !strings.HasPrefix(pat, "*.")) {
				return fmt.Errorf("discovery: allowedCredentialHosts: %q is not a hostname or \"*.suffix\" pattern", pat)
			}
		}
		// Granting a credential without bounding its destination is the
		// exfiltration path the allowlists exist to close: the two halves
		// are only meaningful together.
		credentialed := len(c.Discovery.AllowedSecretRefs) > 0
		for _, s := range c.Discovery.AllowedAuthStrategies {
			if s != "none" {
				credentialed = true
			}
		}
		if credentialed && c.Discovery.AllowedCredentialHosts == nil {
			return fmt.Errorf("discovery: allowedCredentialHosts is required when allowedAuthStrategies or allowedSecretRefs permits credentials — otherwise the discovery source chooses where those credentials are sent")
		}
	}
	if c.Tracing != nil {
		parsed, err := url.Parse(c.Tracing.OTLPEndpoint)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("tracing: otlpEndpoint must be an absolute http(s) URL")
		}
		if c.Tracing.SampleRatio < 0 || c.Tracing.SampleRatio > 1 {
			return fmt.Errorf("tracing: sampleRatio must be between 0 and 1")
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
			case "file":
				if s.Path == "" {
					return fmt.Errorf("audit: file sink requires a path")
				}
			case "otlp-logs":
				// The collector's base URL; a bare one gets OTLP's /v1/logs
				// path, matching the tracing section's convention.
				if s.URL == "" {
					return fmt.Errorf("audit: otlp-logs sink requires a url")
				}
				// Scheme-checked rather than IsAbs: url.Parse reads
				// "collector:4318" as scheme "collector", which IsAbs accepts
				// and the exporter then quietly replaces with its own default
				// endpoint — sending the audit trail somewhere nobody asked
				// for. Same check the tracing section makes.
				parsed, err := url.Parse(s.URL)
				if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
					return fmt.Errorf("audit: otlp-logs url must be an absolute http(s) URL")
				}
			default:
				return fmt.Errorf("audit: unknown sink type %q", s.Type)
			}
			if s.MaxSizeMb < 0 || s.MaxFiles < 0 {
				return fmt.Errorf("audit: maxSizeMb and maxFiles must not be negative")
			}
			if err := s.Retry.validate(); err != nil {
				return err
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

// SessionIdleTimeoutMs returns how long a downstream MCP session may sit
// idle before the gateway closes it, in milliseconds. Default 30 minutes;
// 0 — from a negative config value — means sessions never expire.
func (c *Config) SessionIdleTimeoutMs() int {
	if c.Server != nil && c.Server.SessionIdleTimeoutMs != 0 {
		return max(c.Server.SessionIdleTimeoutMs, 0)
	}
	return 30 * 60 * 1000
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

// IntrospectionEnabled reports whether the read-only APIs (/api/federation,
// /api/auth-hint) are served.
func (c *Config) IntrospectionEnabled() bool {
	return c.Server != nil && c.Server.Introspection != nil && c.Server.Introspection.Enabled
}

// IntrospectionGroups returns the viewer allowlist for /api/federation.
// Empty means any valid principal may read it.
func (c *Config) IntrospectionGroups() []string {
	if c.Server == nil || c.Server.Introspection == nil {
		return nil
	}
	return c.Server.Introspection.Groups
}

// ConsoleEnabled reports whether the read-only console page is served.
func (c *Config) ConsoleEnabled() bool {
	return c.Server != nil && c.Server.Console != nil && c.Server.Console.Enabled
}

// ConsoleOAuthIssuer resolves the trusted issuer the console's PKCE
// sign-in uses: console.oauth.issuer when set (which must name a
// configured direct-mode issuer), else the first direct-mode issuer.
// Errors when console.oauth is absent or no direct issuer qualifies.
func (c *Config) ConsoleOAuthIssuer() (*Issuer, error) {
	if c.Server == nil || c.Server.Console == nil || c.Server.Console.OAuth == nil || c.Auth == nil {
		return nil, fmt.Errorf("server: console.oauth is not configured")
	}
	want := c.Server.Console.OAuth.Issuer
	for i := range c.Auth.Issuers {
		iss := &c.Auth.Issuers[i]
		direct := iss.Mode == "" || iss.Mode == "direct"
		if !direct {
			continue
		}
		if want == "" || iss.Issuer == want {
			return iss, nil
		}
	}
	if want != "" {
		return nil, fmt.Errorf("server: console.oauth.issuer %q does not match a trusted direct-mode auth issuer", want)
	}
	return nil, fmt.Errorf(`server: console.oauth requires at least one direct-mode issuer (every configured issuer is mode "exchange")`)
}

// Passthrough reports whether the gateway runs in zero-copy passthrough mode
// (a single upstream with no namespace).
func (c *Config) Passthrough() bool {
	return len(c.Upstreams) == 1 && c.Upstreams[0].Namespace == ""
}
