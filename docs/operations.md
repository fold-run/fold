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
| `GET /health` | always | Pings every upstream concurrently (5 s internal budget); `503` when none is reachable. `/healthz` — the path through v1.4 — is a deprecated alias serving the identical response with a `Deprecation: true` header; the gateway logs once on its first use so you can find what still probes it. It is removed no sooner than the next major. The fan-out is single-flighted and reused for one second, so a poll loop against this unauthenticated endpoint costs at most one collection per second (a reload or discovery sync invalidates immediately). Detailed fields (URLs, owners, labels, error text) appear only when auth is disabled, so an unauthenticated caller on a public deployment cannot enumerate the federation. Multi-endpoint upstreams include a per-replica `endpoints` array with the balancer's rotation state. |
| `GET /metrics` | always | Prometheus exposition (below). |
| `GET /.well-known/oauth-protected-resource` | `auth.mode: required` | RFC 9728 resource metadata; announces the EMA extension when configured. |
| `GET /.well-known/jwks.json` | EMA configured | fold's minting key. |
| `GET /console/`, `GET /console/api/state`, `GET /console/api/auth` | `server.console.enabled` | The read-only fold console: embedded dashboard + MCP test console (tools, prompts, and resources). Static assets are open (they carry no data). The state API requires the same Bearer token as `/mcp` when auth is on and shares `/mcp`'s rate budgets; any valid principal (or, with `server.console.groups`, any allowlisted principal — others get an audited 403) sees the federation topology — URLs, owners, labels, each upstream's source (static/discovered) and credential-strategy *name*, plus shared-state/audit-sink/tracing facts — with raw connect errors reduced to a category and no secret values or Redis URL ever included; see [security-model.md](security-model.md#the-console-has-no-privileged-path). Test-console calls go through `/mcp` itself — governed and audited like any client. `/console/api/auth` is the deliberately unauthenticated sign-in hint (issuer, client id, scopes, resource — public SPA configuration only), read by the page before it has a token. |
| `POST /oauth/token` | EMA configured | The ID-JAG exchange endpoint — unauthenticated by design (the assertion is the credential) and rate limited (`auth.ema.tokenRateLimitPerMinute`). |

Every endpoint sits behind the `allowedHosts` check — health checkers and
scrapers must send an allowed `Host` header (see
[deploy.md](deploy.md#allowedhosts-and-health-probes)).

## Metrics

| Metric | Labels | Meaning |
|---|---|---|
| `fold_requests_total` | `method`, `outcome` | MCP requests through the gateway. Outcomes mirror audit: `ok`, `error`, `denied`, `rate_limited`, `budget_exhausted`, `upstream_down`. |
| `fold_request_duration_seconds` | `method` | End-to-end request duration histogram. |
| `fold_upstream_requests_total` | `upstream`, `outcome` | Proxied upstream calls. Outcomes: `ok`, `rate_limited`, `budget_exhausted`, `circuit_open`, `connect_error`, `error`. A JSON-RPC error from the upstream counts as `ok` — the upstream answered; its error passes through verbatim. |
| `fold_upstream_request_duration_seconds` | `upstream` | Upstream call duration histogram. |
| `fold_upstream_breaker_state` | `upstream` | 0 closed, 1 half-open, 2 open. |
| `fold_upstream_endpoint_healthy` | `upstream`, `endpoint` | Multi-endpoint upstreams only: 1 in rotation, 0 ejected after a connect failure (or by an active health probe). |
| `fold_request_upstream_calls` | — | Upstream invocations per downstream request. A federated list fans out to every upstream, so the histogram's tail is the width of the federation and its `1` bucket is named calls. Watch it to see what a cheap-looking client request really costs. |
| `fold_tenant_requests_total` | `tenant`, `outcome` | MCP requests by tenant, with the same outcomes as `fold_requests_total`. Only requests whose principal resolved to a tenant appear here — everything is counted in `fold_requests_total` regardless — so a rising `denied` or `rate_limited` rate on one tenant is a customer about to open a ticket. |
| `fold_tenant_upstream_calls_total` | `tenant` | Upstream invocations attributed to a tenant: the same unit `tenants[].budget` is charged in, so this is a monthly allowance being spent, live. Cardinality is the number of declared tenants — config-bounded, like `upstream`. |
| `fold_budget_degraded_total` | `scope` | Budget decisions taken per-instance because shared state was unreachable. Budgets fail open by design, so this is the signal that a fleet is not enforcing one allowance — **alert on any non-zero rate**. |
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
| `outcome` | `ok`, `error`, `denied`, `rate_limited`, `budget_exhausted`, `unauthenticated`, `upstream_down`, `forbidden`. |
| `tenant` | The tenant the principal resolved to, when `tenants` is configured. Empty means no tenant matched — not an error, just a caller governed by the gateway-wide rules. |
| `upstreamCalls` | Upstream invocations this request cost — the fan-out. Not always 1. |
| `itemsServed` | Items a list returned **after** per-principal policy filtering and pagination: the surface this caller was handed, not the federation's total. |
| `usage` | Counters the upstream published in its result `_meta`, verbatim. Absent means the upstream reported nothing — fold never synthesizes it. |
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
| `-32044` | Consumption budget exhausted for the period | Not transient within the period — the message names the reset instant. Do **not** treat it as a backoff delay; a monthly reset would mean sleeping for weeks. |
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

## Answering questions about a tenant

With `tenants` declared, a customer becomes a dimension you can query rather
than a filter you apply afterwards.

**"What did team A consume this month?"** —
`sum(increase(fold_tenant_upstream_calls_total{tenant="acme"}[30d]))`. The unit
is upstream invocations, the same one `tenants[].budget` is charged in, so this
number and the allowance are directly comparable. One `tools/list` fans out to
every upstream the tenant can see, which is why a list costs more than a call.

**"Is a customer being refused?"** —
`rate(fold_tenant_requests_total{tenant="acme",outcome!="ok"}[5m])`, and the
`outcome` label says which kind: `denied` is policy or the visibility subset,
`rate_limited` is the tenant's bucket (or a wider one), `budget_exhausted` is
the allowance. The three have different owners — a policy rule, a limit, and a
finance conversation — so alert on them separately.

**"Which tenant is loudest?"** —
`topk(5, sum by (tenant) (rate(fold_tenant_upstream_calls_total[5m])))`.
Untenanted traffic is deliberately absent from these series (it would otherwise
appear as a `tenant=""` line); it is counted in `fold_requests_total` like
everything else.

**In the audit stream**, every event a tenant's principals produce carries
`tenant`, including denials and rate-limit rejections, so the same questions
are answerable in the SIEM without reconciling against config.

**When an allowance runs out**, callers get `-32044` naming the tenant and the
reset instant, and the event carries `budget_exhausted`. Widening the allowance
is a reload, not a restart: `tenants[]` is snapshot state, and a tenant whose
budget block is unchanged keeps its accumulated count across the swap. Watch
`fold_budget_degraded_total{scope="tenant"}` — non-zero means shared state was
unreachable and each instance is enforcing its own copy of that allowance.

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
