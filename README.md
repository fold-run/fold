# fold

[![CI](https://github.com/fold-run/fold/actions/workflows/ci.yml/badge.svg)](https://github.com/fold-run/fold/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/fold-run/fold.svg)](https://pkg.go.dev/github.com/fold-run/fold)
[![Release](https://img.shields.io/github/v/release/fold-run/fold)](https://github.com/fold-run/fold/releases)

**fold: the enterprise MCP gateway — one governed endpoint between every MCP client and every MCP server.**

fold sits in front of any number of upstream MCP servers — in any language, on any SDK, from any team or vendor — providing federation, enterprise auth, policy, caching, rate limiting, and audit. It is built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), so the wire protocol (streamable HTTP, both request/response and SSE) is the SDK's own implementation on both the client-facing and upstream-facing sides.

**Conformant, provably.** The official [`@modelcontextprotocol/conformance`](https://github.com/modelcontextprotocol/conformance) suite runs against fold fronting the reference everything-server on every merge — **40/40 checks**, including sampling, elicitation, logging, progress, and resource subscriptions bridged through the gateway. The receipt is one click away: the [`conformance` job from the v1.15.0 release run](https://github.com/fold-run/fold/actions/runs/32932449075/job/98067151642), and [every green run on `main`](https://github.com/fold-run/fold/actions/workflows/ci.yml?query=branch%3Amain+is%3Asuccess). A [weekly job](.github/workflows/drift.yml) re-runs the suite against the *latest* unpinned SDK and opens a tracking issue the moment anything drifts. Reproduce it yourself with `./scripts/conformance.sh` — the same command CI runs.

## Use cases

- **Unify a federation.** Acquisitions, child orgs, and teams each ship their own MCP servers; fold presents them as one virtual server with namespaced tools — no team rewrites anything.
- **Draw the security boundary.** One choke point for authentication, deny-by-default tool allowlists (once a `policy` block is present — an absent one is allow-all, and the deploy checklist says so), per-principal visibility, and an audit event for every request including denials.
- **Broker credentials.** Clients hold one token with the gateway as audience; the gateway exchanges it per upstream (RFC 8693) or injects service credentials — API keys never reach agents.
- **Protect fragile services.** Caching, global and per-upstream rate limits, and circuit breakers stand between agent traffic storms and your internal systems.
- **Govern vendor MCP servers.** Put third-party/SaaS MCP endpoints behind your own auth, policy, and audit instead of scattering per-user API keys.

## Quick start

```bash
cat > fold.config.json <<'EOF'
{
  "upstreams": [
    { "id": "github", "url": "https://mcp.example.com/mcp", "namespace": "github" }
  ]
}
EOF

go run github.com/fold-run/fold/cmd/fold@latest --config fold.config.json --port 8080
# MCP endpoint: http://localhost:8080/mcp
# Binds 127.0.0.1 by default; pass --host 0.0.0.0 to expose beyond loopback.
```

Or run the container (~22 MB, distroless):

```bash
docker run --rm -p 8080:8080 \
  -e FOLD_CONFIG="$(cat fold.config.json)" \
  ghcr.io/fold-run/fold:latest
```

`FOLD_CONFIG` accepts either a file path or the JSON document itself (convenient for container env injection). Prebuilt binaries for linux/darwin (amd64/arm64) are on the [releases page](https://github.com/fold-run/fold/releases), or `go install github.com/fold-run/fold/cmd/fold@latest`.

A single upstream without a `namespace` runs in passthrough mode (no name rewriting). Multiple upstreams require namespaces; tools and prompts are exposed as `{namespace}__{name}`.

Embedding in your own Go service is one call:

```go
gw, err := gateway.New(cfg)   // cfg is a *config.Config
if err != nil { ... }
defer gw.Close()
http.ListenAndServe(":8080", gw.Handler())
```

A federated multi-org config — each upstream owned by a different team, in any language:

```jsonc
{
  "upstreams": [
    {
      "id": "github-tools",
      "url": "https://mcp.platform.acme.com/mcp",
      "namespace": "gh",
      "owner": { "org": "acme-platform", "team": "devex" }
    },
    {
      "id": "ml-search",
      "url": "https://mcp.ml.acquired-co.com/mcp",
      "namespace": "search",
      "owner": { "org": "acquired-co", "team": "ml" },
      "rateLimit": { "requestsPerMinute": 600 },
      "circuitBreaker": { "failureThreshold": 5, "halfOpenAfterMs": 30000 }
    }
  ],
  "server": { "rateLimit": { "requestsPerMinute": 6000 } }
}
```

fold fans list requests out across all upstreams concurrently, merges and namespaces the results, degrades gracefully when an upstream is down (`_meta["run.fold/partialFailure"]` lists the failed upstream ids), and short-circuits unhealthy upstreams with a per-upstream circuit breaker. Proxied results are tagged with their origin in `_meta["run.fold/upstream"]`. Set `REDIS_URL` (or `server.redisUrl`) to share cache, rate-limit, and circuit-breaker state across instances — a fleet of gateways behaves as one. See [`fold.config.example.json`](fold.config.example.json) for a full example.

**Replicated upstreams load-balance.** Give an upstream `urls` instead of `url` and the gateway balances across the replicas: MCP is sessionful, so each new session — the shared root session, and each client's bridged session — connects round-robin to the next healthy endpoint and stays pinned there. An endpoint that refuses a connection is skipped (the connect fails over to the next replica in the same attempt) and rests out of rotation for the breaker's `halfOpenAfterMs` before being retried. Add `"healthCheck": { "intervalMs": 30000 }` for active probing: the gateway connects to every endpoint on the interval, so a dead replica is ejected before the first client request and a recovered one returns to rotation without a live-request retry paying for the discovery. Per-endpoint health shows in `/health` and the `fold_upstream_endpoint_healthy` metric. Replicas are assumed identical — one namespace, one credential strategy, one policy surface.

**Configuration hot-reloads.** `kill -HUP` the process — or run with `--watch` to poll the config file — and fold revalidates and applies the new document without dropping the listener: the upstream set and the policy engine swap atomically, in-flight requests finish against the snapshot they started on, and connected clients receive `list_changed` notifications so they refetch. Upstreams whose config is unchanged keep their live sessions; removed or changed ones are drained (closed after their request timeout) and a changed upstream's resource subscriptions move to its replacement. Embedders get the same behavior via `gw.Reload(cfg)`. The `auth`, `server`, `routing`, `audit`, `tracing`, and `discovery` sections are wired in at construction and cannot hot-swap — changing them makes the reload fail loudly, keeping the running configuration; a rejected reload never takes anything down.

**Upstreams can be discovered.** With the `discovery` section configured, fold polls a URL for `{"upstreams": [...]}` (same schema as the static section) and hot-swaps the discovered set into the federation alongside the statically configured upstreams — a team ships an MCP server, the registry lists it, and it appears behind the gateway without anyone touching fold's config. Discovery composes with reload: base reloads keep the discovered set, discovery syncs keep the base, and both flow through the same validated atomic swap. Fail-safe by construction: an unreachable source, a malformed document, or one that collides with a static upstream id or namespace is rejected whole and the last good set keeps serving (`fold_discovery_syncs_total` counts outcomes).

## Request pipeline

```
POST /mcp
 → host validation      DNS-rebinding protection (Host/Origin allowlist)
 → body cap             maxBodyBytes (413 before any read), 30 s body read deadline
 → authenticate         Bearer → issuer allowlist → JWKS → audience → Principal
 → rate limit           global → tenant → per-principal windows → 429 + Retry-After
 → route                federated fan-out (lists) or namespaced routing, tenant resolved
 → visibility           tenant upstream subset (a fan-out never reaches what it excludes)
 → authorize            deny-by-default policy per invocation
 → per-upstream guards  rate limit, circuit breaker, request timeout, budgets
 → proxy                credentials attached, held SDK session per upstream
 → egress               per-principal list filtering, namespace rewriting
 → audit                one event per request, including denials (single exit door)
```

**Server-initiated traffic bridges both ways.** Named invocations run over a per-client upstream session whose handlers forward sampling (`sampling/createMessage`), elicitation, log messages, and progress notifications back to the originating client — routed over that call's own stream, so clients without a standalone SSE stream still hear them. `resources/subscribe` is forwarded to the owning upstream and `resources/updated` notifications fan back out to subscribed clients; `completion/complete` routes by prompt namespace or resource ownership; a client's `logging/setLevel` propagates to its upstream sessions. Idle per-client sessions are swept after 5 minutes, and the same sweep releases subscriptions whose downstream client disconnected without unsubscribing — the one shared upstream subscription per URI is dropped when its last live subscriber is gone. A single downstream session may hold at most 1024 subscriptions; the 1025th is refused with `-32602` until one is released, which keeps a table keyed by caller-chosen URIs finite without evicting a subscription some client still depends on.

**Routing headers cross the hop.** A client may mirror selected tool arguments into `Mcp-Param-{Name}` headers so an intermediary can dispatch without reading the body, and the transport requires an intermediary that does not recognize one to forward it and otherwise ignore it. fold does both: they are relayed to the upstream and never read, so the body stays the only thing fold parses and the headers cannot become a second control surface. The relay is the narrowest thing that satisfies the rule — fold forwards no other client header, never overwrites a value the SDK generated for the call it is actually making, and attaches nothing caller-supplied to a host outside the upstream's configured endpoints.

**If you route on these, validate them.** The specification has a server check a param header against the request body, and that check runs only once a connection has negotiated `2026-07-28` — which fold's default upstream protocol deliberately sits below, because the sessionful handshake is what carries server-initiated traffic. So on a default deployment nobody validates them: fold forwards without reading, by design, and the upstream's own transport does not check at that protocol level. A balancer or router in front of an upstream is therefore dispatching on a caller-supplied value that fold's policy engine never saw and that need not agree with the body fold authorized. Treat them as the routing hints they are and confirm against the body before acting on one. (They also ride every request of the call that carried them, including that session's own handshake, so a value scoped to one invocation can reach a router deciding where a whole session lives.)

**Multi-round-trip requests are answered, not relayed.** An upstream that needs more input answers with an input-required result rather than blocking: it returns the questions it wants answered and an opaque `requestState`, and the exchange completes when someone retries the original request with `inputResponses` filled in and that state echoed back. Through fold, that someone is currently *fold*. Its upstream client runs the SDK's multi-round-trip middleware, so an upstream's question is fulfilled from the caller's bridged session — elicitation, sampling — and the retry is issued below the router. The caller sees one completed call and never sees the question. That is era translation rather than proxying, and for a handshake-era client talking to a modern upstream it is the only thing that can work; it is also not what a relay would do, and the difference is visible in three places. A caller whose client declares no elicitation handler gets the call failed rather than the question. `resources/read` cannot do it at all, because reads ride the shared root session, which has no handlers to answer with. And a caller that could have answered for itself is never asked.

Where a downstream client does supply `inputResponses` and `requestState` itself, fold forwards both verbatim: the specification requires a client to echo the exact state and forbids inspecting or modifying it, and fold is the client on the upstream leg. Nothing about this is cached, and nothing may be — a result that depended on inputs outside the cache key is not cacheable, which is why fold's caching stops at lists.

**MCP Apps works through the gateway.** [MCP Apps](https://modelcontextprotocol.io/extensions/apps/overview) (SEP-1865) is negotiated by the *client*: a server checks `capabilities.extensions["io.modelcontextprotocol/ui"]` before registering UI-enabled tools, and serves a text-only fallback to anyone who did not declare it. fold carries that declaration rather than configuring it — there is no setting for this. A named invocation rides the per-client session, which now declares what its client declared; lists ride the shared root session, which cannot hold two answers, so root sessions and their list-cache entries are keyed by a normalized *capability profile*. A federation whose clients are all alike keeps exactly one session per upstream, as before; one with both kinds keeps two, and neither client is ever served the other's list. The profile is computed from the extension identifiers fold implements, never from the client's raw map, so a caller inventing extension ids cannot mint sessions or cache entries.

**MCP Apps interfaces stay attached to their own server.** [MCP Apps](https://modelcontextprotocol.io/extensions/apps/overview) (SEP-1865) points a tool at an interactive interface through a `ui://` resource URI in its `_meta.ui` — a URI the extension requires to be unique only *within one server*, and which the published starter templates ship with no server segment at all. Federate two upstreams built from one template and they advertise the same URI for different interfaces: fold used to serve a read of it from whichever upstream listed it last, so a host rendering one team's tool could get another team's app, and which one depended on what some other client had done first. fold now mints those URIs per namespace — `ui://fold/{namespace}/{rest}` — in `_meta.ui` (both the nested and the deprecated flat form), in `resources/list`, in what a read answers with, and in `resources/updated` notifications for a subscribed interface. Reads route straight to the owner with no probing and no dependence on request history, policy still decides on the URI the upstream published, and every other resource URI stays untouched: this is the one documented exception to fold never rewriting a URI, and it is narrow on purpose — a `ui://` pointer is republished on every `tools/list` rather than persisted by a client. Passthrough rewrites nothing, here as everywhere.

**Icons survive federation.** MCP lets a server hang an icon off a tool, prompt, or resource, and it tells the *client* how to handle one: fetch only `https` or `data:`, reject unsafe schemes and cross-origin redirects, fetch without credentials, sniff the bytes rather than trust the declared type — and "verify that icon URIs are from the same origin as the server". That last rule is what makes an icon a federation problem rather than a field. An upstream's icon points at the upstream's origin, which behind a gateway is neither the origin the client connected to nor, for the cluster-internal upstream that is the ordinary enterprise case, an origin the client can reach at all — so a conforming client rejects every federated icon and a lenient one fails to load it, and fold looks like a server whose upstreams have no icons. fold mints its own URL for each, `{server.publicUrl}/icons/{namespace}/{digest}`, and serves the bytes: the same move `ui://` minting makes, safe for the same reason, since an icon `src` is republished on every list rather than persisted by a client. Four bounds keep it from being an open proxy — only an icon on one of that upstream's own configured endpoint hosts is minted (the host bound credential attachment already uses, checked at mint and again before the fetch, so a hand-built digest cannot aim the gateway elsewhere); unsafe schemes are dropped rather than republished under fold's name; `data:` URIs pass through untouched; and passthrough mints nothing. The fetch carries no credentials by construction rather than by policy, refuses redirects outright, is bounded and refuses rather than truncates, and identifies the image from its magic bytes: `image/png`, `image/jpeg`, `image/webp`, `image/gif`. `image/svg+xml` is refused — see "Not implemented". The endpoint is unauthenticated because the specification requires clients to fetch icons without credentials and a browser `<img>` carries no token; what it discloses is bounded to match, and the reasoning is in [docs/security-model.md](docs/security-model.md). With no `server.publicUrl` and no `auth.resource`, nothing is minted and icons are served exactly as the upstream published them, which is what fold did before this existed. `server.identity` adds fold's own `icons` and `websiteUrl` to what it reports at `initialize`.

**Federated tasks.** Task ids are opaque and clients persist them across sessions, so — like resource URIs — fold never rewrites them; ownership is remembered instead. A `tools/call` that mints a task (advertised in the result `_meta`) pins `taskId → upstream`; `tasks/get`, `tasks/cancel`, `tasks/result`, and `tasks/update` route to that owner, whose errors pass through verbatim. A task fold never saw (minted out of band, or whose record has expired) is located by a read-only `tasks/get` probe across upstreams — the owner answers, everyone else is a healthy "no" — after which the mutating method is sent to the owner alone. `tasks/list` merges every upstream in deterministic id order and pages like the typed lists (`routing.pageSize`); ids no upstream knows answer `-31045`. Ownership is bound to the minting principal: another caller's task-scoped calls answer `-31045` exactly like an unknown id (no existence leak, no probe), and `tasks/list` shows each principal only its own tasks. The ownership index is an authorization record, not a routing hint, so it lives behind `state.Provider`: with `REDIS_URL` (or `server.redisUrl`) set, a whole fleet reads the same ownership and the binding survives a rolling restart — a caller cannot reach another principal's task by landing on an instance that did not serve the mint. Records are keyed by a digest of the task id, hold a digest of the owning principal, and expire after 24 hours; a Redis outage falls back to that instance's locally mirrored records. Tasks fold has no ownership record for (out of band, expired, or minted on a fleet with no shared state) stay reachable by any caller via the probe fallback; anonymous callers share one owner bucket, so no-auth deployments are unaffected. The Go SDK does not yet model the task lifecycle, so these are forwarded as opaque JSON via the SDK's custom-method mechanism to fold's task contract; the wire types are gateway-local and swap for the SDK's typed task API when it ships. `subscriptions/listen` fan-in is not included (the SDK exposes no public API for it).

## Configuration

One JSON document, validated on startup — structure and cross-field rules by
`fold --validate`, which needs no secrets; the things only a running process
can check (env vars named by `secretRef`, sink paths, Redis, the metrics
bind) at boot. Loaded from
`--config <path>` or `FOLD_CONFIG`, which accepts either a file path or the
JSON itself. A JSON Schema ships with fold
([`config/fold.config.schema.json`](config/fold.config.schema.json), printed
by `fold --schema`) for editor completion and CI linting.

**Full reference: [docs/configuration.md](docs/configuration.md)** — every
field, default, and the reasoning behind each one.

| Section | What it configures |
|---|---|
| [`upstreams`](docs/configuration.md#upstreams) | **Required.** The federation itself: each upstream's endpoint or replica set, namespace, credential strategy, timeouts, breaker, rate limit, budget, and list-cache TTL. |
| [`auth`](docs/configuration.md#auth) | Gateway authentication: trusted issuers, JWKS, and the token audience every caller must present — plus the embedded EMA authorization server. |
| [`policy`](docs/configuration.md#policy) | Deny-by-default authorization: rules over server, method, name, arguments, and tool annotations, gated on principals by group, subject, issuer, claim, or OAuth scope — filtering lists and refusing calls together, in both directions. |
| [`hook`](docs/configuration.md#hook) | The external decision endpoint: an out-of-process allow/deny on ingress, egress, and server-initiated traffic. Absent by default. |
| [`audit`](docs/configuration.md#audit) | Where the event per terminal response goes: stdout, rotating file, webhook, or OTLP logs, with retry and dead-lettering. |
| [`server`](docs/configuration.md#server) | The listener: MCP path, allowed hosts, global and per-principal rate limits, body cap, session expiry, shared state, the metrics listener, introspection, and the console. |
| [`routing`](docs/configuration.md#routing) | The namespace separator, and the page size federated lists are served in. |
| [`discovery`](docs/configuration.md#discovery) | Polling a registry for upstreams, with the allowlists that bound what a partially trusted source may ask for. |
| [`tenants`](docs/configuration.md#tenants) | Groups of principals, resolved from claims the IdP already asserts, and the budget, rate limit, and upstream visibility applied to each. |
| [`tracing`](docs/configuration.md#tracing) | OTLP endpoint, service name, and sample ratio for fold's own spans. Absent, the caller's trace context is still propagated. |

## Error codes

Gateway-minted JSON-RPC errors (upstream errors pass through verbatim):

| Code | Meaning |
|---|---|
| `-31040` | Per-upstream rate limit exceeded |
| `-31041` | Upstream unavailable (circuit open / unreachable / all upstreams down) |
| `-31042` | Policy denied the invocation. When scopes were the only thing missing, `data.missingScopes` names the ones the caller lacks. |
| `-31043` | Name does not resolve to a configured namespace |
| `-31044` | Consumption budget exhausted for the period (server or per-upstream) |
| `-31045` | Task id not owned by any upstream |

## Observability

- `GET /metrics` — Prometheus exposition. Gateway metrics: `fold_requests_total{method,outcome}`, `fold_request_duration_seconds{method}`, `fold_upstream_requests_total{upstream,outcome}`, `fold_upstream_request_duration_seconds{upstream}`, `fold_upstream_breaker_state{upstream}` (0 closed / 1 half-open / 2 open), `fold_upstream_endpoint_healthy{upstream,endpoint}` (multi-endpoint upstreams: 1 in rotation, 0 ejected), `fold_http_rejections_total{reason}`, `fold_jwks_fetches_total{issuer,outcome}` (the inbound trust anchor: `ok`, `error`, or `stale` when a failed refresh left the last good key set serving), `fold_discovery_syncs_total{outcome}`, `fold_list_items_total{method,stage}` (offered / served / capped — the context reduction per-principal filtering performs, as a ratio with a denominator), `fold_build_info{version}`, plus standard Go process/runtime collectors. With `tenants` configured, two more carry the tenant dimension: `fold_tenant_requests_total{tenant,outcome}` and `fold_tenant_upstream_calls_total{tenant}` — the second counts the unit a tenant budget is charged in, so an allowance can be watched being spent. They are separate series rather than a `tenant` label on the metrics above, because label sets are frozen by the [compatibility contract](#api-stability).
- **Packaged interpretation, not just data** — a Grafana dashboard, a `PrometheusRule` set, and documented SLOs ship in the chart (`metrics.dashboard.enabled`, `metrics.prometheusRule.enabled`). The availability SLO deliberately counts only outcomes fold *failed*: a denial, a rate limit, and an exhausted budget are the gateway working as configured, and an SLO that counted them would page someone for tightening a policy. A test keeps every panel and alert in lockstep with the metric names the code registers, the two rule files in lockstep with each other (expression, duration, severity, summary), and every error code the pack names in lockstep with the codes fold mints. Two alerts page — `FoldDown` and `FoldAllRequestsFailing`; the rest warn. See [docs/operations.md](docs/operations.md#dashboards-alerts-and-slos).
- **Distributed tracing** — W3C Trace Context (`traceparent`/`tracestate`) headers on incoming requests are always propagated to the upstream calls they cause, so the gateway hop joins the caller's trace. With the `tracing` section configured, fold also emits its own OpenTelemetry spans (server span per request, client span per upstream call, pipeline outcomes as attributes) over OTLP/HTTP — the gateway hop appears *in* the trace instead of being invisible, and upstream calls parent under fold's client span while keeping the caller's trace id.
- **Latency, measurably** — the `bench` CI job gates every merge on added p50 latency < 5 ms through the proxy path (loose for shared runners). Typical local numbers (Apple Silicon, in-process upstream): **~0.20 ms added p50**, gateway p99 ≈ 0.57 ms. Reproduce: `make bench`.
- **Throughput, measurably** — `make loadtest` sweeps one instance under 8/64/256 concurrent SDK client sessions, direct vs through-fold: **~9,300 req/s `tools/call` at 64 connections (p99 ≤ 19 ms), 13,400 req/s at 256 (p99 ≤ 61 ms), zero errors** (Apple M4 Pro, loopback). Methodology, full tables, and the honest caveats: [docs/benchmarks.md](docs/benchmarks.md).
- **Structured logging** — operational events (startup, upstream connect/reconnect, session drops, circuit-breaker transitions, refused cross-host redirects, SSE-hang fallbacks, shutdown) log via `log/slog`. `--log-format text|json` and `--log-level debug|info|warn|error` on the CLI; embedders pass `gateway.WithLogger(*slog.Logger)`. Per-request accounting stays in metrics and the audit sink, not the log stream.

## Operational endpoints

- `GET /health` — pings every upstream concurrently; reports per-upstream connectivity, latency, breaker state, owner, and — for multi-endpoint upstreams — the balancer's per-endpoint rotation state. `503` when no upstream is reachable — and, on a gateway whose upstreams all arrive from `discovery`, until the first document has actually been applied: a pod whose source was down at boot has an empty federation, not a healthy one, and reports it under a `discovery` object (`lastOutcome`, `lastSyncAt`, `applied`) rather than joining the Service to serve empty lists. (`/healthz`, the pre-v1.5 path, was removed in v1.9 — see [API stability](#api-stability).) The fan-out is shared: concurrent callers ride one collection and the result is reused for a second, so probing this unauthenticated endpoint in a loop cannot multiply into upstream traffic. A reload or discovery sync invalidates it immediately.
- `GET /metrics` — Prometheus metrics (see Observability).
- `GET /.well-known/oauth-protected-resource` — RFC 9728 metadata (when auth is enabled).
- `GET /.well-known/oauth-authorization-server` — RFC 8414 metadata for the embedded EMA authorization server (when EMA is enabled). fold names itself in the RFC 9728 document's `authorization_servers` whenever EMA is on, so this is where that pointer resolves: issuer, token endpoint, key set, and the one grant type it accepts.
- `GET /api/federation` — the federation snapshot (when `server.introspection.enabled`): upstream health and topology, policy shape, audit sinks, discovery status, and the viewer's tenant governance. Authenticates like `/mcp` and shares its rate budgets. Was `/console/api/state` through v1.8.
- `GET /api/auth-hint` — the deliberately unauthenticated sign-in hint (when `server.introspection.enabled`): issuer, client id, scopes, resource. Public SPA configuration only. Was `/console/api/auth` through v1.8.
- `GET /console/` — the read-only fold console page (when `server.console.enabled`): an observability dashboard over `/api/federation` plus an MCP test console. The test console is a plain MCP client against the gateway's own `/mcp`, so policy, rate limits, and audit apply to it like any other client — there is no privileged path.

## Guides

- [docs/configuration.md](docs/configuration.md) — the configuration reference: every section, field, and default, with the reasoning behind each.
- [docs/deploy.md](docs/deploy.md) — Docker, compose, Helm, systemd; TLS/SSE fronting, `allowedHosts` and probes, hot reload in each shape, Redis for fleets, the production checklist.
- [docs/operations.md](docs/operations.md) — day-2 reference: every endpoint, metric, audit field, and error code, and how reloads, discovery, and probes surface in logs and metrics.
- [docs/security-model.md](docs/security-model.md) — the architecture: trust anchors, the inbound chain, the enforcement pair, credential containment, tenant isolation.
- [docs/discovery-controller.md](docs/discovery-controller.md) — `fold-discovery`, the Kubernetes producer: label a Service and it joins the federation.
- [docs/stdio.md](docs/stdio.md) — `fold-stdio`, the shim that puts a local stdio MCP server behind the gateway as an ordinary http upstream.
- [docs/registry-discovery.md](docs/registry-discovery.md) — `fold-registry`, the MCP Registry producer: name the servers you trust and the gateway tracks their endpoints, versions, and retirements.
- [docs/benchmarks.md](docs/benchmarks.md) — the latency gate and the throughput sweep: methodology, numbers, how to reproduce.
- [docs/embedding.md](docs/embedding.md) — the Go embedding surface, with CI-compiled examples.
- [docs/defaults.md](docs/defaults.md) — the v1.0 defaults review, every default a decision on record.
- [docs/roadmap.md](docs/roadmap.md) — direction: what fold intends to build next, and what it deliberately declines.
- [docs/design-stdio.md](docs/design-stdio.md) — design record: why stdio upstreams arrive as a sidecar shim rather than subprocess supervision in the gateway.
- [docs/design-consumption.md](docs/design-consumption.md) — design record: quotas, budgets, and metering — what a gateway on the MCP path can honestly govern, and what it must refuse to guess.
- [docs/design-policy-depth.md](docs/design-policy-depth.md) — design record (proposed): deny rules, argument constraints, and destructive-operation gating — with the precedence, visibility, and trust questions each one raises settled before implementation.
- [docs/design-mcp-apps.md](docs/design-mcp-apps.md) — design record: MCP Apps through a federating gateway — why a client-declared extension needs capability-keyed upstream sessions, why `ui://` URIs are the one kind fold rewrites, and the two gaps that belong to the extension's specification rather than to fold.
- [docs/design-tenancy.md](docs/design-tenancy.md) — design record: the tenant object — a named set of principals and the governance that applies to them, why it never becomes a trust anchor, and the two corrections implementation forced on the design.
- [docs/design-egress-oauth.md](docs/design-egress-oauth.md) — design record (proposed): per-user upstream credentials — how a caller reaches a third-party SaaS MCP server as themselves without the credential ever touching their laptop, why the consent prompt is URL-mode elicitation rather than anything fold invents, and where the line sits between brokering credentials and becoming a secrets manager.

## Deploying

fold is a single static binary with no local state — see [docs/deploy.md](docs/deploy.md) for the full guide (TLS, `allowedHosts`, probes, Redis, secrets, audit shipping, production checklist).

- **Docker**: `ghcr.io/fold-run/fold` (multi-arch, distroless) — see [Quick start](#quick-start).
- **docker compose**: [`compose.yaml`](compose.yaml), with optional Redis, stdio-shim, and observability profiles — `make compose-up` runs it.
- **Kubernetes**: Helm chart in [`deploy/helm/fold`](deploy/helm/fold) — probes, config-as-ConfigMap, secrets via `envFrom`, optional Ingress/HPA/PDB/ServiceMonitor.
- **VM / bare metal**: prebuilt binaries on the [releases page](https://github.com/fold-run/fold/releases) plus a hardened systemd unit in the guide.

## Repository layout

| Path | Purpose |
|---|---|
| `cmd/fold` | The `fold` CLI |
| `cmd/fold-discovery` | The Kubernetes discovery-document producer (`internal/kubediscovery`) |
| `cmd/fold-stdio` | The stdio shim: runs one local MCP server over streamable HTTP (`internal/stdiobridge`) |
| `cmd/fold-registry` | The MCP Registry discovery-document producer (`internal/mcpregistry`) |
| `gateway` | Gateway engine: pipeline, federation routing, proxying, health |
| `config` | Config schema + validation |
| `auth` | OAuth resource server (JWKS verifier) + upstream credential strategies |
| `policy` | Allowlist policy engine + per-principal list filtering |
| `audit` | Audit events + sinks (stdout, webhook) |
| `docs` | Guides and decision records — see [Guides](#guides) above, plus the [roadmap](docs/roadmap.md) |
| `internal/ratelimit` | Sliding-window limiter |
| `internal/breaker` | Circuit breaker |
| `internal/cache` | TTL cache with single-flight refresh |

## Development

```bash
make build
make test          # unit + integration (real SDK client/server fixtures)
make race          # what CI runs
make cover         # race tests + coverage summary (coverage.out)
make lint          # golangci-lint (config in .golangci.yml)
make check         # fmt-check + tidy-check + vet + build + race + lint
make bench         # added-latency gate (p50 < 5 ms through the proxy path)
make fuzz          # fuzz every parser an untrusted party controls: config, cursors, discovery docs, _meta, cache scope, param headers (seeds run in `make test`; drift.yml runs this weekly)
make conformance   # official conformance suite through the gateway (needs node)
make compose-up    # run the local stack on this host (see docs/deploy.md)
```

CI runs these same targets (`.github/workflows/ci.yml`), plus `govulncheck` (`make vuln`).

The integration suite spins up real MCP servers from the official Go SDK behind the gateway and exercises federation, namespacing, policy filtering and denial, partial failure, credential injection (static and passthrough), rate limits, the breaker, JWT auth against a fixture JWKS issuer, RFC 9728 metadata, and the full server-initiated bridging loop (sampling, elicitation, logging, progress).

## API stability

This is the v1 compatibility contract, in force as of v1.0.0.

**Frozen at v1.0** (breaking changes only with a new major version):

- **The config document** — field names, meanings, defaults, and validation semantics. The machine-readable contract is [`config/fold.config.schema.json`](config/fold.config.schema.json) (`fold --schema`), kept in lockstep with the code by test. Defaults are part of the freeze — every one was reviewed as a deliberate decision before v1.0 ([`docs/defaults.md`](docs/defaults.md)).
- **The `fold` CLI** — flags, exit codes, and `FOLD_CONFIG` semantics.
- **The wire surface** — gateway-minted JSON-RPC error codes, HTTP endpoints (`/mcp`, `/health`, `/metrics`, `/api/federation`, `/api/auth-hint`, `/icons/*`, `/.well-known/*`, `/oauth/token`), metric names and label sets, and the audit event JSON shape. The `/api/federation` **response shape** is frozen with the rest: fields may be added within a major, none removed or renamed — it has an out-of-tree consumer that can skew against the gateway, which is what makes it a contract rather than an internal detail. `/healthz` was the health path through v1.4, a deprecated alias from v1.5.0, and **removed in v1.9.0**; point probes at `/health`.
- **Go, for embedders** — the `gateway` package (`New`, `Option`, `WithLogger`, `Gateway.Handler/Reload/Close`, `Version`), the `config` package's document structs and `Load`/`Parse`/`Validate`/`Schema`, plus the contract types the gateway hands outward: `auth.Principal` with `WithPrincipal`/`PrincipalFromContext`, and `audit.Event`/`Outcome`. See the [package example](https://pkg.go.dev/github.com/fold-run/fold/gateway).

**Wiring, not API** (may change in any release): the constructors the gateway threads through its packages — `auth.Verifier`/`EMA`/`UpstreamCredentials`, `policy.Engine`, `audit.Logger`/`Sink`. They are exported so the gateway can reach them across package boundaries, not as an extension surface. `internal/` packages are never API.

**Upgrades and deprecation.** fold follows semver: within a major version, upgrades are drop-in — new config fields and capabilities arrive in minors, nothing frozen changes. Anything slated for removal is deprecated in a minor release (documented here and in the changelog, with the replacement) and removed no sooner than the next major. Security fixes land on the latest minor as patch releases (see [SECURITY.md](SECURITY.md)).

**One break after v1.9.0, and why.** The gateway-minted error codes changed once more after this contract took effect: the `2026-07-28` protocol revision reserved the range they occupied, and one of them — `-32042` — had already been redefined as "URL elicitation required", a meaning shipped SDKs act on by prompting the user and retrying. A policy denial that a conforming client turns into a prompt and a retry is not something a gateway can wait a major version to fix. The codes moved outside the JSON-RPC reserved range (only the thousands digit changed); the reasoning and the full mapping are in the changelog. The same justification as below applies: no users yet.

**This contract applies from v1.9.0 forward.** v1.9.0 itself makes breaking changes to config field names and endpoint paths without a deprecation window — the one release that does. fold had no users to protect at that point, and taking the break there rather than after launch was the honest trade; the alternative was a major-version bump whose module-path change would have silently pinned every published `go run …@latest` to v1.8.0 forever. See the v1.9.0 changelog entry for the migration.

## Not implemented

Known gaps, documented deliberately:

- **SEP-2575 `subscriptions/listen` streams** — the Go SDK supports the 2026-07-28 protocol on its streamable HTTP server only in stateless mode, which fold cannot use: session-keyed bridging (sampling, elicitation, per-client streams) requires stateful sessions. Clients on the legacy handshake — which is what the SDK negotiates against stateful servers today — get full notification fan-in (list-changed and resource-updated, tested in `gateway/listen_test.go`). fold's fan-in already sits on the surfaces the SDK reuses for listen streams, and a drift canary in that test fails when the SDK lifts the restriction. Federated *tasks* (get/list/cancel/result/update with mint-affinity and probe fallback) **are** implemented — see above.
- **Multi-round-trip requests are terminated at the gateway, not proxied** — described under [Request pipeline](#request-pipeline). fold's upstream client runs the SDK's multi-round-trip middleware, so an upstream's input request is answered from the caller's bridged session and the retry is issued below the router; the caller never sees the question. That is the right behavior for a handshake-era caller talking to a modern upstream, and the wrong one for a caller that speaks the pattern itself. Three consequences follow and none is yet addressed: a client declaring no elicitation handler has the call failed rather than being asked; `resources/read` cannot complete an input-required exchange at all, because reads ride the shared root session and it carries no handlers; and a modern-era caller is never given the chance to answer. Turning fold into a relay is a one-line change to the client options and a large change to what the bridged sessions are for, so it waits on the same decision as [roadmap](docs/roadmap.md) item 13 — which was designed against the server-initiated mechanism this pattern replaced.

- **What the `2026-07-28` era changes, and fold has not** — none of it reachable today, because the SDK serves that revision only on stateless HTTP servers and fold runs stateful; `gateway/era_test.go` and `gateway/listen_test.go` pin the refusal from both sides, and each becomes the notice to do this work. fold advertises no `extensions` in its server capabilities, so a client following the tasks extension's negotiation rules would not declare the extension and would never reach fold's federated tasks. It federates `tasks/list` and `tasks/result`, which the redesigned extension removed in favour of polling `tasks/get`. It still advertises `resources/subscribe` and still handles `logging/setLevel`, both of which that revision retired — the level moved to a per-request `_meta` key fold has no forwarding path for. And a result fold relays from an upstream carries no `resultType` of its own, so the SDK stamps `"complete"` on it, which would be wrong for a relayed input-required result. All of it is correct for the era fold actually serves and wrong for the next one; the ordering is deliberate rather than neglectful.

- **An upstream's own caching hints are not honoured** — MCP lets a server declare how long a list may be considered fresh (`ttlMs`) and who may share it (`cacheScope`). fold reads neither: its list cache is bounded by the operator's `cacheTtlMs` alone, so an upstream declaring one second of freshness is served from fold's cache for the default thirty. The cause is structural rather than an oversight — the SDK's paginating iterators yield items and consume the result envelope the hints ride on, so fold never sees them, and `state.ListCache.GetOrFill` takes its TTL *before* running the fill that would produce one. Honouring them means changing a `state.Provider` interface with two implementations. The mirror-image decision on the outbound side is made and documented in `gateway/cachehint.go`; this is the same argument owed on the inbound side. The `cacheScope` half is a narrower gap than it looks: with a gateway-configured credential every caller is one authorization context from the upstream's point of view, and fold already disables list caching entirely where that is not true.

- **Icons: three narrower gaps, all deliberate.** **SVG icons are refused rather than proxied.** Two arguments, either sufficient. fold would be serving the SVG from *fold's own origin*, where an SVG's embedded script runs same-origin with `/console/`, `/api/federation`, and `/oauth/token` — strictly worse than the upstream serving it, since the upstream's origin holds nothing of fold's. And structurally, SVG is XML and has no magic bytes, so the strict magic-byte allowlist the specification asks a consumer to maintain cannot admit it without either trusting the declared content type or parsing the XML, and the second is the content inspection below. An upstream whose only icon is an SVG renders as no icon. **Resource-template icons are not minted** — templates are the one list fold passes through with no copy at all, so minting them means a new memo for the rarest case; they are served as the upstream published them, which is the pre-icons behaviour. And **passthrough rewrites nothing**, so a single un-namespaced upstream's icons keep pointing at that upstream — which is what a client talking to it directly would see, and therefore what passthrough means.

- **An upstream's `instructions` are discarded.** MCP lets a server send guidance for the model alongside its capabilities, and fold reads no upstream `InitializeResult` at all: `Gateway.instructions` synthesizes fold's own text from the namespace list, so a client is told how names are namespaced and nothing whatsoever about what the upstreams behind them are for. That is real content loss for a federating gateway and it is not a hard problem — the open question is what merging N instruction blocks into one honestly looks like, since concatenation grows without bound and picking one upstream's is arbitrary. No design has been argued yet, which is why this is a gap rather than a plan.

- **Progress is bridged only on `tools/call` and `prompts/get`.** `notifications/progress` reaches a caller over its bridged session, and `bridgeFor` has exactly three call sites. Everything else — `resources/read`, every list, `completion/complete`, and the federated `tasks/*` — runs on the shared root session, whose client options carry no progress handler, so an upstream reporting progress on a long read reports it into nothing. This is the same root cause README already records for multi-round-trip requests ("reads ride the shared root session and it carries no handlers"); the progress consequence was not stated. It also requires `protocol: "session"` and a stateful downstream session on both legs.

- **Content inspection (DLP / PII filtering / prompt-injection detection)** — deliberately out of scope. Inspecting request and response bodies means buffering and rewriting traffic, which conflicts with fold's invisibility rule (behavior through the gateway matches hitting the upstream directly) and its latency gate — and inline detection is a product of its own, not a gateway feature. fold's security model is structural instead: deny-by-default tool allowlists, per-principal invisibility, claim-gated (ABAC) rules, credential brokering so agents never hold upstream keys, and a full audit trail to feed the SIEM that does the detecting. When inline inspection is genuinely table stakes, the fold-shaped answer is the opt-in [decision hook](docs/configuration.md#hook) — an external policy endpoint on the ingress, egress, and server-initiated paths — not built-in scanning. It decides and cannot rewrite, which is what keeps the invisibility rule intact while still giving an organization somewhere to put its own judgement.

- **MCP Apps: two protocol-level gaps** — the extension gives an app no way to learn the name its host knows a tool by, so an app that hardcodes one cannot call it once federation has namespaced it (verified: the reference host forwards the bare name and fold answers `-31043`). And the host's rule that an app may not call tools across servers has no meaning when every upstream arrives on one connection, because the specification defines no way for a gateway to tell an app-initiated `tools/call` from a model-initiated one. Both need answers in the extension itself — every aggregator has the second hole and none can close it alone — so they are reports to file rather than features to build. Everything on fold's side of the line ships: capability negotiation and `ui://` routing, above. Design record: [docs/design-mcp-apps.md](docs/design-mcp-apps.md).

The gaps that carry a plan or a dependency appear in [docs/roadmap.md](docs/roadmap.md) — `subscriptions/listen` with the SDK dependency it waits on, content inspection as a standing non-goal, and the MCP Apps pair in the gated horizon, waiting on the extension's own specification — alongside the rest of what fold does and does not intend to build. The three added with icons carry neither: the SVG refusal is a decision, and the other two are honest reports of things nobody has argued a design for yet.

## Changelog

Release history, newest first: [CHANGELOG.md](CHANGELOG.md).

## License

Apache-2.0
