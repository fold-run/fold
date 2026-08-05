# Design: the fold console

Status: **implemented** (shipping in the next minor). This doc recorded the
design before implementation; it now serves as the decision record. The
operational documentation lives in the README, [operations.md](operations.md),
and [security-model.md](security-model.md).

## Motivation

A competitive review (2026-08) of another MCP gateway found that fold's
governance surface — federation, per-tool policy, credential brokering,
audit, traffic protection, protocol-faithful bridging — has no counterpart
there, but two things on its feature list are genuinely absent from fold:

1. **A way to look at a running federation.** fold's operator surface is a
   JSON document, `/healthz`, and Prometheus. There is no page that shows the
   federation: upstreams, owners, breaker states, endpoint rotation,
   discovery sync status.
2. **A way to poke a tool through the gateway** without standing up an MCP
   client — their portal's JSON-RPC test console is the one feature of
   theirs worth wanting.

Their portal's other half — create/edit/delete of servers, lifecycle
management — is deliberately **not** wanted. fold governs endpoints that
already exist; registration is discovery's job, which is pull-based,
validated whole, and auditable. A write control plane would be a second,
competing registration path. This boundary is on record here so a future
"why doesn't the console have an Add Upstream button" has an answer.

## What it is

One feature, two faces, both read-only: the **fold console** — an embedded
web page served by the gateway with (a) an observability dashboard and
(b) an interactive test console. Off by default.

## Design decisions

### No build step, no framework

Hand-written HTML/CSS/vanilla-JS embedded via `go:embed`. fold's story is a
single static binary and the only node dependency in CI is the conformance
suite; an npm/React toolchain would put a frontend build into every release
for what is a dashboard and a JSON-RPC console. If the UI outgrows vanilla
JS, that is a v2 problem to have.

### Endpoints

- `GET /console/` — static assets (embedded, no data in them, served
  unauthenticated).
- `GET /console/api/state` — read-only JSON snapshot (authenticated when
  gateway auth is on; see below).

Both are additive HTTP endpoints — allowed in a minor; the frozen wire
surface is untouched. Wired in `buildHandler()` alongside `/healthz`, so
host validation and the body cap wrap them for free. Assets ship with
`Content-Security-Policy: default-src 'self'` — no external fetches, ever.

The path is fixed at `/console` (not configurable) until someone needs
otherwise.

### Config

`server.console: { "enabled": false }`. The `server` section is
construction-wired and Reload-rejected, so the right reload semantics are
inherited without touching the snapshot. Default **off**: no new surface
unless asked for, consistent with the security-posture defaults in
[defaults.md](defaults.md) — if this ships, `console.enabled=false` gets a
row there. Schema and lockstep test update with the field.

### Auth

The state API reuses the existing gateway verifier. With
`auth.mode: "required"`, `/console/api/state` demands the same Bearer token
as `/mcp`; any valid principal may view. (The viewer allowlist —
`server.console.groups` — shipped as the follow-up: an audited 403 for
principals outside the listed groups.) With auth disabled,
the endpoint follows `/healthz`'s existing trusted-deployment logic and its
redaction discipline: raw connect errors and URLs are never echoed to
untrusted callers, and `secretRef` *names* only — never values — appear
anywhere.

The page takes a pasted token and holds it in memory only (no storage).
Clunky but honest for v1; an OAuth PKCE flow in the console is a natural
follow-up needing no server changes beyond what RFC 9728 already
advertises.

### The test console is an MCP client, full stop

The console's test surface is a minimal streamable-HTTP MCP client in the
browser, pointed at `/mcp` itself. This is the load-bearing decision — it
keeps every invariant intact by construction:

- Console traffic runs the full pipeline: policy filters the tool list the
  user sees, denials answer `-32042`, rate limits apply, and **audit logs
  every call** — the console cannot bypass governance because it has no
  privileged path.
- Nothing new touches the proxy path, so the invisibility rule and the
  latency gate are unaffected by construction, not by measurement.

The client (~200–300 lines of JS) does `initialize` → `tools/list` →
`tools/call` and must parse both JSON and SSE-framed POST responses — this
is the main technical risk; test against the SDK server's actual framing,
not the spec's. UI: tool picker from the (policy-filtered) list, JSON args
editor, raw JSON-RPC request/response inspector. Tools only for v1; prompts
and resources tabs only if trivial.

### State API content

Version, snapshot summary (upstream count, passthrough flag), per-upstream
health/breaker/endpoint rotation (refactor `handleHealth`'s per-upstream
collection into a shared helper rather than duplicating it), discovery sync
status (last outcome and time — currently visible only as a metric), and
effective rate-limit / policy-rule counts.

## Implementation phases

1. **Config plumbing** — `server.console.enabled`, validation, schema +
   lockstep test, example config; run the reloadable-state checklist to
   record the construction-wired classification.
2. **State API** — `gateway/console.go`, sharing `handleHealth`'s
   collection helper.
3. **Dashboard UI** — embedded assets, federation table, discovery status,
   polling refresh.
4. **Test console** — the browser MCP client and inspector.
5. **Tests** — real SDK peers per repo rule 1: 404 when disabled; 401
   without a token when auth is required; redaction with auth off vs on;
   and the key one — a console-shaped tool call lands in the audit sink
   with the right principal and is policy-denied when the rule says so.
   gateway-reviewer and security-auditor review (a new unauthenticated
   static surface plus a new authenticated JSON surface is the
   security-auditor's beat).
6. **Docs and release** — README section, operations.md, security-model.md
   (the console's trust story: no privileged path, token stays
   client-side), defaults.md row, then a minor release.

## Explicitly out of scope

- Write operations of any kind (register/edit/remove upstreams) — that is
  discovery's job.
- Server lifecycle management (deploying/running upstreams) — permanently
  out of scope, same reasoning as the README "Not implemented" entries.
- Policy editing — policy is config, config is reviewed and reloaded.
