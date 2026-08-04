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

Or run the container (~18 MB, distroless):

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

**Federated tasks.** Task ids are opaque and clients persist them across sessions, so — like resource URIs — fold never rewrites them; ownership is remembered instead. A `tools/call` that mints a task (advertised in the result `_meta`) pins `taskId → upstream`; `tasks/get`, `tasks/cancel`, `tasks/result`, and `tasks/update` route to that owner, whose errors pass through verbatim. A task fold never saw (minted on another instance, or affinity evicted) is located by a read-only `tasks/get` probe across upstreams — the owner answers, everyone else is a healthy "no" — after which the mutating method is sent to the owner alone. `tasks/list` merges every upstream; ids no upstream knows answer `-32002`. Ownership is bound to the minting principal: another caller's task-scoped calls answer `-32002` exactly like an unknown id (no existence leak, no probe), and `tasks/list` shows each principal only its own tasks. Tasks fold has no ownership record for (out-of-band, or evicted) stay reachable by any caller via the probe fallback; anonymous callers share one owner bucket, so no-auth deployments are unaffected. The Go SDK does not yet model the task lifecycle, so these are forwarded as opaque JSON via the SDK's custom-method mechanism to fold's task contract; the wire types are gateway-local and swap for the SDK's typed task API when it ships. `subscriptions/listen` fan-in is not included (the SDK exposes no public API for it).

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

### `routing`

| Field | Default | Notes |
|---|---|---|
| `namespaceSeparator` | `__` | Separator between namespace and bare name in public tool/prompt names. Must not contain lowercase letters, digits, or hyphens (the namespace alphabet). |
| `pageSize` | `200` | Per-page bound on federated list results (tools, prompts, resources, templates). Fold merges and policy-filters every upstream's full list, then serves it in pages; cursors are opaque, bound to the calling principal, and expire when the underlying snapshot changes (the client receives `-32602` and restarts the list — `list_changed` notifications already prompt refetches). Negative disables pagination (single merged page). `tasks/list` is always a single merged page. |

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

- `GET /metrics` — Prometheus exposition. Gateway metrics: `fold_requests_total{method,outcome}`, `fold_request_duration_seconds{method}`, `fold_upstream_requests_total{upstream,outcome}`, `fold_upstream_request_duration_seconds{upstream}`, `fold_upstream_breaker_state{upstream}` (0 closed / 1 half-open / 2 open), `fold_http_rejections_total{reason}`, `fold_build_info{version}`, plus standard Go process/runtime collectors.
- **Distributed tracing** — W3C Trace Context (`traceparent`/`tracestate`) headers on incoming requests are propagated to the upstream calls they cause, so the gateway hop joins the caller's trace.
- **Latency, measurably** — the `bench` CI job gates every merge on added p50 latency < 5 ms through the proxy path (loose for shared runners). Typical local numbers (Apple Silicon, in-process upstream): **~0.20 ms added p50**, gateway p99 ≈ 0.57 ms. Reproduce: `FOLD_BENCH=1 go test ./bench -run TestAddedLatencyGate -v`.
- **Structured logging** — operational events (startup, upstream connect/reconnect, session drops, circuit-breaker transitions, refused cross-host redirects, SSE-hang fallbacks, shutdown) log via `log/slog`. `--log-format text|json` and `--log-level debug|info|warn|error` on the CLI; embedders pass `gateway.WithLogger(*slog.Logger)`. Per-request accounting stays in metrics and the audit sink, not the log stream.

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

fold is pre-1.0. The supported surfaces are the `fold` binary (CLI flags, the JSON config document, error codes, operational endpoints) and embedding via `gateway.New` + `Gateway.Handler` + `Gateway.Close` with a `config.Config` — see the [package example](https://pkg.go.dev/github.com/fold-run/fold/gateway). The `auth`, `policy`, and `audit` packages are exported because the gateway wires through them, but their Go APIs may change between minor versions until v1.0. `internal/` packages are not API.

## Not implemented

Known gaps, documented deliberately:

- **SEP-2575 `subscriptions/listen` streams** — the Go SDK supports the 2026-07-28 protocol on its streamable HTTP server only in stateless mode, which fold cannot use: session-keyed bridging (sampling, elicitation, per-client streams) requires stateful sessions. Clients on the legacy handshake — which is what the SDK negotiates against stateful servers today — get full notification fan-in (list-changed and resource-updated, tested in `gateway/listen_test.go`). fold's fan-in already sits on the surfaces the SDK reuses for listen streams, and a drift canary in that test fails when the SDK lifts the restriction. Federated *tasks* (get/list/cancel/result/update with mint-affinity and probe fallback) **are** implemented — see above.
- **`tasks/list` pagination** — federated task lists are merged and returned as a single page (the typed lists — tools, prompts, resources, templates — paginate; see `routing.pageSize`).

## License

Apache-2.0
