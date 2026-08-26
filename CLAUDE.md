# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

This repo is fold, the enterprise MCP gateway — one governed endpoint federating any number of upstream MCP servers into a single virtual server, adding auth, policy, caching, rate limiting, and audit. Single Go module; the Makefile is a thin wrapper over the Go toolchain and is the source of truth for dev/CI commands. The wire protocol (streamable HTTP, request/response and SSE) is the official [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)'s implementation on both the client-facing and upstream-facing sides — fold never hand-rolls protocol framing.

## Commands

```bash
make build
make test                                    # unit + integration (real SDK client/server fixtures)
make race                                    # what CI runs
make lint                                    # golangci-lint (config in .golangci.yml)
make check                                   # fmt-check + tidy-check + vet + build + race + lint

go test ./gateway -run TestName -v           # one test
make bench                                   # latency gate (FOLD_BENCH=1; skipped without it)
make conformance                             # official MCP conformance suite through the gateway (needs node/npx)

# Federation cost — the list path, whose work scales with federation size and
# which the latency gate's 1-upstream fixture does not exercise:
go test ./gateway -run '^$' -bench BenchmarkFederatedListTools -benchmem
FOLD_LOAD_UPSTREAMS=20 FOLD_LOAD_TOOLS=50 make loadtest

# Tenant resolution cost, whose work scales with the number of tenant
# declarations rather than with the federation:
go test ./gateway -run '^$' -bench BenchmarkResolveTenant -benchmem
```

CI (`.github/workflows/ci.yml`) gates every merge on: gofmt, `go mod tidy -diff`, vet, build, `go test -race`, golangci-lint, govulncheck, the added-latency benchmark (added p50 < 5 ms through the proxy path), and the conformance suite (40/40 checks, pinned to a commit in `scripts/conformance.sh` — bump deliberately).

## Architecture

**The gateway is an `mcp.Server` with middleware.** `gateway.New` builds an SDK `mcp.Server` and installs `federationMiddleware` (`gateway/router.go`), which intercepts every method: classify → route (federated fan-out for lists, namespace resolution for named calls) → authorize → proxy → audit. `Gateway.Handler()` returns the `http.Handler`; `cmd/fold` is a thin CLI around it, and embedding in another Go service is `gateway.New(cfg)` + `gw.Handler()` + `gw.Close()` (plus `gw.Reload(cfg)` for hot config reload).

**Reloadable state is one atomic snapshot** (`routes` in `gateway/gateway.go`): upstream set + indexes, passthrough flag, policy engine. Every request loads the snapshot once and routes against it; `Gateway.Reload` validates, swaps, reuses config-identical upstreams (sessions survive), and drains retired ones. The snapshot is built from base config + discovery-sourced upstreams (`gateway/discovery.go` polls `discovery.url`); base reloads and discovery syncs each preserve the other's contribution, and the merged document is validated whole before any swap. The `auth`/`server`/`routing`/`audit`/`tracing` sections are construction-wired — Reload rejects changes to them. New reloadable state belongs in the snapshot, not in fresh Gateway fields. Multi-endpoint upstreams (`urls`) balance new sessions round-robin with connect failover via `endpointPool` (`gateway/endpoints.go`); sessions stay pinned to the endpoint they connected to, and `healthCheck.intervalMs` adds an active probe loop that ejects/restores endpoints without client traffic.

