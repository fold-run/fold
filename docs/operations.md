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
| `GET /health` | always | Pings every upstream concurrently (5 s internal budget); `503` when none is reachable. `/healthz` — the path through v1.4, kept as a deprecated alias through v1.8 — was **removed in v1.9**; point probes at `/health`. The fan-out is single-flighted and reused for one second, so a poll loop against this unauthenticated endpoint costs at most one collection per second (a reload or discovery sync invalidates immediately). Detailed fields (URLs, owners, labels, error text) appear only when auth is disabled, so an unauthenticated caller on a public deployment cannot enumerate the federation. Multi-endpoint upstreams include a per-replica `endpoints` array with the balancer's rotation state. |
| `GET /metrics` | always | Prometheus exposition (below). |
| `GET /.well-known/oauth-protected-resource` | `auth.mode: required` | RFC 9728 resource metadata; announces the EMA extension when configured. |
| `GET /.well-known/jwks.json` | EMA configured | fold's minting key. |
| `GET /api/federation` | `server.introspection.enabled` | The federation snapshot. Requires the same Bearer token as `/mcp` when auth is on and shares `/mcp`'s rate budgets; any valid principal (or, with `server.introspection.groups`, any allowlisted principal — others get an audited 403) sees the topology — URLs, owners, labels, each upstream's source (static/discovered) and credential-strategy *name*, plus shared-state/audit-sink/tracing facts and the viewer's tenant governance — with raw connect errors reduced to a category and no secret values or Redis URL ever included; see [security-model.md](security-model.md#the-console-has-no-privileged-path). Was `GET /console/api/state` through v1.8. |
| `GET /api/auth-hint` | `server.introspection.enabled` | The deliberately unauthenticated sign-in hint (issuer, client id, scopes, resource — public SPA configuration only), read by a browser client before it has a token. Was `GET /console/api/auth` through v1.8. |
| `GET /console/` | `server.console.enabled` | The read-only fold console page: dashboard + MCP test console (tools, prompts, and resources). Static assets are open (they carry no data). The dashboard reads `/api/federation` like any other client of it, so the page requires `server.introspection.enabled`. Test-console calls go through `/mcp` itself — governed and audited like any client. |
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
| `fold_list_items_total` | `method`, `stage` | List items **offered** by upstreams, **served** to the caller after per-principal policy filtering, and **capped** by a rule's `maxItems`. `served/offered` is the context reduction fold performs structurally, with no model in the loop; a low ratio is the feature working. A non-zero `capped` rate means callers are receiving truncated catalogues. |
| `fold_audit_events_total` | `sink`, `outcome` | Audit events by sink type and fate: `delivered`, `retried`, `dead_lettered`, `dropped`. The audit trail cannot report its own gaps — this is where one becomes visible. **Alert on `dropped` and `dead_lettered`**; the packaged `FoldAuditEventsLost` does. |
| `fold_tenant_requests_total` | `tenant`, `outcome` | MCP requests by tenant, with the same outcomes as `fold_requests_total`. Only requests whose principal resolved to a tenant appear here — everything is counted in `fold_requests_total` regardless — so a rising `denied` or `rate_limited` rate on one tenant is a customer about to open a ticket. |
| `fold_tenant_upstream_calls_total` | `tenant` | Upstream invocations attributed to a tenant: the same unit `tenants[].budget` is charged in, so this is a monthly allowance being spent, live. Cardinality is the number of declared tenants — config-bounded, like `upstream`. |
| `fold_budget_degraded_total` | `scope` | Budget decisions taken per-instance because shared state was unreachable. Budgets fail open by design, so this is the signal that a fleet is not enforcing one allowance — **alert on any non-zero rate**. |
| `fold_state_degraded_total` | `kind` | The limiter/breaker peer of the above: rate-limit (`limiter`) and circuit-breaker (`breaker`) decisions made from the instance's local mirror because Redis was unreachable. Per-instance enforcement continues; fleet-wide enforcement does not — **alert on any non-zero rate** (packaged as `FoldStateDegraded`). |
| `fold_http_rejections_total` | `reason` | Requests refused before the MCP layer: `body_too_large`, `forbidden_host`, `forbidden_origin`, `unauthenticated`, `rate_limited`, `oauth_token_rate_limited`, `introspection_viewer` (principal outside `server.introspection.groups`; was `console_viewer` and undocumented before v1.9). |
| `fold_discovery_syncs_total` | `outcome` | Discovery polls: `applied`, `unchanged`, `rejected` (document failed parse or merged validation), `error` (fetch failed). |
| `fold_hook_decisions_total` | `stage`, `outcome` | External decision-hook results: `allow`, `deny`, `error`. **Alert on `error`** — under `onError: "allow"` it counts calls that proceeded without inspection, which is a compliance gap rather than a service one. Packaged as `FoldHookErrors`. |
| `fold_hook_duration_seconds` | `stage` | What one decision round trip added to a request. This is latency fold adds on purpose, at your request — it is the number that says what inspection costs, and the floor against a local no-op endpoint is ~42 µs. |
| `fold_definition_drift_total` | `upstream`, `kind` | Definitions an upstream rewrote after fold had already served them, when `pinDefinitions` is on. The tool name is deliberately not a label — it is upstream-chosen and unbounded — so read the `upstream/definitionChanged` audit event for which one. A change is counted once, not once per refill: the new definition becomes the baseline. |
| `fold_downstream_sessions` | — | Live downstream MCP sessions. Sessions idle past `server.sessionIdleTimeoutMs` are expired; sustained growth means clients are minting sessions faster than they expire — raise the alarm before memory does. |
| `fold_upstream_bridged_sessions` | `upstream` | Per-client bridged sessions currently held against each upstream (idle ones sweep after 5 minutes). |
| `fold_panics_total` | `site` | Panics the gateway recovered instead of dying from: the request path (`route`, `fanout`), background loops (`sweep`, `discovery`, `probe`, `health`, `reload`, `telemetry`), SDK-invoked handlers (`bridge`, `notify`), and the audit delivery worker (`audit`). The process survives them by design, but **every one is a bug: alert on non-zero** and file what the paired `panic recovered` log line's stack trace shows. |
| `fold_build_info` | `version` | Always 1. |

