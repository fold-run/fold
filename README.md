# fold

[![CI](https://github.com/fold-run/fold/actions/workflows/ci.yml/badge.svg)](https://github.com/fold-run/fold/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/fold-run/fold.svg)](https://pkg.go.dev/github.com/fold-run/fold)
[![Release](https://img.shields.io/github/v/release/fold-run/fold)](https://github.com/fold-run/fold/releases)

**fold: the enterprise MCP gateway — one governed endpoint between every MCP client and every MCP server.**

fold sits in front of any number of upstream MCP servers — in any language, on any SDK, from any team or vendor — providing federation, enterprise auth, policy, caching, rate limiting, and audit. It is built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), so the wire protocol (streamable HTTP, both request/response and SSE) is the SDK's own implementation on both the client-facing and upstream-facing sides.

**Conformant, provably.** The official [`@modelcontextprotocol/conformance`](https://github.com/modelcontextprotocol/conformance) suite runs against fold fronting the reference everything-server on every merge — **40/40 checks**, including sampling, elicitation, logging, progress, and resource subscriptions bridged through the gateway. The receipt is one click away: the [`conformance` job from the v1.9.0 release run](https://github.com/fold-run/fold/actions/runs/31498966523/job/93803686320), and [every green run on `main`](https://github.com/fold-run/fold/actions/workflows/ci.yml?query=branch%3Amain+is%3Asuccess). A [weekly job](.github/workflows/drift.yml) re-runs the suite against the *latest* unpinned SDK and opens a tracking issue the moment anything drifts. Reproduce it yourself with `./scripts/conformance.sh` — the same command CI runs.

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

**Replicated upstreams load-balance.** Give an upstream `urls` instead of `url` and the gateway balances across the replicas: MCP is sessionful, so each new session — the shared root session, and each client's bridged session — connects round-robin to the next healthy endpoint and stays pinned there. An endpoint that refuses a connection is skipped (the connect fails over to the next replica in the same attempt) and rests out of rotation for the breaker's `halfOpenAfterMs` before being retried. Add `"healthCheck": { "intervalMs": 30000 }` for active probing: the gateway connects to every endpoint on the interval, so a dead replica is ejected before the first client request and a recovered one returns to rotation without a live-request retry paying for the discovery. Per-endpoint health shows in `/health` and the `fold_upstream_endpoint_healthy` metric. Replicas are assumed identical — one namespace, one credential strategy, one policy surface.

**Configuration hot-reloads.** `kill -HUP` the process — or run with `--watch` to poll the config file — and fold revalidates and applies the new document without dropping the listener: the upstream set and the policy engine swap atomically, in-flight requests finish against the snapshot they started on, and connected clients receive `list_changed` notifications so they refetch. Upstreams whose config is unchanged keep their live sessions; removed or changed ones are drained (closed after their request timeout) and a changed upstream's resource subscriptions move to its replacement. Embedders get the same behavior via `gw.Reload(cfg)`. The `auth`, `server`, `routing`, `audit`, `tracing`, and `discovery` sections are wired in at construction and cannot hot-swap — changing them makes the reload fail loudly, keeping the running configuration; a rejected reload never takes anything down.

**Upstreams can be discovered.** With the `discovery` section configured, fold polls a URL for `{"upstreams": [...]}` (same schema as the static section) and hot-swaps the discovered set into the federation alongside the statically configured upstreams — a team ships an MCP server, the registry lists it, and it appears behind the gateway without anyone touching fold's config. Discovery composes with reload: base reloads keep the discovered set, discovery syncs keep the base, and both flow through the same validated atomic swap. Fail-safe by construction: an unreachable source, a malformed document, or one that collides with a static upstream id or namespace is rejected whole and the last good set keeps serving (`fold_discovery_syncs_total` counts outcomes).

## Request pipeline

```
POST /mcp
 → host validation      DNS-rebinding protection (Host/Origin allowlist)
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

**Server-initiated traffic bridges both ways.** Named invocations run over a per-client upstream session whose handlers forward sampling (`sampling/createMessage`), elicitation, log messages, and progress notifications back to the originating client — routed over that call's own stream, so clients without a standalone SSE stream still hear them. `resources/subscribe` is forwarded to the owning upstream and `resources/updated` notifications fan back out to subscribed clients; `completion/complete` routes by prompt namespace or resource ownership; a client's `logging/setLevel` propagates to its upstream sessions. Idle per-client sessions are swept after 5 minutes, and the same sweep releases subscriptions whose downstream client disconnected without unsubscribing — the one shared upstream subscription per URI is dropped when its last live subscriber is gone.

**Federated tasks.** Task ids are opaque and clients persist them across sessions, so — like resource URIs — fold never rewrites them; ownership is remembered instead. A `tools/call` that mints a task (advertised in the result `_meta`) pins `taskId → upstream`; `tasks/get`, `tasks/cancel`, `tasks/result`, and `tasks/update` route to that owner, whose errors pass through verbatim. A task fold never saw (minted out of band, or whose record has expired) is located by a read-only `tasks/get` probe across upstreams — the owner answers, everyone else is a healthy "no" — after which the mutating method is sent to the owner alone. `tasks/list` merges every upstream in deterministic id order and pages like the typed lists (`routing.pageSize`); ids no upstream knows answer `-32002`. Ownership is bound to the minting principal: another caller's task-scoped calls answer `-32002` exactly like an unknown id (no existence leak, no probe), and `tasks/list` shows each principal only its own tasks. The ownership index is an authorization record, not a routing hint, so it lives behind `state.Provider`: with `REDIS_URL` (or `server.redisUrl`) set, a whole fleet reads the same ownership and the binding survives a rolling restart — a caller cannot reach another principal's task by landing on an instance that did not serve the mint. Records are keyed by a digest of the task id, hold a digest of the owning principal, and expire after 24 hours; a Redis outage falls back to that instance's locally mirrored records. Tasks fold has no ownership record for (out of band, expired, or minted on a fleet with no shared state) stay reachable by any caller via the probe fallback; anonymous callers share one owner bucket, so no-auth deployments are unaffected. The Go SDK does not yet model the task lifecycle, so these are forwarded as opaque JSON via the SDK's custom-method mechanism to fold's task contract; the wire types are gateway-local and swap for the SDK's typed task API when it ships. `subscriptions/listen` fan-in is not included (the SDK exposes no public API for it).

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
| `budget` | none | `{ period, upstreamCalls }` — a consumption allowance that **accumulates** until the calendar period rolls over, unlike `rateLimit`, which smooths a burst and forgets it. `period` is `hour`, `day`, or `month` (default), aligned to UTC. Requires shared state to mean anything across a fleet; without it each instance enforces its own allowance and the gateway warns at startup. |
| `healthCheck` | none | `{ intervalMs }` — actively probe every endpoint (full MCP connect) on this interval, ejecting dead replicas before client traffic hits them and restoring recovered ones immediately. Absent → passive health (connect failures eject, cooldown restores). |
| `cacheTtlMs` | `30000` | TTL for cached list results. Negative disables caching. |

List freshness works end to end: when an upstream emits a `list_changed` notification, the gateway invalidates its cache **and re-emits the notification to every connected client**, so clients refetch and see the change immediately. TTLs remain the backstop when no notification arrives.

**Stdio servers.** `url` is always an HTTP endpoint — the gateway never runs a process. To federate an MCP server that speaks stdio (which is most of them), put [`fold-stdio`](docs/stdio.md) in front of it: it runs the server and exposes it over streamable HTTP, so the upstream entry is an ordinary `url` and every strategy, guard, and policy rule above applies unchanged. The command is fixed at the shim's argv and never travels over the network — which is why stdio is not a field here. See [docs/design-stdio.md](docs/design-stdio.md) for why the process supervision lives in a sidecar rather than in the gateway.

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

Add `"maxItems": 40` to a rule to bound how many list items it may make visible in one response — a guardrail against handing an agent a thousand tools, which is context paid for on every turn. It is a bound, not a curation: fold drops whatever falls past the cap in merge order, because it has no notion of which tools matter, and acquiring one would be the semantic tool selection this project [declines](docs/roadmap.md#non-goals). A truncated list says so in the result's `_meta["run.fold/truncated"]`, in the audit event's `itemsCapped`, and in `fold_list_items_total{stage="capped"}` — a cap that hid capability silently would be worse than no cap. **The cap bounds visibility, not authority**: a name it withheld from a list is still callable if policy allows it.

First matching rule allows; otherwise `defaultDecision`. Policy governs named invocations (`tools/call`, `prompts/get`, `resources/read`), the completions and subscriptions derived from them (`completion/complete` is gated behind the prompt/resource it completes; `resources/subscribe` behind the resource), and it **filters list results per principal** — callers never see tools, prompts, or resources they cannot reach. Protocol plumbing (ping, the lists themselves) is not policy-gated; invisibility plus call-denial is the enforcement pair.

Scope a rule to specific token issuers with `"subjects": { "issuers": ["https://corp.okta.com"], "groups": [...] }`. Subjects and group names are only unique within an issuer, so **when more than one issuer is trusted, pin rules to an issuer** — otherwise a lower-assurance IdP could mint a principal that matches a rule written for another.

Attribute-based rules match on verified token claims: `"subjects": { "claims": { "dept": "eng", "mfa": true } }`. Every listed claim must match — the token claim equals the value, or, when the token carries an array (like an entitlements list), contains it. Values are JSON scalars (string, number, bool). Claims gate like issuers: they combine with `subs`/`groups` as an additional requirement, or stand alone as the whole subject. The same issuer-pinning caveat applies — claim names mean whatever each IdP says they mean, so pin claim-based rules to an issuer when more than one is trusted. Richer conditions (device posture, network location) belong in the IdP, surfaced to fold as claims — that is what token claims are for.

### `audit`

```jsonc
{
  "sinks": [
    { "type": "stdout" },
    { "type": "file", "path": "/var/log/fold/audit.jsonl", "maxSizeMb": 100, "maxFiles": 5 },
    { "type": "otlp-logs", "url": "http://otel-collector:4318" },
    {
      "type": "webhook",
      "url": "https://siem.example.com/ingest",
      "headers": { "x-api-key": "..." },
      "retry": { "maxAttempts": 4, "initialBackoffMs": 500, "maxBackoffMs": 30000 },
      "deadLetterPath": "/var/log/fold/audit-dead.jsonl"
    }
  ]
}
```

One JSON event per terminal response — including 401s, 403-equivalents, and 429s — with principal, upstream, tenant, authz decision + rule id, outcome, and latency. Delivery is asynchronous and batched, so audit never adds request latency.

| Sink | Fields | Notes |
|---|---|---|
| `stdout` | — | One JSON line per event. |
| `file` | `path`, `maxSizeMb` (100), `maxFiles` (5) | One JSON line per event, rotated by size. Rotation renames in place (`audit.jsonl` → `.1` → `.2`), so a tail or log shipper watching the live name survives it, and the file count is bounded — a gateway that fills its disk with its own audit trail has found a novel way to stop serving. |
| `webhook` | `url`, `headers`, `retry`, `deadLetterPath` | Batched POSTs. Redirects are refused outright: the request carries the sink's token and records naming principals and tools. |
| `otlp-logs` | `url`, `headers`, `retry`, `deadLetterPath` | OpenTelemetry log records over OTLP/HTTP, via the OTel SDK's own exporter — the wire format is not hand-rolled. A bare base URL (`http://collector:4318`) gets OTLP's `/v1/logs` path, matching the `tracing` section's convention. The event's fields become attributes under the same names fold's spans use (`mcp.method`, `fold.upstream`, `fold.tenant`, `enduser.id`), so a trace and its audit record join on the same keys; the body is a short summary, and the outcome sets severity — a refusal is `WARN`, not `ERROR`, because policy denying a call is the gateway working. |

**Delivery is retried, and what it cannot deliver is kept.** A failing POST is retried with exponential backoff and equal jitter — `maxAttempts` 4, `initialBackoffMs` 500, `maxBackoffMs` 30000 by default, so a receiver restarting costs nothing. Retry is on without configuration, because the alternative is losing exactly the events someone will later go looking for. A `4xx` other than `429` is not retried: a receiver that rejects the payload will reject it identically four times. When attempts run out, events are appended to `deadLetterPath` for replay; without one they are counted and gone.

**Losses are visible.** `fold_audit_events_total{sink,outcome}` counts `delivered`, `retried`, `dead_lettered`, and `dropped` — the audit trail cannot report its own gaps, so this is where a gap shows up. The packaged alert `FoldAuditEventsLost` fires on either kind of loss ([observability](docs/operations.md#dashboards-alerts-and-slos)).

### `server`

| Field | Default | Notes |
|---|---|---|
| `mcpPath` | `/mcp` | Path the gateway serves MCP on. |
| `allowedHosts` | localhost set | DNS-rebinding protection: allowed Host/Origin hostnames. Set to your public hostname(s) in production, or `["*"]` only behind a trusted proxy. |
| `rateLimit` | none | Global `{ requestsPerMinute }` across all upstreams, plus optional `perPrincipalPerMinute` capping each authenticated principal on its own bucket, so one caller's flood cannot 429 the others. For a bucket shared by a *team* rather than one per person, see [`tenants`](#tenants). |
| `budget` | none | `{ period, upstreamCalls }` — a consumption allowance across every upstream, accumulating until the calendar period rolls over. Like the rest of this section it is construction-wired: a reload rejects a change to it, so an allowance cannot be widened under a running gateway. |
| `maxBodyBytes` | 1 MiB | Request body cap; larger bodies are answered `413` (chunked bodies are cut off at the cap). |
| `redisUrl` | `REDIS_URL` env | `redis://` URL sharing cache, rate-limit, and breaker state across gateway instances. Absent → in-process state. Redis outages fail open (bounded 500 ms per operation). |
| `metricsAddr` | unset | Moves `/metrics` (and `/health`) to their own listener, e.g. `":9090"`. Absent, they stay on the main port behind `allowedHosts` — which is why a scraper arriving as a pod IP or a service name gets `403` and reads as "target down". A separate listener is the arrangement to prefer whenever something other than the gateway's own host scrapes it: it is not an origin a browser can be steered to, so it needs no Host allowlist, while the public port stops exposing upstream ids, namespaces, tenant ids, and endpoint URLs to a rebinding attempt. **Bind it to an internal interface** — network scope is what protects it. Construction-wired; the Helm chart sets it for you via `metrics.listener.enabled`. |
| `introspection` | disabled | `{ "enabled": true }` serves the read-only APIs: `GET /api/federation` (the federation snapshot — health, breaker and endpoint state, upstream source — static vs discovered — and credential-strategy names, discovery status, shared-state/audit/tracing facts, and the viewer's tenant governance) and `GET /api/auth-hint` (the unauthenticated sign-in hint). With auth enabled, `/api/federation` requires the same Bearer token as `/mcp` and shares its rate budgets; add `"groups": ["platform-ops"]` to further restrict reading to principals carrying one of those groups (403 otherwise, audited) — the fix for deployments where any valid token holder is too wide an audience. (A viewer who resolves to a tenant sees that tenant's federation rather than the whole one; see [`tenants`](#tenants).) |
| `console` | disabled | `{ "enabled": true }` serves the read-only fold console page at `/console`: an observability dashboard plus an MCP test console for tools, prompts, and resources that talks to the gateway's own `/mcp` endpoint — console traffic is governed and audited like any other client's. The dashboard renders what `/api/federation` reports, so it requires `introspection.enabled`. Add `"oauth": { "clientId": "fold-console" }` and the console signs users in with Authorization Code + PKCE against a trusted issuer (register `{origin}/console/` as the redirect URI at the IdP; `issuer` picks among multiple trusted issuers, `scopes` adds authorization scopes) instead of a pasted token. The page's assets are maintained in [fold-run/fold-console](https://github.com/fold-run/fold-console) and vendored here at a pinned commit. |

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

### `tenants`

Groups principals for governance. A tenant is a label on identity, resolved from claims the IdP already asserts — it never travels alongside a token and is never a trust anchor. **A tenant groups principals; it does not authenticate them**, and policy remains the authority on what may be invoked. Reloadable, unlike `server.budget`: tenants change when a customer signs up.

```jsonc
"tenants": [
  {
    "id": "acme",
    "subjects": { "claims": { "org_id": "acme-prod" } },  // same shape policy rules use
    "budget": { "period": "month", "upstreamCalls": 500000 },
    "rateLimit": { "requestsPerMinute": 2000 },           // one bucket for the whole tenant
    "upstreams": ["billing", "crm"]                        // optional: all upstreams if omitted
  }
]
```

| Field | Default | Notes |
|---|---|---|
| `id` | — | Lowercase alphanumeric + hyphens. Appears in every audit event the tenant's principals produce, and as the `tenant` label on `fold_tenant_*` metrics. |
| `subjects` | — | Required. Which principals belong, using the same shape policy rules use (`groups`, `subs`, `issuers`, `claims`). A tenant with no selector would capture every caller, so it is rejected. |
| `budget` | none | `{ period, upstreamCalls }` for the tenant as a whole — the dimension a per-upstream or server-wide budget cannot express. Charged in upstream invocations like every other budget, only for calls that reach an upstream; exhaustion mints `-32044` naming the tenant. |
| `rateLimit` | none | `{ requestsPerMinute }`, one bucket shared by the tenant's principals; over it, `429` with `Retry-After`. Distinct from `server.rateLimit.perPrincipalPerMinute`, which gives each *person* a bucket: ten agents on one team get ten allowances there and one here. |
| `upstreams` | all | Optional visibility subset by upstream id, evaluated before policy. |

**How the four pieces behave.** The budget and the server's are charged narrowest-first (upstream → tenant → server), so a refusal never spends a wider allowance. The rate limit is enforced with its siblings before routing, widest-first (global → tenant → per-principal). The visibility subset filters the *fan-out*: an upstream outside it is never asked, so it costs no request, no budget, and no partial-failure entry when it is down — and a named invocation against it is refused before the policy engine sees it, with `-32042` (`tasks/*` answer "no upstream owns that id" instead, because there a refusal must not reveal existence). A viewer's console shows their tenant's federation, not the operator's.

A principal belongs to at most one tenant. Overlap that validation cannot decide statically — two selectors that only collide for some principals — is caught at request time and **refused**, not guessed: assigning a caller by precedence would hand them another tenant's allowance and visibility. An unmatched principal has no tenant and is governed exactly as before tenancy existed, so an existing deployment behaves identically until it declares one.

Resolution is a map lookup for the two selector shapes a large document repeats — one claim equalling one value, or one group — so a document with ten thousand tenants resolves as fast as one with ten ([benchmarks](docs/benchmarks.md#tenant-resolution-cardinality)); compound selectors still scan, so keep those in the tens. Design record: [docs/design-tenancy.md](docs/design-tenancy.md).

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
| `-32044` | Consumption budget exhausted for the period (server or per-upstream) |
| `-32002` | Task id not owned by any upstream |

## Observability

- `GET /metrics` — Prometheus exposition. Gateway metrics: `fold_requests_total{method,outcome}`, `fold_request_duration_seconds{method}`, `fold_upstream_requests_total{upstream,outcome}`, `fold_upstream_request_duration_seconds{upstream}`, `fold_upstream_breaker_state{upstream}` (0 closed / 1 half-open / 2 open), `fold_upstream_endpoint_healthy{upstream,endpoint}` (multi-endpoint upstreams: 1 in rotation, 0 ejected), `fold_http_rejections_total{reason}`, `fold_discovery_syncs_total{outcome}`, `fold_list_items_total{method,stage}` (offered / served / capped — the context reduction per-principal filtering performs, as a ratio with a denominator), `fold_build_info{version}`, plus standard Go process/runtime collectors. With `tenants` configured, two more carry the tenant dimension: `fold_tenant_requests_total{tenant,outcome}` and `fold_tenant_upstream_calls_total{tenant}` — the second counts the unit a tenant budget is charged in, so an allowance can be watched being spent. They are separate series rather than a `tenant` label on the metrics above, because label sets are frozen by the [compatibility contract](#api-stability).
- **Packaged interpretation, not just data** — a Grafana dashboard, a `PrometheusRule` set, and documented SLOs ship in the chart (`metrics.dashboard.enabled`, `metrics.prometheusRule.enabled`). The availability SLO deliberately counts only outcomes fold *failed*: a denial, a rate limit, and an exhausted budget are the gateway working as configured, and an SLO that counted them would page someone for tightening a policy. A test keeps every panel and alert in lockstep with the metric names the code registers. See [docs/operations.md](docs/operations.md#dashboards-alerts-and-slos).
- **Distributed tracing** — W3C Trace Context (`traceparent`/`tracestate`) headers on incoming requests are always propagated to the upstream calls they cause, so the gateway hop joins the caller's trace. With the `tracing` section configured, fold also emits its own OpenTelemetry spans (server span per request, client span per upstream call, pipeline outcomes as attributes) over OTLP/HTTP — the gateway hop appears *in* the trace instead of being invisible, and upstream calls parent under fold's client span while keeping the caller's trace id.
- **Latency, measurably** — the `bench` CI job gates every merge on added p50 latency < 5 ms through the proxy path (loose for shared runners). Typical local numbers (Apple Silicon, in-process upstream): **~0.20 ms added p50**, gateway p99 ≈ 0.57 ms. Reproduce: `make bench`.
- **Throughput, measurably** — `make loadtest` sweeps one instance under 8/64/256 concurrent SDK client sessions, direct vs through-fold: **~9,300 req/s `tools/call` at 64 connections (p99 ≤ 19 ms), 13,400 req/s at 256 (p99 ≤ 61 ms), zero errors** (Apple M4 Pro, loopback). Methodology, full tables, and the honest caveats: [docs/benchmarks.md](docs/benchmarks.md).
- **Structured logging** — operational events (startup, upstream connect/reconnect, session drops, circuit-breaker transitions, refused cross-host redirects, SSE-hang fallbacks, shutdown) log via `log/slog`. `--log-format text|json` and `--log-level debug|info|warn|error` on the CLI; embedders pass `gateway.WithLogger(*slog.Logger)`. Per-request accounting stays in metrics and the audit sink, not the log stream.

## Operational endpoints

- `GET /health` — pings every upstream concurrently; reports per-upstream connectivity, latency, breaker state, owner, and — for multi-endpoint upstreams — the balancer's per-endpoint rotation state. `503` when no upstream is reachable. (`/healthz`, the pre-v1.5 path, was removed in v1.9 — see [API stability](#api-stability).) The fan-out is shared: concurrent callers ride one collection and the result is reused for a second, so probing this unauthenticated endpoint in a loop cannot multiply into upstream traffic. A reload or discovery sync invalidates it immediately.
- `GET /metrics` — Prometheus metrics (see Observability).
- `GET /.well-known/oauth-protected-resource` — RFC 9728 metadata (when auth is enabled).
- `GET /api/federation` — the federation snapshot (when `server.introspection.enabled`): upstream health and topology, policy shape, audit sinks, discovery status, and the viewer's tenant governance. Authenticates like `/mcp` and shares its rate budgets. Was `/console/api/state` through v1.8.
- `GET /api/auth-hint` — the deliberately unauthenticated sign-in hint (when `server.introspection.enabled`): issuer, client id, scopes, resource. Public SPA configuration only. Was `/console/api/auth` through v1.8.
- `GET /console/` — the read-only fold console page (when `server.console.enabled`): an observability dashboard over `/api/federation` plus an MCP test console. The test console is a plain MCP client against the gateway's own `/mcp`, so policy, rate limits, and audit apply to it like any other client — there is no privileged path.

## Guides

- [docs/deploy.md](docs/deploy.md) — Docker, compose, Helm, systemd; TLS/SSE fronting, `allowedHosts` and probes, hot reload in each shape, Redis for fleets, the production checklist.
- [docs/operations.md](docs/operations.md) — day-2 reference: every endpoint, metric, audit field, and error code, and how reloads, discovery, and probes surface in logs and metrics.
- [docs/security-model.md](docs/security-model.md) — the architecture: trust anchors, the inbound chain, the enforcement pair, credential containment, tenant isolation.
- [docs/discovery-controller.md](docs/discovery-controller.md) — `fold-discovery`, the Kubernetes producer: label a Service and it joins the federation.
- [docs/stdio.md](docs/stdio.md) — `fold-stdio`, the shim that puts a local stdio MCP server behind the gateway as an ordinary http upstream.
- [docs/benchmarks.md](docs/benchmarks.md) — the latency gate and the throughput sweep: methodology, numbers, how to reproduce.
- [docs/embedding.md](docs/embedding.md) — the Go embedding surface, with CI-compiled examples.
- [docs/defaults.md](docs/defaults.md) — the v1.0 defaults review, every default a decision on record.
- [docs/roadmap.md](docs/roadmap.md) — direction: what fold intends to build next, and what it deliberately declines.
- [docs/design-stdio.md](docs/design-stdio.md) — design record: why stdio upstreams arrive as a sidecar shim rather than subprocess supervision in the gateway.
- [docs/design-consumption.md](docs/design-consumption.md) — design record: quotas, budgets, and metering — what a gateway on the MCP path can honestly govern, and what it must refuse to guess.
- [docs/design-policy-depth.md](docs/design-policy-depth.md) — design record (proposed): deny rules, argument constraints, and destructive-operation gating — with the precedence, visibility, and trust questions each one raises settled before implementation.
- [docs/design-tenancy.md](docs/design-tenancy.md) — design record: the tenant object — a named set of principals and the governance that applies to them, why it never becomes a trust anchor, and the two corrections implementation forced on the design.

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
make fuzz          # fuzz config parsing and namespace routing (seeds run in `make test`)
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
- **The wire surface** — gateway-minted JSON-RPC error codes, HTTP endpoints (`/mcp`, `/health`, `/metrics`, `/api/federation`, `/api/auth-hint`, `/.well-known/*`, `/oauth/token`), metric names and label sets, and the audit event JSON shape. The `/api/federation` **response shape** is frozen with the rest: fields may be added within a major, none removed or renamed — it has an out-of-tree consumer that can skew against the gateway, which is what makes it a contract rather than an internal detail. `/healthz` was the health path through v1.4, a deprecated alias from v1.5.0, and **removed in v1.9.0**; point probes at `/health`.
- **Go, for embedders** — the `gateway` package (`New`, `Option`, `WithLogger`, `Gateway.Handler/Reload/Close`, `Version`), the `config` package's document structs and `Load`/`Parse`/`Validate`/`Schema`, plus the contract types the gateway hands outward: `auth.Principal` with `WithPrincipal`/`PrincipalFromContext`, and `audit.Event`/`Outcome`. See the [package example](https://pkg.go.dev/github.com/fold-run/fold/gateway).

**Wiring, not API** (may change in any release): the constructors the gateway threads through its packages — `auth.Verifier`/`EMA`/`UpstreamCredentials`, `policy.Engine`, `audit.Logger`/`Sink`. They are exported so the gateway can reach them across package boundaries, not as an extension surface. `internal/` packages are never API.

**Upgrades and deprecation.** fold follows semver: within a major version, upgrades are drop-in — new config fields and capabilities arrive in minors, nothing frozen changes. Anything slated for removal is deprecated in a minor release (documented here and in the changelog, with the replacement) and removed no sooner than the next major. Security fixes land on the latest minor as patch releases (see [SECURITY.md](SECURITY.md)).

**This contract applies from v1.9.0 forward.** v1.9.0 itself makes breaking changes to config field names and endpoint paths without a deprecation window — the one release that does. fold had no users to protect at that point, and taking the break there rather than after launch was the honest trade; the alternative was a major-version bump whose module-path change would have silently pinned every published `go run …@latest` to v1.8.0 forever. See the v1.9.0 changelog entry for the migration.

## Not implemented

Known gaps, documented deliberately:

- **SEP-2575 `subscriptions/listen` streams** — the Go SDK supports the 2026-07-28 protocol on its streamable HTTP server only in stateless mode, which fold cannot use: session-keyed bridging (sampling, elicitation, per-client streams) requires stateful sessions. Clients on the legacy handshake — which is what the SDK negotiates against stateful servers today — get full notification fan-in (list-changed and resource-updated, tested in `gateway/listen_test.go`). fold's fan-in already sits on the surfaces the SDK reuses for listen streams, and a drift canary in that test fails when the SDK lifts the restriction. Federated *tasks* (get/list/cancel/result/update with mint-affinity and probe fallback) **are** implemented — see above.
- **Content inspection (DLP / PII filtering / prompt-injection detection)** — deliberately out of scope. Inspecting request and response bodies means buffering and rewriting traffic, which conflicts with fold's invisibility rule (behavior through the gateway matches hitting the upstream directly) and its latency gate — and inline detection is a product of its own, not a gateway feature. fold's security model is structural instead: deny-by-default tool allowlists, per-principal invisibility, claim-gated (ABAC) rules, credential brokering so agents never hold upstream keys, and a full audit trail to feed the SIEM that does the detecting. If inline inspection becomes table stakes, the fold-shaped answer is an opt-in content-inspection hook (an external policy endpoint on the ingress/egress path), not built-in scanning.

Both gaps appear in [docs/roadmap.md](docs/roadmap.md) — the first with the SDK dependency it waits on, the second as a standing non-goal — alongside the rest of what fold does and does not intend to build.

## Changelog

### v1.9.0 — 2026-08-11

The console's source leaves the repo; its API gets a name of its own.

**Breaking.** This release renames config fields and endpoint paths with no aliases — the only release that does, and the reason is on record under [API stability](#api-stability). Migration:

| Was (≤ v1.8) | Now (v1.9) |
| --- | --- |
| `GET /console/api/state` | `GET /api/federation` |
| `GET /console/api/auth` | `GET /api/auth-hint` |
| `GET /healthz` | `GET /health` (the alias is removed; a probe left on the old path now 404s) |
| `server.console.groups` | `server.introspection.groups` |
| `server.console.enabled` alone | `server.console.enabled` **plus** `server.introspection.enabled` |
| `fold_http_rejections_total{reason="console_viewer"}` | `…{reason="introspection_viewer"}` — a dashboard or alert selecting the old value stops matching **silently**, with no error anywhere |
| audit denial `error: "principal not in console viewer allowlist"` | `"principal not in introspection viewer allowlist"` — update any SIEM rule matching that string |

- **The console's assets move to [fold-run/fold-console](https://github.com/fold-run/fold-console).** A hand-written browser app was living in a Go repo whose review discipline is built for a proxy path, released on that proxy's cadence. The source now has its own repo, its own contributors, and its own cadence; `gateway/console/` is vendored output pinned to a commit, verified by CI, and still embedded — so what a fold binary serves is exactly what it served before. The assets stay checked in rather than fetched at build time because the Go module proxy is fold's distribution channel: `go run github.com/fold-run/fold/cmd/fold@latest` must build from the proxy zip alone, which runs no generators and carries no submodules.
- **`server.introspection`** — the read APIs are no longer the console's private detail. `/api/federation` reports configuration truth that `/metrics` structurally cannot: the effective policy default and rule count, audit sink types, whether shared state and tracing are on, discovery's last sync, and the viewer's tenant governance. It is separately configurable because an operator scripting against the federation snapshot should not have to serve a browser page to get it. `introspection.groups` is the viewer allowlist, unchanged in meaning and moved to where it belongs — it gates the API, and the API now has more than one consumer. The response shape joins the frozen wire surface.
- **`server.mcpPath` is validated against the gateway's own paths.** Serving MCP at `/api/federation` or `/health` used to register a duplicate mux pattern and panic the process at startup with no useful message; it now fails `fold --validate` and the chart's config-validating init container instead.
- **The embedded fonts ship with their licence.** The four woff2 subtypes have been in every binary since v1.2 with no OFL text anywhere in the tree; OFL 1.1 requires the licence accompany the Font Software, so the omission travelled with each release. `console/fonts/OFL.txt` now ships with them, and the embed-manifest test keeps it there.
- **`/healthz` is removed from the gateway.** It was the health path through v1.4 and a deprecated alias since v1.5.0. Leaving it while declining to cut a major would have meant an alias with no removal date at all. `fold-discovery` **keeps** its alias for now: there, the path does not 404 when removed but falls through to the document handler, so a stale probe would quietly start scraping the upstreams document and reporting 200 — retiring it safely needs an explicit 404 rather than a deletion.

### v1.8.0 — 2026-08-10

Tenancy: the customer becomes an object instead of an assembly.

- **`tenants[]`** — a named set of principals plus the governance that applies to them as a group, matched from the same verified claims policy already matches on. Before this, "team A sees these tools, gets this allowance, and appears separately in audit" was assembled from four unrelated mechanisms and the word *tenant* appeared nowhere in the config, the audit stream, or the metrics. The line the design holds above all: **a tenant groups principals; it does not authenticate them** — it is derived from the verified `auth.Principal`, never presented alongside a token, and policy remains the authority on what may be invoked. Reloadable, unlike `server.budget`: tenants change when a customer signs up. Additive by construction — declare none and nothing changes.
- **One allowance and one bucket per team.** `tenants[].budget` is the dimension consumption governance actually wanted: `perPrincipalPerMinute` gives each *person* a bucket, so ten agents on one team hold ten allowances, while a tenant shares one — which is what "team A cannot flood team B" means. Budgets charge narrowest-first (upstream → tenant → server) so a refusal never spends a wider allowance, and buckets check widest-first (global → tenant → per-principal) so a flood is refused before it costs any routing work.
- **`tenants[].upstreams` bounds what a tenant can see**, evaluated *before* policy. It filters the **fan-out**, not the result: an upstream outside the subset is never asked, so it costs no request, no budget, and no partial-failure entry when it is down. Named invocations against it are refused ahead of the policy engine with `-32042` — except `tasks/*`, which answer "no upstream owns that id", the posture that path already takes for another principal's task, because there a refusal must not reveal existence.
- **A principal resolves to at most one tenant, and ambiguity is refused rather than guessed.** Assigning by precedence would hand a caller another customer's allowance and visibility the day someone reorders a list. Selector overlap cannot be decided statically — it is only decidable against a real principal — so validation catches what is genuinely checkable and the rest is caught at request time.
- **Resolution is flat in the number of tenants.** The two selector shapes a per-customer document repeats — one claim equalling one value, or one group — are indexed at snapshot time, keyed from opposite sides: the claim index by what the tenant requires, the group index by what the principal holds. Ten thousand tenants resolve in **97 ns, zero allocations**, the same as ten; a scan cost 450 µs at that size ([benchmarks](docs/benchmarks.md#tenant-resolution-cardinality)). Compound selectors still scan, so keep those in the tens.
- **In the record** — `tenant` on every audit event its principals produce, denials included, plus `fold_tenant_requests_total{tenant,outcome}` and `fold_tenant_upstream_calls_total{tenant}`, the second counting the unit a budget is charged in. These are new metric *names* rather than a `tenant` label on the existing ones: label sets are frozen by the compatibility contract, and a new label would break every dashboard built on them. The console's federation view is now the *viewer's* — a tenant with a subset sees its own upstreams and its own limits, closing the one place a dashboard could show a customer the topology its own traffic is refused.
- **Helm: `appVersion` is the image users actually get.** `values.yaml` ships `image.tag: ""` and the deployment falls back to `Chart.AppVersion`, which had not moved since v0.4.0 — so a default `helm install` deployed a pre-1.0 gateway. Fixed, and the release flow now bumps it, verified by re-rendering the chart rather than by inspecting the edit.
- **The console adopts the current fold.run design system.** Its tokens still carried the previous identity: a mint accent on every button, three radii, and the mark-plus-text lockup the drawn wordmark replaced. Live (`#D6FF00`) is now licensed to proof alone — status-up and the focus ring — while actions run the neutral ramp on a Signal-white fill, and every corner is machined at 2px. Two deliberate departures from the marketing spec, both recorded at the token: controls are 34px rather than 44px, because a console stacks a token field, a picker, and three mode buttons above a table; and a half-open breaker reads on the neutral ramp rather than in amber, because the palette has no third hue and a half-open breaker is not proof of anything. Landed in `a474025`, whose message describes only the Helm fix above.

### v1.7.0 — 2026-08-08

Consumption governance: what was spent, by whom, and a ceiling on it.

- **Budgets** — `server.budget` and `upstreams[].budget` cap consumption over a calendar period (`hour`/`day`/`month`, UTC-aligned) that **accumulates** until it rolls over. This is the question `rateLimit` cannot answer: a sliding window smooths a burst and forgets it, a budget remembers. Absent by default — a default allowance is a default outage waiting for a busy month. `server.budget` is construction-wired like the rest of that section, so an allowance cannot be widened under a running gateway by editing config.
- **The unit is upstream invocations, not client requests.** One `tools/list` fans out to every upstream, so counting client requests would price a list the same as a ping. The new `upstreamCalls` audit field and `fold_request_upstream_calls` histogram make that fan-out visible in its own right.
- **Exhaustion mints `-32044`** and the `budget_exhausted` audit outcome, distinct from `-32040` because the remedies differ: a rate limit clears in seconds, a budget not until the period rolls. The message names the reset instant rather than a retry delay — a client backing off by a monthly reset would sleep for a fortnight.
- **Budgets are checked where the invocation really happens**, after the session is in hand, so a rate limit, an open circuit, or a failed connect never spends the allowance. Without that, an upstream down for a month would burn tens of thousands of units on calls nobody served.
- **Metering** — additive audit fields recording what fold observed and nothing it did not: `upstreamCalls`, `itemsServed` (what a list handed *this* caller, after policy filtering), and `usage` carried verbatim from an upstream's result `_meta`. There is no tokenizer in the gateway; fold governs MCP consumption, not model spend, and an installation needing both runs both. The reasoning is on record in [docs/design-consumption.md](docs/design-consumption.md).
- **Fail-open, but loud** — a budget check that cannot reach shared state degrades to per-instance enforcement rather than to none, and says so via `fold_budget_degraded_total`. Alert on any non-zero rate: it means the fleet is not enforcing one allowance. The gateway also warns at startup when a budget is configured without shared state.

### v1.6.0 — 2026-08-08

Local MCP servers join the federation, and the list path stops rebuilding what it already holds.

- **`fold-stdio`, the shim for local servers** — fold federates HTTP endpoints, but most MCP servers are stdio processes. The shim runs one of them and serves it over streamable HTTP, so it joins the federation as an ordinary `url` upstream: credential strategies, health checks, load balancing, breakers, timeouts, policy, pagination, and audit all apply with no special case, and nothing in the config document changed. Ships as its own binary, image (`ghcr.io/fold-run/fold-stdio`), and compose profile — see [docs/stdio.md](docs/stdio.md), and [docs/design-stdio.md](docs/design-stdio.md) for why process supervision lives in a sidecar rather than in the gateway.
- **The command never comes from the network** — it is fixed at the shim's argv, never taken from a request, a config document, or discovery. A `command` field in the config would hand whoever controls the discovery document an `exec` on the gateway host, reducing `allowedSecretRefs` and `allowedCredentialHosts` to formalities. The child's environment is an explicit allowlist rather than an inheritance, one process serves one session (a stdio connection carries exactly one MCP session, so sharing one would share a JSON-RPC id space), children run in their own process group so a wrapper's grandchildren cannot survive teardown, and a non-loopback bind without `--bearer-env` is refused at startup.
- **Warm list hits stop re-decoding** — `cachedList` ran `json.Unmarshal` over the full serialized list on every warm hit, per upstream, and the egress namespace rewrite copied every tool and rebuilt every name per request. Both are now memoized against the identity of the cached bytes, which makes the memo self-invalidating and keeps it off for upstreams whose caching is disabled because their credential is caller-derived. A warm hit is **~55 ns and one allocation regardless of list size**.
- **Policy filtering allocates nothing** — `globMatch` re-split its pattern on `*` for every evaluation, one allocation per item on every `tools/list`. Patterns come from config and never change for the life of an engine, so they compile when the engine is built. Filtering 1,000 tools per principal is now CPU only.
- **Federation cost is measured** — the added-latency gate uses one upstream with one trivial tool, so nothing in CI observed the work that scales with federation size. `BenchmarkFederatedListTools` covers the merge path directly: a 20 × 50 federation merges 1,000 tools in **25 µs**. Methodology and the full table in [docs/benchmarks.md](docs/benchmarks.md).
- **A roadmap** — [docs/roadmap.md](docs/roadmap.md) records what fold intends to build and, more usefully, what it declines to build and why.
- Dockerfiles move to [`deploy/docker/`](deploy/docker); `azure/setup-helm` bumped off deprecated Node 20.

### v1.5.0 — 2026-08-07

Hardening, and task ownership the whole fleet agrees on.

- **Fleet-wide task ownership** — the `taskId → (upstream, minting principal)` index moves out of process memory into a shared-state primitive, so with `REDIS_URL` (or `server.redisUrl`) set every instance reads the same ownership: a caller can no longer reach another principal's task by landing on an instance that did not serve the mint, and the binding survives a rolling restart. Redis is authoritative on reads, every write is mirrored locally, and an outage falls back to that mirror rather than to no records. Entries key on a digest of the task id and hold a digest of the owning principal; `tasks/list` resolves a whole page in one batch read. Records expire after 24 hours and a gateway without Redis is still per-instance — both fall through to the existing locate-by-probe path.
- **`/health` replaces `/healthz`** — the health path is now `/health`. `/healthz` answers identically as a deprecated alias (`Deprecation: true`, plus one log line on its first use so you can find what still probes it) and is removed no sooner than the next major; point probes at `/health`. Applies to `fold-discovery` too, where the old path would otherwise have fallen through to its document handler.
- **`/health` is no longer a load amplifier** — every call used to fan a live MCP ping to every upstream, unauthenticated and outside the rate limiter. The fan-out is now single-flighted and reused for a second, invalidated immediately by a reload or discovery sync, so polling it in a loop costs at most one collection per second.
- **Subscriptions are released when their client goes away** — `resources/unsubscribe` was the only path that decremented the ref count, so a client that subscribed and disconnected left the gateway holding an upstream subscription indefinitely. The idle sweeper now reaps them and drops the shared upstream subscription with the last live subscriber.
- **Credential paths hardened** — the token endpoint's response body is bounded and its fetches are single-flighted per identity (a burst of first-time callers for one principal became a burst of grant requests, each carrying the client secret and that caller's own bearer token); the discovery poller and the audit webhook now refuse redirects, as the token-endpoint client has since v1.0.1.
- **Stricter host matching, bounded stores** — a non-numeric port in `Host` or `Origin` is rejected rather than split at the last colon, closing a rebinding bypass where `localhost:8080.evil.com` read as the allowed host `localhost`; and the per-instance affinity caches (resource-URI ownership, the per-principal limiter memo) are size-bounded.

### v1.4.1 — 2026-08-06

The console wears the brand.

- **fold.run design system** — the console adopts the site's stardust tokens (dark-only, IBM Plex Sans + Geist Mono) with the fonts embedded in the binary as self-hosted OFL latin subsets (~127 KB), so the CSP's no-external-fetches rule holds for typography too. Status facts render as a uniform four-across card grid, the header carries the brandmark, and a footer links to fold.run, docs, GitHub, and status. No wire, config, or API change.
- **Honest client version** — the test console's `initialize` now reports the gateway's stamped version in `clientInfo` instead of a hardcoded `"1"`, so upstream and audit trails see which console called.

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
