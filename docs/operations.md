# Operating fold

Day-2 reference: the endpoints a running gateway serves, every metric and
audit field it emits, what the error codes mean to client teams, and how to
observe reloads and discovery. Deployment shapes (Docker, Helm, systemd,
TLS, Redis) are in [deploy.md](deploy.md); configuration reference is in the
[README](../README.md#configuration).

## HTTP endpoints

| Endpoint | When | Notes |
|---|---|---|
| `POST/GET /mcp` | always | The MCP endpoint (path configurable via `server.mcpPath`). |
| `GET /healthz` | always | Pings every upstream concurrently (5 s internal budget); `503` when none is reachable. Detailed fields (URLs, owners, labels, error text) appear only when auth is disabled, so an unauthenticated caller on a public deployment cannot enumerate the federation. Multi-endpoint upstreams include a per-replica `endpoints` array with the balancer's rotation state. |
| `GET /metrics` | always | Prometheus exposition (below). |
| `GET /.well-known/oauth-protected-resource` | `auth.mode: required` | RFC 9728 resource metadata; announces the EMA extension when configured. |
| `GET /.well-known/jwks.json` | EMA configured | fold's minting key. |
| `GET /console/`, `GET /console/api/state` | `server.console.enabled` | The read-only fold console: embedded dashboard + MCP test console. Static assets are open (they carry no data). The state API requires the same Bearer token as `/mcp` when auth is on and shares `/mcp`'s rate budgets; any valid principal sees the federation topology (URLs, owners, labels), with raw connect errors reduced to a category — see [security-model.md](security-model.md#the-console-has-no-privileged-path). Test-console calls go through `/mcp` itself — governed and audited like any client. |
| `POST /oauth/token` | EMA configured | The ID-JAG exchange endpoint — unauthenticated by design (the assertion is the credential) and rate limited (`auth.ema.tokenRateLimitPerMinute`). |

Every endpoint sits behind the `allowedHosts` check — health checkers and
scrapers must send an allowed `Host` header (see
[deploy.md](deploy.md#allowedhosts-and-health-probes)).

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `fold_requests_total` | `method`, `outcome` | MCP requests through the gateway. Outcomes mirror audit: `ok`, `error`, `denied`, `rate_limited`, `upstream_down`. |
| `fold_request_duration_seconds` | `method` | End-to-end request duration histogram. |
| `fold_upstream_requests_total` | `upstream`, `outcome` | Proxied upstream calls. Outcomes: `ok`, `rate_limited`, `circuit_open`, `connect_error`, `error`. A JSON-RPC error from the upstream counts as `ok` — the upstream answered; its error passes through verbatim. |
| `fold_upstream_request_duration_seconds` | `upstream` | Upstream call duration histogram. |
| `fold_upstream_breaker_state` | `upstream` | 0 closed, 1 half-open, 2 open. |
| `fold_upstream_endpoint_healthy` | `upstream`, `endpoint` | Multi-endpoint upstreams only: 1 in rotation, 0 ejected after a connect failure (or by an active health probe). |
| `fold_http_rejections_total` | `reason` | Requests refused before the MCP layer: `body_too_large`, `forbidden_host`, `forbidden_origin`, `unauthenticated`, `rate_limited`, `oauth_token_rate_limited`. |
| `fold_discovery_syncs_total` | `outcome` | Discovery polls: `applied`, `unchanged`, `rejected` (document failed parse or merged validation), `error` (fetch failed). |
| `fold_build_info` | `version` | Always 1. |

Plus the standard Go process/runtime collectors. Alerting starters:
`fold_upstream_breaker_state == 2` sustained, any `fold_http_rejections_total`
rate spike, and — with discovery — any `rejected`/`error` sync outcomes.

## Audit events

One JSON event per terminal response — including 401s, 403-equivalents, and
429s — to the configured sinks (`stdout`, `webhook`; delivery is
asynchronous and batched, never adding request latency). Fields:

| Field | Meaning |
|---|---|
| `time` | UTC timestamp. |
| `principal`, `issuer` | Verified subject and token issuer; absent when auth is disabled. |
| `method` | MCP method (`tools/call`, …) or `http` for pre-MCP rejections. |
| `name` | Namespaced tool/prompt name or resource URI. |
| `upstream` | Routed upstream id. |
| `decision`, `ruleId` | Policy outcome (`allow`/`deny`) and the matching rule. |
| `outcome` | `ok`, `error`, `denied`, `rate_limited`, `unauthenticated`, `upstream_down`, `forbidden`. |
| `error` | Error text, when the request failed. |
| `latencyMs` | End-to-end latency. |

## Gateway error codes

What client teams see when the gateway itself refuses a request (upstream
errors pass through verbatim):

| Code | Meaning | Client action |
|---|---|---|
| `-32040` | Per-upstream rate limit exceeded | Back off (message includes retry hint). |
| `-32041` | Upstream unavailable (circuit open / unreachable / all down) | Retry later; transient. |
| `-32042` | Policy denied the invocation | Not transient — the principal lacks a grant. |
| `-32043` | Name resolves to no configured namespace | Refetch the tool list. |
| `-32002` | Task id not owned by any upstream | The task is unknown or belongs to another principal. |
| `-32602` | Invalid or expired list cursor | Restart the list from the beginning. |

HTTP-level refusals: `401` (missing/invalid token, with a
`WWW-Authenticate` challenge), `403` (host/origin not allowed), `413` (body
over `server.maxBodyBytes`), `429` (+ `Retry-After`).

## Observing reloads and discovery

Reloads (SIGHUP, `--watch`, `Reload`) log `configuration reloaded` with the
upstream, discovered, and retired counts — or an error naming what was
rejected (`reload: the auth section cannot change without a restart`,
validation failures) while the old configuration keeps serving. Clients
receive `list_changed` after every successful swap.

Discovery logs state transitions rather than every poll: `discovery
applied` with the upstream count, `discovery fetch failed` once per outage
(with `discovery source recovered` on the way back), and `discovery
document rejected`/`malformed` once per bad document. The
`fold_discovery_syncs_total` outcomes carry the per-poll record.

Active health probes (`healthCheck.intervalMs`) log only transitions:
`health probe ejected endpoint` and `health probe restored endpoint`.

## Tracing

W3C trace context propagates to upstream calls unconditionally. With the
`tracing` section configured, fold also emits its own spans over OTLP/HTTP:
a server span per MCP request named by method, carrying `mcp.method`,
`mcp.name`, `fold.upstream`, `fold.outcome`, `fold.policy.decision`,
`fold.policy.rule`, and `enduser.id` (the same terminal fields as the audit
event), and a client span per upstream call (`upstream <id>`) closed with
its guard outcome. Export is batched off the request path; `Close`/shutdown
flushes with a 3 s bound so a dead collector cannot hang termination.

## Log streams

Operational logs go to stderr via `log/slog` (`--log-format text|json`,
`--log-level debug|info|warn|error`). Per-request accounting deliberately
stays out of the log stream — that is what metrics and the audit sinks are
for. Startup, upstream connect failures and session drops, breaker transitions,
refused cross-host redirects, reload results, discovery and probe
transitions, and shutdown are the events to expect at `info`/`warn`
(successful upstream connects log at `debug`).
