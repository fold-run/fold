# Defaults review (v1.0)

Freezing the v1 contract freezes the defaults: changing one after v1.0 is a
breaking change even though no field name moves. This is the pre-1.0 review
of every resolved default — each is a decision on record, not an accident of
implementation order. Verdict for all: **keep**.

## Security posture

| Default | Value | Rationale |
|---|---|---|
| `auth.mode` | `disabled` | Paired with the CLI's loopback bind (below): the out-of-the-box gateway is private to the machine, so the quick start works without an IdP. Exposure requires the deliberate act of passing `--host 0.0.0.0`, and production configs set `mode: required`. Secure-by-default network posture instead of mandatory auth. |
| `--host` | `127.0.0.1` | The other half of the pair. Never widen this default. |
| `server.allowedHosts` | localhost set (only when unset) | DNS-rebinding protection that matches the loopback default. An explicit allowlist replaces — never extends — the localhost seed. |
| `server.maxBodyBytes` | 1 MiB | Bounds memory per request. Deliberately conservative; workloads shipping large base64 content raise it knowingly. |
| `auth.ema.tokenTtlSec` | 600 | Short-lived minted tokens; refresh is cheap (the ID-JAG is re-presented). |
| `auth.ema.tokenRateLimitPerMinute` | 600 | Anti-amplification on the unauthenticated token endpoint. |
| `server.console.enabled` | `false` (added in v1.2) | No new surface unless asked for: the console's static assets serve unauthenticated by design, so exposing them is an operator's deliberate choice, not a default. |
| `issuer.jwksUri` | `{issuer}/.well-known/jwks.json` | Common convention; IdPs that differ (e.g. Okta org servers) set it explicitly. A guess, but a configurable one. |
| `issuer.groupsClaim` | `groups` | Okta's name; Entra/Auth0 set their own. |

## Protocol and federation

| Default | Value | Rationale |
|---|---|---|
| `upstream.protocol` | `session` | The deliberate divergence from the SDK's own preference: only sessionful connections carry server-initiated traffic (sampling, elicitation, logging, progress) back through the gateway. `auto` is the opt-out, not the default. |
| `routing.namespaceSeparator` | `__` | Survives every namespace character; validation rejects ambiguous separators. |
| `routing.pageSize` | 200 | Large enough that most federations are one page; small enough to bound response size. Negative opts out. |
| `upstream.cacheTtlMs` | 30000 | A backstop only — `list_changed` notifications invalidate immediately; the TTL covers upstreams that never send them. |

## Resilience

| Default | Value | Rationale |
|---|---|---|
| `timeouts` | connect 5 s, request 60 s, streamIdle 120 s | Request 60 s accommodates slow tools; connect 5 s fails over quickly (multi-endpoint upstreams try the next replica within the same attempt). |
| `circuitBreaker` | 5 failures / 30 s half-open | Conventional values; also the endpoint pool's cooldown, by design (one "retry the unhealthy thing after" knob). |
| `rateLimit` (global, per-upstream) | none | The gateway must never throttle by surprise; limits are an operator's policy, opted into. |
| `healthCheck` | absent (passive) | Active probing costs a connect per endpoint per interval; passive ejection + cooldown is free and correct. Opt in. |
| `discovery.intervalMs` | 30000 | Registry churn is minutes-scale; 30 s balances freshness against load on the source. |
| `discovery.allowedAuthStrategies` / `allowedSecretRefs` | absent (unrestricted) | Compatibility with pre-hardening discovery deployments; restricting by default post-v1.0 would break them. The producer (`fold-discovery`) is the inverse — default-deny — because it shipped with the hardening. Set the gateway allowlists whenever the discovery source is not operated by the gateway's operators. |
| `server.redisUrl` | unset (in-process state) | Single instances need no infrastructure; fleets opt in. Redis outages fail open, bounded 500 ms per operation. |

## Observability

| Default | Value | Rationale |
|---|---|---|
| `tracing` | absent (propagation-only) | First-party spans are opt-in; W3C trace propagation is always on and free. |
| `tracing.sampleRatio` | 1.0 | An operator who configures tracing wants the traces; parent-based, so callers' sampling decisions are honored either way. |
| `tracing.serviceName` | `fold` | — |
| `--log-level` / `--log-format` | `info` / `text` | Human-first on a terminal; `json` for collectors. |

Non-configurable behaviors reviewed alongside (bridged-session idle sweep at
5 minutes, SSE-header hang timeout 3 s, discovery document cap 4 MiB, JWKS
fetch bounds) are implementation details, not contract — they may be tuned
or made configurable in any release.
