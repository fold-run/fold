# fold

[![CI](https://github.com/fold-run/fold/actions/workflows/ci.yml/badge.svg)](https://github.com/fold-run/fold/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/fold-run/fold.svg)](https://pkg.go.dev/github.com/fold-run/fold)
[![Release](https://img.shields.io/github/v/release/fold-run/fold)](https://github.com/fold-run/fold/releases)

**fold: the enterprise MCP gateway — one governed endpoint between every MCP client and every MCP server.**

fold sits in front of any number of upstream MCP servers — in any language, on any SDK, from any team or vendor — providing federation, enterprise auth, policy, caching, rate limiting, and audit. It is built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), so the wire protocol (streamable HTTP, both request/response and SSE) is the SDK's own implementation on both the client-facing and upstream-facing sides.

**Conformant, provably.** The official [`@modelcontextprotocol/conformance`](https://github.com/modelcontextprotocol/conformance) suite runs against fold fronting the reference everything-server on every merge — **40/40 checks**, including sampling, elicitation, logging, progress, and resource subscriptions bridged through the gateway (`conformance` job in [CI](.github/workflows/ci.yml); reproduce with `./scripts/conformance.sh`).

## Use cases

- **Unify a federation.** Acquisitions, child orgs, and teams each ship their own MCP servers; fold presents them as one virtual server with namespaced tools — no team rewrites anything.
- **Draw the security boundary.** One choke point for authentication, deny-by-default tool allowlists, per-principal visibility, and an audit event for every request including denials.
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

**Replicated upstreams load-balance.** Give an upstream `urls` instead of `url` and the gateway balances across the replicas: MCP is sessionful, so each new session — the shared root session, and each client's bridged session — connects round-robin to the next healthy endpoint and stays pinned there. An endpoint that refuses a connection is skipped (the connect fails over to the next replica in the same attempt) and rests out of rotation for the breaker's `halfOpenAfterMs` before being retried. Add `"healthCheck": { "intervalMs": 30000 }` for active probing: the gateway connects to every endpoint on the interval, so a dead replica is ejected before the first client request and a recovered one returns to rotation without a live-request retry paying for the discovery. Per-endpoint health shows in `/healthz` and the `fold_upstream_endpoint_healthy` metric. Replicas are assumed identical — one namespace, one credential strategy, one policy surface.

**Configuration hot-reloads.** `kill -HUP` the process — or run with `--watch` to poll the config file — and fold revalidates and applies the new document without dropping the listener: the upstream set and the policy engine swap atomically, in-flight requests finish against the snapshot they started on, and connected clients receive `list_changed` notifications so they refetch. Upstreams whose config is unchanged keep their live sessions; removed or changed ones are drained (closed after their request timeout) and a changed upstream's resource subscriptions move to its replacement. Embedders get the same behavior via `gw.Reload(cfg)`. The `auth`, `server`, `routing`, `audit`, `tracing`, and `discovery` sections are wired in at construction and cannot hot-swap — changing them makes the reload fail loudly, keeping the running configuration; a rejected reload never takes anything down.

**Upstreams can be discovered.** With the `discovery` section configured, fold polls a URL for `{"upstreams": [...]}` (same schema as the static section) and hot-swaps the discovered set into the federation alongside the statically configured upstreams — a team ships an MCP server, the registry lists it, and it appears behind the gateway without anyone touching fold's config. Discovery composes with reload: base reloads keep the discovered set, discovery syncs keep the base, and both flow through the same validated atomic swap. Fail-safe by construction: an unreachable source, a malformed document, or one that collides with a static upstream id or namespace is rejected whole and the last good set keeps serving (`fold_discovery_syncs_total` counts outcomes).

## Request pipeline

```
POST /mcp
 → host validation      DNS-rebinding protection (Host/Origin allowlist)
 → authenticate         Bearer → issuer allowlist → JWKS → audience → Principal
 → rate limit           global window → 429 + Retry-After
 → route                federated fan-out (lists) or namespaced routing
 → authorize            deny-by-default policy per invocation
 → per-upstream guards  rate limit, circuit breaker, request timeout
 → proxy                credentials attached, held SDK session per upstream
 → egress               per-principal list filtering, namespace rewriting
 → audit                one event per request, including denials (single exit door)
```

**Server-initiated traffic bridges both ways.** Named invocations run over a per-client upstream session whose handlers forward sampling (`sampling/createMessage`), elicitation, log messages, and progress notifications back to the originating client — routed over that call's own stream, so clients without a standalone SSE stream still hear them. `resources/subscribe` is forwarded to the owning upstream and `resources/updated` notifications fan back out to subscribed clients; `completion/complete` routes by prompt namespace or resource ownership; a client's `logging/setLevel` propagates to its upstream sessions. Idle per-client sessions are swept after 5 minutes.

**Federated tasks.** Task ids are opaque and clients persist them across sessions, so — like resource URIs — fold never rewrites them; ownership is remembered instead. A `tools/call` that mints a task (advertised in the result `_meta`) pins `taskId → upstream`; `tasks/get`, `tasks/cancel`, `tasks/result`, and `tasks/update` route to that owner, whose errors pass through verbatim. A task fold never saw (minted on another instance, or affinity evicted) is located by a read-only `tasks/get` probe across upstreams — the owner answers, everyone else is a healthy "no" — after which the mutating method is sent to the owner alone. `tasks/list` merges every upstream in deterministic id order and pages like the typed lists (`routing.pageSize`); ids no upstream knows answer `-32002`. Ownership is bound to the minting principal: another caller's task-scoped calls answer `-32002` exactly like an unknown id (no existence leak, no probe), and `tasks/list` shows each principal only its own tasks. Tasks fold has no ownership record for (out-of-band, or evicted) stay reachable by any caller via the probe fallback; anonymous callers share one owner bucket, so no-auth deployments are unaffected. The Go SDK does not yet model the task lifecycle, so these are forwarded as opaque JSON via the SDK's custom-method mechanism to fold's task contract; the wire types are gateway-local and swap for the SDK's typed task API when it ships. `subscriptions/listen` fan-in is not included (the SDK exposes no public API for it).

## Configuration

One JSON document, validated on startup (`fold --validate`). Loaded from `--config <path>` or `FOLD_CONFIG`.

A JSON Schema for the document ships with fold — [`config/fold.config.schema.json`](config/fold.config.schema.json), printed by `fold --schema`, included in release archives — for editor completion and CI linting. The schema is the structural contract (fields, types, enums, required properties) and is kept in lockstep with the code by test; cross-field rules (namespace requirements, https mandates) remain `fold --validate`'s job.

### `upstreams` (required)

| Field | Default | Notes |
|---|---|---|
| `id` | — | Lowercase alphanumeric + hyphens. Used in policy, audit, health. |
| `url` | — | The upstream's MCP endpoint. Exactly one of `url` / `urls`. |
| `urls` | — | Multiple equivalent replicas of this upstream. New sessions balance round-robin across them with connect failover; a failed endpoint rests for the breaker's `halfOpenAfterMs`. |
| `namespace` | none | Tool/prompt name prefix (`{namespace}__{name}`). Omitted → passthrough; only valid with a single upstream. |
| `protocol` | `session` | `"session"` negotiates the sessionful handshake, required to bridge server-initiated traffic (sampling, elicitation, logging, progress, resource updates). `"auto"`/`"2026-07-28"` lets the SDK prefer the stateless 2026 protocol, which cannot carry server-initiated requests. |
| `owner` | none | `{ org, team, contact }` — surfaces in audit and health. |
| `labels` | none | Free-form string map for reporting. |
| `auth` | `{"strategy":"none"}` | Upstream credential strategy — see below. |
| `timeouts` | `5s/60s` | `connectMs`, `requestMs`. |
| `circuitBreaker` | `5 / 30s` | `failureThreshold` consecutive failures open the circuit; a probe is admitted after `halfOpenAfterMs`. |
| `rateLimit` | none | `{ requestsPerMinute }` for this upstream only. |
| `healthCheck` | none | `{ intervalMs }` — actively probe every endpoint (full MCP connect) on this interval, ejecting dead replicas before client traffic hits them and restoring recovered ones immediately. Absent → passive health (connect failures eject, cooldown restores). |
| `cacheTtlMs` | `30000` | TTL for cached list results. Negative disables caching. |

List freshness works end to end: when an upstream emits a `list_changed` notification, the gateway invalidates its cache **and re-emits the notification to every connected client**, so clients refetch and see the change immediately. TTLs remain the backstop when no notification arrives.

### Upstream auth strategies

| Strategy | Fields | When |
|---|---|---|
| `none` | — | Trusted network, no upstream auth. |
| `static` | `secretRef`, `header?`, `scheme?` | API-key upstreams. `secretRef` names an environment variable. |
| `passthrough` | — | Forwards the client's Bearer token as-is. Upstreams doing strict RFC 8707 audience checks will reject it — prefer token-exchange. |
| `client-credentials` | `tokenEndpoint`, `clientId`, `clientAuth`, `scopes?`, `resource?` | Service identity per upstream. Tokens cached until 60s before expiry. |
| `token-exchange` | `tokenEndpoint`, `clientId`, `clientAuth`, `audience`, `scopes?` | RFC 8693 — exchanges the caller's token for an upstream-audience token, preserving user identity end-to-end. **Recommended enterprise default.** Cached per (upstream, subject). |

`passthrough` and `token-exchange` derive per-principal credentials, so they require `auth.mode: "required"` — without a verified caller identity there is no subject to exchange for, and passthrough would forward whatever header an anonymous caller supplied.

`clientAuth`: `{ "type": "client_secret_post" | "client_secret_basic", "secretRef": "..." }`. Token endpoints must use `https` (loopback exempt). Upstream credentials are attached per request and bound to the configured upstream host: the gateway refuses cross-host redirects and never re-attaches a credential to another host, so a hostile upstream cannot capture the API key (or a passthrough caller's token) with a 3xx. Exchanged tokens are cached per `(upstream, issuer, subject)`. List results are not cached for `passthrough`/`token-exchange` upstreams, since those may be per-user.

### `auth` (gateway authentication)

```jsonc
{
  "mode": "required",                     // "disabled" (default) | "required"
  "resource": "https://gw.example.com",   // canonical resource URI = required token audience (RFC 8707)
  "issuers": [
    {
      "issuer": "https://acme.okta.com",
      "jwksUri": "https://acme.okta.com/oauth2/v1/keys",  // default: {issuer}/.well-known/jwks.json
      "groupsClaim": "groups"             // Okta "groups", Entra "roles", Auth0 custom-namespaced
    }
  ]
}
```

With `mode: "required"`, every `/mcp` request needs a valid Bearer token: trusted issuer (checked before any network I/O), verified signature via cached JWKS, exact audience match, a non-empty `sub`, asymmetric algorithms only (RS/ES/EdDSA). Failures answer 401 with a `WWW-Authenticate` challenge pointing at `/.well-known/oauth-protected-resource` (RFC 9728), which the gateway publishes. Issuer and JWKS URLs must use `https` (loopback exempt) — they are the inbound trust anchor. The JWKS fetch is single-flighted, size-bounded, and timeout-bounded so an unauthenticated flood of unknown-`kid` tokens cannot be amplified into requests against the IdP.

#### `auth.ema` (Enterprise-Managed Authorization)

fold can embed a deliberately one-grant-wide MCP Authorization Server: `POST /oauth/token` exchanges an enterprise-IdP-issued **ID-JAG** (Identity Assertion JWT Authorization Grant, RFC 7523 `jwt-bearer`) for a short-lived fold-signed access token. Everything the gateway then accepts has `aud` = fold, which keeps upstream token exchange coherent.

```jsonc
{
  "mode": "required",
  "resource": "https://gw.example.com",
  "issuers": [
    { "issuer": "https://acme.okta.com", "mode": "exchange" }  // ID-JAGs only — never accepted directly
  ],
  "ema": {
    "idpIssuer": "https://acme.okta.com",
    "idpJwksUri": "https://acme.okta.com/oauth2/v1/keys",  // default: {idpIssuer}/.well-known/jwks.json
    "signingKeyRef": "FOLD_EMA_KEY",   // env var: ES256 private key, PKCS#8 PEM
    "tokenTtlSec": 600,                // minted-token lifetime (default 600)
    "tokenRateLimitPerMinute": 600     // cap on the unauthenticated /oauth/token endpoint (default 600)
  }
}
```

An assertion must be issued by `idpIssuer` for the `resource` audience and carry `exp` and `jti`; each `jti` is single-use until it expires (recorded fleet-wide via Redis when configured), so a captured ID-JAG cannot be redeemed twice. Issuers with `mode: "exchange"` are excluded from direct token presentation and from the advertised `authorization_servers` — fold itself is the authorization server for those, publishing its minting key at `/.well-known/jwks.json` and announcing the `io.modelcontextprotocol/enterprise-managed-authorization` extension in the protected-resource metadata. The token endpoint is unauthenticated by design (the assertion is the credential) and rate-limited against amplification. Generate a key with `openssl ecparam -genkey -name prime256v1 | openssl pkcs8 -topk8 -nocrypt`.

### `policy`

```jsonc
{
  "defaultDecision": "deny",
  "rules": [
    {
      "id": "eng-github",
      "subjects": { "groups": ["engineering"] },   // and/or "subs"; omit → any principal
      "allow": [
        { "server": "github", "methods": ["tools/call"], "names": ["get_*", "create_pr"] },
        { "server": "search" }                     // all methods/names on that upstream
      ]
    }
  ]
}
```

First matching rule allows; otherwise `defaultDecision`. Policy governs named invocations (`tools/call`, `prompts/get`, `resources/read`), the completions and subscriptions derived from them (`completion/complete` is gated behind the prompt/resource it completes; `resources/subscribe` behind the resource), and it **filters list results per principal** — callers never see tools, prompts, or resources they cannot reach. Protocol plumbing (ping, the lists themselves) is not policy-gated; invisibility plus call-denial is the enforcement pair.

Scope a rule to specific token issuers with `"subjects": { "issuers": ["https://corp.okta.com"], "groups": [...] }`. Subjects and group names are only unique within an issuer, so **when more than one issuer is trusted, pin rules to an issuer** — otherwise a lower-assurance IdP could mint a principal that matches a rule written for another.

Attribute-based rules match on verified token claims: `"subjects": { "claims": { "dept": "eng", "mfa": true } }`. Every listed claim must match — the token claim equals the value, or, when the token carries an array (like an entitlements list), contains it. Values are JSON scalars (string, number, bool). Claims gate like issuers: they combine with `subs`/`groups` as an additional requirement, or stand alone as the whole subject. The same issuer-pinning caveat applies — claim names mean whatever each IdP says they mean, so pin claim-based rules to an issuer when more than one is trusted. Richer conditions (device posture, network location) belong in the IdP, surfaced to fold as claims — that is what token claims are for.

### `audit`

```jsonc
{ "sinks": [ { "type": "stdout" }, { "type": "webhook", "url": "https://siem.example.com/ingest", "headers": { "x-api-key": "..." } } ] }
```

One JSON event per terminal response — including 401s, 403-equivalents, and 429s — with principal, upstream, authz decision + rule id, outcome, and latency. Webhook delivery is asynchronous and batched so audit never adds request latency.

### `server`

| Field | Default | Notes |
|---|---|---|
| `mcpPath` | `/mcp` | Path the gateway serves MCP on. |
| `allowedHosts` | localhost set | DNS-rebinding protection: allowed Host/Origin hostnames. Set to your public hostname(s) in production, or `["*"]` only behind a trusted proxy. |
| `rateLimit` | none | Global `{ requestsPerMinute }` across all upstreams, plus optional `perPrincipalPerMinute` capping each authenticated principal on its own bucket (one tenant's flood cannot 429 the others). |
| `maxBodyBytes` | 1 MiB | Request body cap; larger bodies are answered `413` (chunked bodies are cut off at the cap). |
| `redisUrl` | `REDIS_URL` env | `redis://` URL sharing cache, rate-limit, and breaker state across gateway instances. Absent → in-process state. Redis outages fail open (bounded 500 ms per operation). |
| `console` | disabled | `{ "enabled": true }` serves the read-only fold console at `/console`: an observability dashboard (federation health, breaker and endpoint state, upstream source — static vs discovered — and credential-strategy names, discovery status, shared-state/audit/tracing facts) plus an MCP test console for tools, prompts, and resources that talks to the gateway's own `/mcp` endpoint — console traffic is governed and audited like any other client's. With auth enabled, the console's state API requires the same Bearer token as `/mcp`; add `"groups": ["platform-ops"]` to further restrict viewing to principals carrying one of those groups (403 otherwise, audited) — the fix for multi-tenant deployments where any valid token holder is too wide an audience. Add `"oauth": { "clientId": "fold-console" }` and the console signs users in with Authorization Code + PKCE against a trusted issuer (register `{origin}/console/` as the redirect URI at the IdP; `issuer` picks among multiple trusted issuers, `scopes` adds authorization scopes) instead of a pasted token. |

### `routing`

| Field | Default | Notes |
|---|---|---|
| `namespaceSeparator` | `__` | Separator between namespace and bare name in public tool/prompt names. Must not contain lowercase letters, digits, or hyphens (the namespace alphabet). |
| `pageSize` | `200` | Per-page bound on federated list results (tools, prompts, resources, templates, tasks). Fold merges and policy-filters every upstream's full list, then serves it in pages; cursors are opaque, bound to the calling principal, and expire when the underlying snapshot changes (the client receives `-32602` and restarts the list — `list_changed` notifications already prompt refetches). Negative disables pagination (single merged page). |

### `discovery`

```jsonc
{
  "url": "https://registry.internal/fold-upstreams.json",  // serves {"upstreams":[...]} — same schema as the static section
  "intervalMs": 30000,                                     // poll interval (default 30s); syncs once immediately at startup
  "bearerSecretRef": "FOLD_REGISTRY_TOKEN",                // optional: env var sent as a Bearer token on the poll
  "allowedAuthStrategies": ["static"],                     // optional: credential strategies discovered upstreams may carry (absent → unrestricted)
  "allowedSecretRefs": ["ML_SEARCH_API_KEY"],              // optional: env vars discovered upstreams may name in secretRef (absent → unrestricted)
  "allowedCredentialHosts": ["*.svc.cluster.local"],       // optional: where a credentialed discovered upstream may send secrets (url + tokenEndpoint hosts)
  "minHealthCheckIntervalMs": 1000                         // floor on discovered healthCheck.intervalMs (default 1000)
}
```

These are the gateway-side backstop for a partially trusted registry. Whoever controls the discovery source controls an upstream's `secretRef` names, its `tokenEndpoint`, **and** its destination URL — so naming a secret is only half the exposure. `allowedSecretRefs` bounds which secrets a discovered upstream may reference; `allowedCredentialHosts` bounds where a credentialed upstream may send them (both its endpoint hosts and its token endpoint). Patterns are exact hostnames or `*.suffix`, which matches subdomains only — list the apex separately if you mean it. Because the two halves are only meaningful together, **`allowedCredentialHosts` is required whenever `allowedAuthStrategies` or `allowedSecretRefs` permits credentials**; config validation rejects the half-configured combination rather than leaving the destination open. Any violation rejects the document whole and the last good set keeps serving. Set all three whenever the people who can register upstreams are not the people who operate the gateway (see [docs/security-model.md](docs/security-model.md)).

The URL decides where traffic routes and where upstream credentials attach, so it must use `https` (loopback exempt). Back it with whatever produces the document — on Kubernetes, [`fold-discovery`](docs/discovery-controller.md) does it out of the box: label a Service `fold.run/upstream: "true"` and it joins the federation. Any other producer works too — a service registry, a script writing to object storage. Each gateway instance polls independently; a consistent source keeps a fleet consistent.

### `tracing`

```jsonc
{
  "otlpEndpoint": "http://otel-collector:4318",  // OTLP/HTTP collector; a bare base URL gets the standard /v1/traces path
  "serviceName": "fold",                         // resource service.name (default "fold")
  "sampleRatio": 1.0                             // sampling for traces fold roots itself; parent-based, so sampled callers stay sampled
}
```

Absent, fold only propagates the caller's W3C trace context (see Observability). Present, fold emits its own spans: one server span per MCP request — carrying method, tool/prompt name, routed upstream, policy decision + rule id, principal, and outcome, the same fields as the audit event — and one client span per upstream call with its guard outcome (ok, rate-limited, circuit open, error). Spans export through a batching processor, so the request path never waits on the collector.

## Error codes

Gateway-minted JSON-RPC errors (upstream errors pass through verbatim):

| Code | Meaning |
|---|---|
| `-32040` | Per-upstream rate limit exceeded |
| `-32041` | Upstream unavailable (circuit open / unreachable / all upstreams down) |
| `-32042` | Policy denied the invocation |
| `-32043` | Name does not resolve to a configured namespace |
| `-32002` | Task id not owned by any upstream |

## Observability

- `GET /metrics` — Prometheus exposition. Gateway metrics: `fold_requests_total{method,outcome}`, `fold_request_duration_seconds{method}`, `fold_upstream_requests_total{upstream,outcome}`, `fold_upstream_request_duration_seconds{upstream}`, `fold_upstream_breaker_state{upstream}` (0 closed / 1 half-open / 2 open), `fold_upstream_endpoint_healthy{upstream,endpoint}` (multi-endpoint upstreams: 1 in rotation, 0 ejected), `fold_http_rejections_total{reason}`, `fold_discovery_syncs_total{outcome}`, `fold_build_info{version}`, plus standard Go process/runtime collectors.
- **Distributed tracing** — W3C Trace Context (`traceparent`/`tracestate`) headers on incoming requests are always propagated to the upstream calls they cause, so the gateway hop joins the caller's trace. With the `tracing` section configured, fold also emits its own OpenTelemetry spans (server span per request, client span per upstream call, pipeline outcomes as attributes) over OTLP/HTTP — the gateway hop appears *in* the trace instead of being invisible, and upstream calls parent under fold's client span while keeping the caller's trace id.
- **Latency, measurably** — the `bench` CI job gates every merge on added p50 latency < 5 ms through the proxy path (loose for shared runners). Typical local numbers (Apple Silicon, in-process upstream): **~0.20 ms added p50**, gateway p99 ≈ 0.57 ms. Reproduce: `make bench`.
- **Throughput, measurably** — `make loadtest` sweeps one instance under 8/64/256 concurrent SDK client sessions, direct vs through-fold: **~9,300 req/s `tools/call` at 64 connections (p99 ≤ 19 ms), 13,400 req/s at 256 (p99 ≤ 61 ms), zero errors** (Apple M4 Pro, loopback). Methodology, full tables, and the honest caveats: [docs/benchmarks.md](docs/benchmarks.md).
- **Structured logging** — operational events (startup, upstream connect/reconnect, session drops, circuit-breaker transitions, refused cross-host redirects, SSE-hang fallbacks, shutdown) log via `log/slog`. `--log-format text|json` and `--log-level debug|info|warn|error` on the CLI; embedders pass `gateway.WithLogger(*slog.Logger)`. Per-request accounting stays in metrics and the audit sink, not the log stream.

## Operational endpoints

- `GET /healthz` — pings every upstream concurrently; reports per-upstream connectivity, latency, breaker state, owner, and — for multi-endpoint upstreams — the balancer's per-endpoint rotation state. `503` when no upstream is reachable.
- `GET /metrics` — Prometheus metrics (see Observability).
- `GET /.well-known/oauth-protected-resource` — RFC 9728 metadata (when auth is enabled).
- `GET /console/` — the read-only fold console (when `server.console.enabled`): an observability dashboard plus an MCP test console. The test console is a plain MCP client against the gateway's own `/mcp`, so policy, rate limits, and audit apply to it like any other client — there is no privileged path.

## Guides

- [docs/deploy.md](docs/deploy.md) — Docker, compose, Helm, systemd; TLS/SSE fronting, `allowedHosts` and probes, hot reload in each shape, Redis for fleets, the production checklist.
- [docs/operations.md](docs/operations.md) — day-2 reference: every endpoint, metric, audit field, and error code, and how reloads, discovery, and probes surface in logs and metrics.
- [docs/security-model.md](docs/security-model.md) — the architecture: trust anchors, the inbound chain, the enforcement pair, credential containment, tenant isolation.
- [docs/discovery-controller.md](docs/discovery-controller.md) — `fold-discovery`, the Kubernetes producer: label a Service and it joins the federation.
- [docs/benchmarks.md](docs/benchmarks.md) — the latency gate and the throughput sweep: methodology, numbers, how to reproduce.
- [docs/embedding.md](docs/embedding.md) — the Go embedding surface, with CI-compiled examples.
- [docs/defaults.md](docs/defaults.md) — the v1.0 defaults review, every default a decision on record.

## Deploying

fold is a single static binary with no local state — see [docs/deploy.md](docs/deploy.md) for the full guide (TLS, `allowedHosts`, probes, Redis, secrets, audit shipping, production checklist).

- **Docker**: `ghcr.io/fold-run/fold` (multi-arch, distroless) — see [Quick start](#quick-start).
- **docker compose**: [`compose.yaml`](compose.yaml) with an optional Redis profile.
- **Kubernetes**: Helm chart in [`deploy/helm/fold`](deploy/helm/fold) — probes, config-as-ConfigMap, secrets via `envFrom`, optional Ingress/HPA/PDB/ServiceMonitor.
- **VM / bare metal**: prebuilt binaries on the [releases page](https://github.com/fold-run/fold/releases) plus a hardened systemd unit in the guide.

## Repository layout

| Path | Purpose |
|---|---|
| `cmd/fold` | The `fold` CLI |
| `cmd/fold-discovery` | The Kubernetes discovery-document producer (`internal/kubediscovery`) |
| `gateway` | Gateway engine: pipeline, federation routing, proxying, health |
| `config` | Config schema + validation |
| `auth` | OAuth resource server (JWKS verifier) + upstream credential strategies |
| `policy` | Allowlist policy engine + per-principal list filtering |
| `audit` | Audit events + sinks (stdout, webhook) |
| `docs` | Deploy, operations, security-model, embedding, and defaults guides |
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
make fuzz          # fuzz config parsing and namespace routing (seeds run in `make test`)
make conformance   # official conformance suite through the gateway (needs node)
```

CI runs these same targets (`.github/workflows/ci.yml`), plus `govulncheck` (`make vuln`).

The integration suite spins up real MCP servers from the official Go SDK behind the gateway and exercises federation, namespacing, policy filtering and denial, partial failure, credential injection (static and passthrough), rate limits, the breaker, JWT auth against a fixture JWKS issuer, RFC 9728 metadata, and the full server-initiated bridging loop (sampling, elicitation, logging, progress).

## API stability

This is the v1 compatibility contract, in force as of v1.0.0.

**Frozen at v1.0** (breaking changes only with a new major version):

- **The config document** — field names, meanings, defaults, and validation semantics. The machine-readable contract is [`config/fold.config.schema.json`](config/fold.config.schema.json) (`fold --schema`), kept in lockstep with the code by test. Defaults are part of the freeze — every one was reviewed as a deliberate decision before v1.0 ([`docs/defaults.md`](docs/defaults.md)).
- **The `fold` CLI** — flags, exit codes, and `FOLD_CONFIG` semantics.
- **The wire surface** — gateway-minted JSON-RPC error codes, HTTP endpoints (`/mcp`, `/healthz`, `/metrics`, `/.well-known/*`, `/oauth/token`), metric names and label sets, and the audit event JSON shape.
- **Go, for embedders** — the `gateway` package (`New`, `Option`, `WithLogger`, `Gateway.Handler/Reload/Close`, `Version`), the `config` package's document structs and `Load`/`Parse`/`Validate`/`Schema`, plus the contract types the gateway hands outward: `auth.Principal` with `WithPrincipal`/`PrincipalFromContext`, and `audit.Event`/`Outcome`. See the [package example](https://pkg.go.dev/github.com/fold-run/fold/gateway).

**Wiring, not API** (may change in any release): the constructors the gateway threads through its packages — `auth.Verifier`/`EMA`/`UpstreamCredentials`, `policy.Engine`, `audit.Logger`/`Sink`. They are exported so the gateway can reach them across package boundaries, not as an extension surface. `internal/` packages are never API.

**Upgrades and deprecation.** fold follows semver: within a major version, upgrades are drop-in — new config fields and capabilities arrive in minors, nothing frozen changes. Anything slated for removal is deprecated in a minor release (documented here and in the changelog, with the replacement) and removed no sooner than the next major. Security fixes land on the latest minor as patch releases (see [SECURITY.md](SECURITY.md)).

## Not implemented

Known gaps, documented deliberately:

- **SEP-2575 `subscriptions/listen` streams** — the Go SDK supports the 2026-07-28 protocol on its streamable HTTP server only in stateless mode, which fold cannot use: session-keyed bridging (sampling, elicitation, per-client streams) requires stateful sessions. Clients on the legacy handshake — which is what the SDK negotiates against stateful servers today — get full notification fan-in (list-changed and resource-updated, tested in `gateway/listen_test.go`). fold's fan-in already sits on the surfaces the SDK reuses for listen streams, and a drift canary in that test fails when the SDK lifts the restriction. Federated *tasks* (get/list/cancel/result/update with mint-affinity and probe fallback) **are** implemented — see above.
- **Content inspection (DLP / PII filtering / prompt-injection detection)** — deliberately out of scope. Inspecting request and response bodies means buffering and rewriting traffic, which conflicts with fold's invisibility rule (behavior through the gateway matches hitting the upstream directly) and its latency gate — and inline detection is a product of its own, not a gateway feature. fold's security model is structural instead: deny-by-default tool allowlists, per-principal invisibility, claim-gated (ABAC) rules, credential brokering so agents never hold upstream keys, and a full audit trail to feed the SIEM that does the detecting. If inline inspection becomes table stakes, the fold-shaped answer is an opt-in content-inspection hook (an external policy endpoint on the ingress/egress path), not built-in scanning.

## Changelog

### v1.4.0 — 2026-08-06

Console sign-in.

- **`server.console.oauth`** — the console signs users in with Authorization Code + PKCE against a trusted direct-mode issuer, replacing the pasted token (which remains the fallback, and the path for EMA deployments). The console is a public client: no secret exists, the S256 verifier is the proof, the access token lives in page memory only, and tokens are requested with the gateway as RFC 8707 resource — what the flow mints is exactly what `/mcp` verifies and audits. A deliberately unauthenticated `/console/api/auth` hint serves the public client configuration, and the asset CSP admits exactly the configured issuer's origin in `connect-src` — config-derived, never a wildcard. Register `{origin}/console/` as the redirect URI at the IdP.

### v1.3.0 — 2026-08-04

Console refinements and the viewer allowlist.

- **`server.console.groups`** — the console's viewer allowlist: when set, the state API answers an audited 403 to any authenticated principal not carrying an allowlisted group, closing the "any valid token holder sees the topology" caveat for multi-tenant deployments. Requires `auth.mode: "required"`; static assets stay open (they carry no data).
- **Test console covers the full invocable surface** — tools, prompts, *and* resources, with cursor-paginated lists; `resources/read` shows URIs passing through un-rewritten.
- **Deeper dashboard** — each upstream's source (static vs discovered) and credential-strategy name, plus deployment facts: shared-state backend, audit sink types, tracing and EMA enablement, rate budgets, routing settings. Secret values and the Redis URL never appear.
- **UI polish** — breaker-state color coding, contrast and focus-state fixes, empty states, label chips, and the token field hides itself when auth is off.

### v1.2.0 — 2026-08-04

The fold console.

- **`server.console`** — an embedded, read-only console at `/console` (default off): an observability dashboard — federation health, breaker and per-endpoint rotation state, discovery sync status — plus an MCP test console. The test console is a plain MCP client against the gateway's own `/mcp`, so policy filters what it lists, denials answer `-32042`, rate limits apply, and every call is audited; there is no privileged path. Assets are hand-written and embedded in the binary (no build step, no external fetches — CSP-pinned). The state API authenticates like `/mcp` and shares its rate budgets; with auth on, any valid principal sees the federation topology while raw connect errors are reduced to a category (decision on record in [docs/security-model.md](docs/security-model.md)).

### v1.1.0 — 2026-08-04

Self-serve federation on Kubernetes. Includes the v1.0.1 security fix.

- **`fold-discovery`** — the producer half of dynamic discovery: label a Service `fold.run/upstream: "true"` and its tools appear behind the gateway, no fold config change. It lists Services over the plain Kubernetes API with the pod's service account (no client-go dependency), maps them via `fold.run/*` annotations, and serves the document the gateway already polls. Ships as its own binary, image (`ghcr.io/fold-run/fold-discovery`), and manifest — see [docs/discovery-controller.md](docs/discovery-controller.md).
- **Registration bounds** — labeling rights are registration rights, so both sides bound them: the producer is default-deny on credentialed strategies and secret references, identities are namespace-prefixed, and contested claims drop every claimant. The gateway independently enforces `discovery.allowedAuthStrategies`, `allowedSecretRefs`, and `allowedCredentialHosts`, rejecting a violating document whole.
- **Security fix (also in v1.0.1)** — the token-endpoint client no longer follows redirects; see below.

### v1.0.1 — 2026-08-04

**Security fix**, released from the `release-1.0` branch as a drop-in patch. The token-endpoint client followed HTTP redirects, and Go replays POST bodies on 307/308 — so a redirecting token endpoint handed the grant to the redirect target: the client secret under `client_secret_post`, and the caller's own bearer token as `subject_token` under `token-exchange`. Affects any upstream using `client-credentials` or `token-exchange` whose token endpoint can be made to redirect. The client now refuses every redirect and the grant fails closed.

### v1.0.0 — 2026-08-04

The compatibility contract is in force. No behavior changes from v0.8.0 — this release is the promise: the config document (schema-verified), CLI, wire surface (error codes, endpoints, metric names, audit shape), and embedder Go API are frozen; breaking changes now require a new major version. See [API stability](#api-stability) for the contract and [SECURITY.md](SECURITY.md) for the support policy.

### v0.8.0 — 2026-08-04

The 1.0 readiness release — hardening and documentation, no behavior changes.

- **Hardening** — fuzzers for the untrusted parsers (pagination cursors, discovery documents; ~1M exploratory execs, clean) and a churn test interleaving reloads, discovery flapping, health probes, and concurrent traffic under the race detector.
- **Decisions on record** — the defaults review ([docs/defaults.md](docs/defaults.md)); supported-versions and deprecation policy in [SECURITY.md](SECURITY.md) and README.
- **Guides** — new [operations](docs/operations.md), [security-model](docs/security-model.md), and [embedding](docs/embedding.md) docs (embedding examples compile in CI), plus a deploy-guide accuracy pass with hot-reload coverage.

### v0.7.0 — 2026-08-04

The road to v1.0.

- **Config JSON Schema** — [`config/fold.config.schema.json`](config/fold.config.schema.json) is the machine-readable structural contract for the config document, printed by `fold --schema`, shipped in release archives, and kept in lockstep with the code by test. Point your editor at it for completion and validation.
- **The v1 compatibility contract** — README "API stability" now states exactly what v1.0 will freeze (config document, CLI, wire surface, embedder Go API) and what remains internal wiring.

### v0.6.0 — 2026-08-04

Self-serve federation.

- **Dynamic upstream discovery** — fold polls `discovery.url` for an upstreams document and hot-swaps the discovered set into the federation alongside the static config: a team ships an MCP server, the registry lists it, and it appears behind the gateway. Fail-safe (a bad document or dead source keeps the last good set) and composable with base config reloads.
- **`tasks/list` pagination** — the merged federated task list now pages with the same principal-bound snapshot-offset cursors as the typed lists, in deterministic id order.

### v0.5.0 — 2026-08-04

Running a federation in production: balancing, live reconfiguration, and deeper observability.

- **Load-balanced upstreams** — an upstream can list replica `urls`; new sessions balance round-robin with connect failover, and optional active health probes (`healthCheck.intervalMs`) eject dead replicas before client traffic hits them.
- **Hot config reload** — `SIGHUP`, `--watch`, or `Gateway.Reload` apply a new config without dropping the listener; unchanged upstreams keep their live sessions, and clients are nudged to refetch via `list_changed`.
- **First-party OpenTelemetry tracing** — `tracing.otlpEndpoint` adds a server span per request and a client span per upstream call, carrying the same fields as the audit event; W3C trace propagation remains always-on.
- **ABAC policy** — rules can gate on verified token claims (`subjects.claims`), composing with groups/subjects or standing alone.
- **Composite federated pagination** — merged lists serve in pages with principal-bound snapshot cursors (`routing.pageSize`).
- **Deployment assets** — Helm chart, compose file, and a deployment guide.

Full history: [releases](https://github.com/fold-run/fold/releases).

## License

Apache-2.0
