# fold-go

**A Go port of [fold](https://github.com/fold-run/fold): the enterprise MCP gateway — one governed endpoint between every MCP client and every MCP server.**

fold-go sits in front of any number of upstream MCP servers — in any language, on any SDK, from any team or vendor — providing federation, enterprise auth, policy, caching, rate limiting, and audit. It is built on the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk), so the wire protocol (streamable HTTP, both request/response and SSE) is the SDK's own implementation on both the client-facing and upstream-facing sides.

**Conformant, provably.** The official [`@modelcontextprotocol/conformance`](https://github.com/modelcontextprotocol/conformance) suite runs against fold-go fronting the reference everything-server on every merge — **40/40 checks**, including sampling, elicitation, logging, progress, and resource subscriptions bridged through the gateway (`conformance` job in [CI](.github/workflows/ci.yml); reproduce with `./scripts/conformance.sh`).

## Use cases

- **Unify a federation.** Acquisitions, child orgs, and teams each ship their own MCP servers; fold-go presents them as one virtual server with namespaced tools — no team rewrites anything.
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

go run github.com/fold-run/fold-go/cmd/fold@latest --config fold.config.json --port 8080
# MCP endpoint: http://localhost:8080/mcp
```

Or run the container (~18 MB, distroless):

```bash
docker run --rm -p 8080:8080 \
  -e FOLD_CONFIG="$(cat fold.config.json)" \
  ghcr.io/fold-run/fold-go:latest
```

`FOLD_CONFIG` accepts either a file path or the JSON document itself (convenient for container env injection). Prebuilt binaries for linux/darwin (amd64/arm64) are on the [releases page](https://github.com/fold-run/fold-go/releases), or `go install github.com/fold-run/fold-go/cmd/fold@latest`.

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

fold-go fans list requests out across all upstreams concurrently, merges and namespaces the results, degrades gracefully when an upstream is down (`_meta["run.fold/partialFailure"]` lists the failed upstream ids), and short-circuits unhealthy upstreams with a per-upstream circuit breaker. Proxied results are tagged with their origin in `_meta["run.fold/upstream"]`. See [`fold.config.example.json`](fold.config.example.json) for a full example.

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

## Configuration

One JSON document, validated on startup (`fold --validate`). Loaded from `--config <path>` or `FOLD_CONFIG`.

### `upstreams` (required)

| Field | Default | Notes |
|---|---|---|
| `id` | — | Lowercase alphanumeric + hyphens. Used in policy, audit, health. |
| `url` | — | The upstream's MCP endpoint. |
| `namespace` | none | Tool/prompt name prefix (`{namespace}__{name}`). Omitted → passthrough; only valid with a single upstream. |
| `protocol` | `session` | `"session"` negotiates the sessionful handshake, required to bridge server-initiated traffic (sampling, elicitation, logging, progress, resource updates). `"auto"`/`"2026-07-28"` lets the SDK prefer the stateless 2026 protocol, which cannot carry server-initiated requests. |
| `owner` | none | `{ org, team, contact }` — surfaces in audit and health. |
| `labels` | none | Free-form string map for reporting. |
| `auth` | `{"strategy":"none"}` | Upstream credential strategy — see below. |
| `timeouts` | `5s/60s` | `connectMs`, `requestMs`. |
| `circuitBreaker` | `5 / 30s` | `failureThreshold` consecutive failures open the circuit; a probe is admitted after `halfOpenAfterMs`. |
| `rateLimit` | none | `{ requestsPerMinute }` for this upstream only. |
| `cacheTtlMs` | `30000` | TTL for cached list results. Negative disables caching. |

List caches are also invalidated immediately when an upstream emits a `list_changed` notification — TTLs remain the backstop.

### Upstream auth strategies

| Strategy | Fields | When |
|---|---|---|
| `none` | — | Trusted network, no upstream auth. |
| `static` | `secretRef`, `header?`, `scheme?` | API-key upstreams. `secretRef` names an environment variable. |
| `passthrough` | — | Forwards the client's Bearer token as-is. Upstreams doing strict RFC 8707 audience checks will reject it — prefer token-exchange. |
| `client-credentials` | `tokenEndpoint`, `clientId`, `clientAuth`, `scopes?`, `resource?` | Service identity per upstream. Tokens cached until 60s before expiry. |
| `token-exchange` | `tokenEndpoint`, `clientId`, `clientAuth`, `audience`, `scopes?` | RFC 8693 — exchanges the caller's token for an upstream-audience token, preserving user identity end-to-end. **Recommended enterprise default.** Cached per (upstream, subject). |

`clientAuth`: `{ "type": "client_secret_post" | "client_secret_basic", "secretRef": "..." }`.

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

With `mode: "required"`, every `/mcp` request needs a valid Bearer token: trusted issuer (checked before any network I/O), verified signature via cached JWKS, exact audience match, asymmetric algorithms only (RS/ES/EdDSA). Failures answer 401 with a `WWW-Authenticate` challenge pointing at `/.well-known/oauth-protected-resource` (RFC 9728), which the gateway publishes.

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

First matching rule allows; otherwise `defaultDecision`. Policy governs named invocations (`tools/call`, `prompts/get`) and **filters list results per principal** — callers never see tools they cannot call. Protocol plumbing (ping, the lists themselves) is not policy-gated; invisibility plus call-denial is the enforcement pair.

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
| `rateLimit` | none | Global `{ requestsPerMinute }` across all upstreams. |

## Error codes

Gateway-minted JSON-RPC errors (upstream errors pass through verbatim):

| Code | Meaning |
|---|---|
| `-32040` | Per-upstream rate limit exceeded |
| `-32041` | Upstream unavailable (circuit open / unreachable / all upstreams down) |
| `-32042` | Policy denied the invocation |
| `-32043` | Name does not resolve to a configured namespace |

## Observability

- `GET /metrics` — Prometheus exposition. Gateway metrics: `fold_requests_total{method,outcome}`, `fold_request_duration_seconds{method}`, `fold_upstream_requests_total{upstream,outcome}`, `fold_upstream_request_duration_seconds{upstream}`, `fold_upstream_breaker_state{upstream}` (0 closed / 1 half-open / 2 open), `fold_http_rejections_total{reason}`, `fold_build_info{version}`, plus standard Go process/runtime collectors.
- **Distributed tracing** — W3C Trace Context (`traceparent`/`tracestate`) headers on incoming requests are propagated to the upstream calls they cause, so the gateway hop joins the caller's trace.
- **Latency, measurably** — the `bench` CI job gates every merge on added p50 latency < 5 ms through the proxy path (loose for shared runners). Typical local numbers (Apple Silicon, in-process upstream): **~0.20 ms added p50**, gateway p99 ≈ 0.57 ms. Reproduce: `FOLD_BENCH=1 go test ./bench -run TestAddedLatencyGate -v`.

## Operational endpoints

- `GET /healthz` — pings every upstream concurrently; reports per-upstream connectivity, latency, breaker state, and owner. `503` when no upstream is reachable.
- `GET /metrics` — Prometheus metrics (see Observability).
- `GET /.well-known/oauth-protected-resource` — RFC 9728 metadata (when auth is enabled).

## Repository layout

| Path | Purpose |
|---|---|
| `cmd/fold` | The `fold` CLI |
| `gateway` | Gateway engine: pipeline, federation routing, proxying, health |
| `config` | Config schema + validation |
| `auth` | OAuth resource server (JWKS verifier) + upstream credential strategies |
| `policy` | Allowlist policy engine + per-principal list filtering |
| `audit` | Audit events + sinks (stdout, webhook) |
| `internal/ratelimit` | Sliding-window limiter |
| `internal/breaker` | Circuit breaker |
| `internal/cache` | TTL cache with single-flight refresh |

## Development

```bash
go build ./...
go test ./...              # unit + integration (real SDK client/server fixtures)
go test -race ./...
./scripts/conformance.sh   # official conformance suite through the gateway (needs node)
```

The integration suite spins up real MCP servers from the official Go SDK behind the gateway and exercises federation, namespacing, policy filtering and denial, partial failure, credential injection (static and passthrough), rate limits, the breaker, JWT auth against a fixture JWKS issuer, RFC 9728 metadata, and the full server-initiated bridging loop (sampling, elicitation, logging, progress).

## Differences from fold (TypeScript)

fold-go is a faithful port of fold's core feature set on a single runtime. Not (yet) ported:

- **Era translation** (legacy 2025 ↔ stateless 2026 bridging, MRTR parking) — fold-go pins upstream connections to the session era by default (see `protocol`) so server-initiated traffic bridges; fold's held-session legacy bridge and header-based body-free routing are not replicated.
- **Enterprise-Managed Authorization** (ID-JAG token endpoint) — planned; standard OAuth resource-server auth and RFC 8693 token exchange are implemented.
- **Cloudflare Workers runtime and Redis-shared state** — fold-go is a single-binary Node-equivalent deployment (Docker/k8s friendly); cache, rate-limit, and breaker state are in-process.
- **Federated tasks and `subscriptions/listen` fan-in** — long-running task federation is not in this port.
- **Composite federated pagination** — list results are merged and returned as a single page.

## License

Apache-2.0