Plus the standard Go process/runtime collectors. Alerting starters:
`fold_upstream_breaker_state == 2` sustained, any `fold_http_rejections_total`
rate spike, and — with discovery — any `rejected`/`error` sync outcomes.

## Dashboards, alerts, and SLOs

The metric names above are frozen by fold's [v1 compatibility
contract](../README.md#api-stability), which is what makes shipping queries
against them safe: a dashboard published today keeps working across upgrades.
Three things ship in the chart:

| What | Where | Enable |
|---|---|---|
| Grafana dashboard | [`deploy/helm/fold/dashboards/fold-overview.json`](../deploy/helm/fold/dashboards/fold-overview.json) | `metrics.dashboard.enabled=true` (ConfigMap for the Grafana sidecar), or import the JSON by hand |
| Alert rules | [`templates/prometheusrule.yaml`](../deploy/helm/fold/templates/prometheusrule.yaml) | `metrics.prometheusRule.enabled=true` (needs the prometheus-operator CRDs) |
| Scrape config | `templates/servicemonitor.yaml` | `metrics.serviceMonitor.enabled=true` |
| The same alerts for a plain Prometheus | [`deploy/observability/alerts.yml`](../deploy/observability/alerts.yml) | `rule_files:` in your prometheus.yml |
| A whole local stack — Prometheus + Grafana, dashboard preloaded | [`deploy/observability/`](../deploy/observability) | `docker compose --profile observability up` |

### The scrape has to reach a listener that will answer it

DNS-rebinding protection covers `/metrics` like every other path, so a scraper
reaching fold by any name outside `server.allowedHosts` is answered `403` and
the target reads as down with no other symptom. A pod IP is a Host no static
allowlist can name.

**The fix is `server.metricsAddr`** — a separate listener for `/metrics` and
`/health`:

```jsonc
"server": { "metricsAddr": ":9090" }   // bind it to an internal interface
```

In the chart, `metrics.listener.enabled=true` sets it, opens the container and
service port, and points the ServiceMonitor at it — after which scraping works
without touching `allowedHosts`. Under compose, add the port and scrape
`fold:9090`.

Why a second listener rather than exempting the path: a scrape names upstream
ids, namespaces, tenant ids, and multi-endpoint upstreams' endpoint URLs. On
the main port, rebinding protection is what keeps a page the operator visits
from reading that — a real exposure for the loopback-bound default. A separate
listener is not an origin a browser can be steered to, so it needs no Host
allowlist, and **what protects it is that you did not put it on the public
network**. The public port keeps its checks unchanged.

Without the listener, the workarounds are unchanged and still documented:
`allowedHosts` must include whatever name the scraper dials — `"fold"` under
compose, `"*"` on Kubernetes. This is the same constraint the chart already
handles for probes with `probes.hostHeader`.


A `gateway/observability_pack_test.go` keeps the pack and the code in
lockstep, both directions: every `fold_*` name a panel or rule references must
exist, and every metric fold exports must appear somewhere in the pack. A
renamed metric fails the build rather than producing a panel that draws
nothing and an alert that can never fire — the two failure modes that look
exactly like "healthy".

### The SLOs

**Availability — 99.9% of requests are ones fold did not fail.**

```promql
1 - (
  sum(increase(fold_requests_total{outcome=~"error|upstream_down"}[28d]))
  / clamp_min(sum(increase(fold_requests_total[28d])), 1)
)
```

The numerator is deliberately narrow. `denied`, `rate_limited`, and
`budget_exhausted` are **excluded**: those are the gateway doing its job, and
an SLO that counts them punishes an operator for tightening a policy and pages
someone when a customer hits the ceiling they were sold. Look for those in the
audit stream and the per-tenant series, where they are signal rather than
noise. `upstream_down` *is* counted — fold owns the federation's reachability
even when the fault is an upstream's.

**Latency — a deployment choice, not a fold constant.**

`fold_request_duration_seconds` is end to end, so it includes whatever the
upstream took. There is no honest universal threshold: a gateway fronting a
2 ms echo and one fronting a 30 s report generator should not share a number.
Set `metrics.prometheusRule.latencyP99Seconds` from your slowest legitimate
tool.

What fold *does* promise about its own cost is measured in CI, not in
production: added p50 under 5 ms through the proxy path, typically ~0.2 ms
([benchmarks](benchmarks.md)). The dashboard's "gateway overhead" panel
approximates it live by subtracting the upstream duration p95 from the request
duration p95 — useful as a shape, but quantiles do not subtract and a
federated list fans out to many upstream calls, so it over-reads on list-heavy
traffic. Treat a rising line as a question, not a measurement.

### What the alerts assume

| Alert | Fires on | Why it is not noise |
|---|---|---|
| `FoldErrorRateHigh` | error + upstream_down ratio over threshold, 10m | Refusals excluded, so it means fold is failing, not governing |
| `FoldRequestLatencyHigh` | p99 over a configured bound, 10m | Threshold is yours; unset it rather than tolerate a flapping page |
| `FoldUpstreamCircuitOpen` | breaker at 2 for 5m | The federation still serves everything else — urgent, not an outage |
| `FoldUpstreamEndpointEjected` | endpoint out of rotation 15m | Traffic is fine; a replica never came back |
| `FoldBudgetDegraded` | any degraded decision in 10m | The fleet is enforcing N copies of one allowance |
| `FoldDiscoverySyncFailing` | rejected/error sync in 15m | Last good set still serving, but nothing new applies — an id collision freezes discovery until resolved |
| `FoldTenantBudgetExhausted` | a tenant refused on budget in 15m | Nobody is broken; someone owes a customer a decision |
| `FoldHookErrors` | any hook error in 10m | Under `onError: "allow"`, these calls were served uninspected — the audit events name them via `hookOutcome` |
| `FoldHookLatencyHigh` | hook p99 over the configured bound, 10m | Not a fold regression: it is what your inspector costs, added to every inspected invocation |
| `FoldDefinitionDrift` | an upstream rewrote a definition it had already advertised, 15m | Fires once per change, not per request: what a model is instructed to do changed after the federation was approved, and a human should confirm which kind of change it was |
| `FoldIngressRejectionSpike` | bad Host/Origin or unauthenticated over threshold, 10m | Misconfigured client or someone probing — worth knowing which |

Severities are `warning` and `info` only. Nothing here pages by default:
fold's genuinely paging condition is "no upstream is reachable", which
`/health` answers with a 503 and your existing probe already watches.

## Audit events

One JSON event per terminal response — including 401s, 403-equivalents, and
429s — to the configured sinks (`stdout`, `webhook`; delivery is
asynchronous and batched, never adding request latency). Fields:

| Field | Meaning |
|---|---|
| `time` | UTC timestamp. |
| `principal`, `issuer` | Verified subject and token issuer; absent when auth is disabled. |
| `method` | MCP method (`tools/call`, …), `http` for pre-MCP rejections, `oauth/token` for the EMA endpoint, or `upstream/definitionChanged` for pinned-definition drift. |
| `name` | Namespaced tool/prompt name or resource URI. |
| `upstream` | Routed upstream id. |
| `decision`, `ruleId` | Policy outcome (`allow`/`deny`) and the matching rule. An explicit `deny` rule names itself here; a refusal that fell through to `defaultDecision` has no `ruleId`, which is how you tell "a rule refused this" from "nothing granted it". |
| `hookOutcome` | What the external decision hook said: `allow`, `deny`, or `error`. Present only when a hook inspected the request. `error` under `onError: "allow"` means this call was served **uninspected**. |
| `outcome` | `ok`, `error`, `denied`, `hook_denied`, `rate_limited`, `budget_exhausted`, `unauthenticated`, `upstream_down`, `forbidden`, `warned`. `hook_denied` is kept distinct from `denied` because policy and the inspector are different systems, often owned by different teams. The last is a finding rather than a request outcome — fold noticed something an operator should see and served the request anyway. |
| `direction` | `server_initiated` on the reverse path — a sampling or elicitation request an upstream made of the caller's client. Absent means the ordinary client-to-upstream direction, so every event predating the field reads unchanged. |
| `tenant` | The tenant the principal resolved to, when `tenants` is configured. Empty means no tenant matched — not an error, just a caller governed by the gateway-wide rules. |
| `upstreamCalls` | Upstream invocations this request cost — the fan-out. Not always 1. |
| `itemsServed` | Items a list returned **after** per-principal policy filtering and pagination: the surface this caller was handed, not the federation's total. |
| `usage` | Counters the upstream published in its result `_meta`, verbatim. Absent means the upstream reported nothing — fold never synthesizes it. |
| `error` | Error text, when the request failed. |
| `latencyMs` | End-to-end latency. |

## When audit delivery fails

Audit is the single exit door, so its own failures are the ones nothing else
reports. Delivery to a webhook is retried with exponential backoff and equal
jitter (4 attempts, 500 ms to 30 s by default), which covers the ordinary case
of a receiver restarting. Beyond that:

| `fold_audit_events_total` outcome | What happened | What to do |
|---|---|---|
| `retried` | An attempt failed; another is coming | Nothing, unless it is sustained — then the receiver is unhealthy |
| `dead_lettered` | Attempts ran out; events were appended to `deadLetterPath` | Fix the receiver, then replay the file — the records are intact |
| `dropped` | Events are gone: the buffer filled while the receiver was down, or delivery was abandoned with no `deadLetterPath` | Set `deadLetterPath`; the record has a hole for that window |

The `otlp-logs` sink delegates transport to the OTel exporter, so its retry is
the exporter's — fold's `retry` block still configures it (the attempt count is
expressed as the exporter's elapsed-time bound). Its outcomes are counted the
same way, and a batch it finally abandons is dead-lettered in the record's
converted form, labelled as such so a replay tool knows which shape it is
reading.

A `4xx` other than `429` is treated as permanent and not retried — a receiver
that rejects the payload rejects it identically every time, and retrying only
delays the dead letter.

Two things worth knowing before an incident. The buffer is bounded (1024
events) and a full buffer drops rather than blocking, because audit must never
become the reason a request is slow — the trade is deliberate, and the drop is
counted. And a sink that cannot be constructed at startup (a file path that
will not open) is logged and skipped rather than fatal, so one bad destination
does not take the gateway down; check the startup log for `audit sink not
started` if a sink seems inert.

The dead-letter file is a rotating file like any other fold writes — bounded at
100 MB × 5 by default — for the same reason: a dead-letter file that fills the
disk turns a delivery problem into an outage.

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
| `-32603` | Internal gateway error: a recovered panic, or an ambiguous tenant configuration | For a panic, retry is reasonable and the operator's `fold_panics_total` counter plus a `panic recovered` log line correspond to it. An ambiguous tenant match is config, not code — the gateway logs `ambiguous tenant configuration` and refuses rather than guesses; fix the tenant selectors. |

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
`mcp.name`, `fold.upstream`, `fold.outcome`, `fold.policy.decision`, and
`fold.policy.rule` (the same terminal fields as the audit event; the
principal's subject joins them as `enduser.id` only with
`tracing.recordPrincipal: true` — since v1.11 spans carry no personal data
by default, the audit trail being where identity belongs), and a client
span per upstream call (`upstream <id>`) closed with its guard outcome. Export is batched off the request path; `Close`/shutdown
flushes with a 3 s bound so a dead collector cannot hang termination.

## Log streams

Operational logs go to stderr via `log/slog` (`--log-format text|json`,
`--log-level debug|info|warn|error`). Per-request accounting deliberately
stays out of the log stream — that is what metrics and the audit sinks are
for. Startup, upstream connect failures and session drops, breaker transitions,
refused cross-host redirects, reload results, discovery and probe
transitions, and shutdown are the events to expect at `info`/`warn`
(successful upstream connects log at `debug`).