**Request pipeline order matters** (see README "Request pipeline"): host validation → authenticate (JWKS verifier; the verified `auth.Principal` rides through the SDK's `TokenInfo.Extra` into request metadata) → global rate limit → route → deny-by-default policy check → per-upstream rate limit / circuit breaker / timeout → proxy with credentials attached → egress (per-principal list filtering, namespace rewriting) → one audit event per terminal response, including denials — audit is the single exit door.

**Two kinds of upstream session** (`gateway/upstream.go`): each upstream holds a shared `rootSession` for lists, reads, and subscriptions, plus per-downstream-client `bridgedSession`s keyed by downstream session ID. Root sessions live in a keyed map: normally one for the whole federation, one per principal when the credential is caller-derived, and one per *capability profile* (`gateway/uicapability.go`) — because MCP Apps is client-declared and an upstream may register a different tool set for a client that supports it, which one session cannot declare two ways. The profile is fold's own normalization of the client's declared extensions, never the raw map, and it qualifies the list-cache key so two profiles cannot serve each other's lists. Bridged sessions carry server-initiated traffic (sampling, elicitation, logging, progress) back to the originating client: `Gateway.callCtx` tracks each in-flight named invocation's context so the SDK routes upstream-initiated requests over that call's own stream. Idle bridged sessions are swept after 5 minutes.

**Shared state goes behind `state.Provider`** (`internal/state`): rate-limit windows, circuit breakers, list caches, single-use records (EMA `jti`), and the task-ownership store are interfaces with two providers — in-memory (default) and Redis (`REDIS_URL` / `server.redisUrl`), which makes a gateway fleet behave as one. Redis outages fail open with a 500 ms bound per operation; `Once` and `Store` additionally fall back to a local mirror, so an outage degrades to per-instance enforcement rather than none. New cross-instance state belongs behind this interface, not in ad-hoc maps. Per-instance affinity caches (`resourceOwner`, `principalLimits`) use `internal/bounded` — every one of them must be size-bounded, because all are keyed by identifiers the gateway does not choose. `ListCache` stores serialized JSON so entries can live in Redis, so each upstream memoizes the *decoded* form of its lists (`cachedList`, keyed by list kind — fold-chosen, at most one entry per list method, hence not `bounded`); the memo is validated by the identity of the cached bytes, which makes it self-invalidating and keeps it off for upstreams whose caching is disabled because their credential is caller-derived. **Items returned from `cachedList` are shared across requests and must be treated as read-only** — the egress paths that rewrite a name copy the item first.

**Naming and identity**: tools/prompts are exposed as `{namespace}__{name}`; a single upstream without a namespace runs passthrough (no rewriting). Resource URIs are opaque and never rewritten — `Gateway.resourceOwner` remembers which upstream listed each URI. The one exception is the MCP Apps `ui://` scheme (`gateway/uiresource.go`), which the extension scopes per server rather than globally: those are minted as `ui://fold/{namespace}/{rest}` on egress and resolved back on read, subscribe, and update, so two upstreams cannot collide on one interface. Policy and audit still see the upstream's own URI. Policy filters list results per principal *and* denies named invocations; invisibility plus call-denial is the enforcement pair.

**Errors**: the gateway mints only `-31040` (upstream rate limit), `-31041` (upstream unavailable/circuit open), `-31042` (policy denied), `-31043` (unknown namespace), `-31044` (consumption budget exhausted), and `-31045` (task id owned by no upstream). Upstream errors pass through verbatim. README "Errors" is the canonical table — this list moves with it.

**Config**: one JSON document (`config` package), validated in `gateway.New` and by `fold --validate`. `FOLD_CONFIG` accepts a file path or inline JSON. The version string is stamped via `-ldflags "-X github.com/fold-run/fold/gateway.ldflagsVersion=..."`, and falls back to the module version Go records in the binary when nothing stamped it (a `go install` build).

## Rules the repo follows

1. **Test with real peers**: integration tests (in `gateway/*_test.go`) spin up real MCP servers from the official Go SDK behind the gateway — hand-rolled fixtures only for instrumentation. Redis paths are tested against miniredis.
2. **The gateway stays invisible**: behavior through the gateway must match hitting the upstream directly; the conformance suite enforces this. Don't buffer or rewrite responses unless federation requires it (namespacing, list merging, policy filtering).
3. **Performance is a test**: the bench job gates merges on added latency; keep the proxy path allocation-light.
4. **Known gaps are documented**: deliberately unimplemented features (`subscriptions/listen` fan-in, content inspection) live in README "Not implemented" — update it if you close or widen a gap.

## Claude Code tooling

`.claude/` (see `.claude/README.md`) carries project skills, subagents, and hooks that encode this file's invariants as workflows. Reach for them instead of improvising: `/preflight` (local merge gate), `/ship` (branch, title, PR body), `/fold-release`, `/reloadable-state` (new config/state checklist), `/observability` (metrics, spans, audit fields), `/helm-chart`, `/stdio-bridge`, `/discovery-controller`, `/deps`, `/roadmap`, `/conformance`, `/update-docs`, `/perf`, `/watch-ci`. Subagents: gateway-reviewer (invariant review), integration-test-author, docs-sync, bench-profiler, security-auditor, mcp-spec-auditor, deploy-reviewer, release-verifier, flake-triage. Hooks auto-gofmt edited Go files, flag the lockstep surfaces (config↔schema, metrics↔observability pack, chart versions, the two pins, the error-code table), block writes to the vendored console, guard force-push/`--no-verify`, prompt on release tags and on commands that discard uncommitted work, and block ending a turn with unformatted files or undocumented behavior changes.
