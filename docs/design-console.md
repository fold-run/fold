# Design: the fold console

Status: **implemented**, with the source extracted to `fold-run/fold-console`
in v1.9. This doc recorded the design before implementation; it now serves as
the decision record. The operational documentation lives in the README,
[operations.md](operations.md), and [security-model.md](security-model.md).

## Two repos, one artifact

Since v1.9 the console's HTML/CSS/JS is maintained in `fold-run/fold-console`
and vendored into `gateway/console/` at a pinned commit recorded in
`gateway/console_source.go`. The gateway still embeds and serves it, so
nothing about what an operator receives changed.

Why extract at all: a hand-written browser app was sitting in a repo whose
review discipline is built for a proxy path, being released on that proxy's
cadence. The two have nothing to do with each other. This is also the shape
the leading OSS gateways converged on — UI source in its own repo, artifact
bundled into the gateway release and pinned by commit — including one that
tried a fully standalone dashboard and moved back after the separate repo
decayed for lack of maintenance.

Why the assets stay **checked in** rather than fetched at build time, which
is how those gateways do it: the Go module proxy is fold's distribution
channel. `go run github.com/fold-run/fold/cmd/fold@latest` builds from the
proxy zip alone — it runs no generators, and **module proxy zips do not
contain submodule content**, so a submodule would yield an empty
`gateway/console/` and a `//go:embed` build failure. Vendoring is not a
compromise here; it is the only mechanism that survives fold's distribution
model, and it happens to be better: no network, no token, no fresh-clone
breakage.

What keeps vendoring honest:

- **The pin is a commit SHA, not a tag** — tags are mutable, and the whole
  safety property is that the checked-in tree is provably a function of an
  immutable upstream point. Same discipline as `CONFORMANCE_COMMIT`.
- **The pin lives outside the embedded tree.** `//go:embed console` would
  sweep up a file placed in `gateway/console/` and serve it at
  `GET /console/SOURCE`, so the constant is a generated Go file instead —
  which also puts it in `make fmt-check` and lets `/api/federation` report
  which console build a binary carries.
- **CI re-downloads the pinned artifact and diffs it.** Without that, a hand
  edit in `gateway/console/` makes the separate repo a fiction.
- **The sync copies an explicit allowlist**, and an embed-manifest test
  asserts the exact file set. The console repo will grow a README, a LICENSE,
  and CI config; none of that belongs in an operator's binary. Adding a file
  to the shipped set is a reviewed change *here*, not a unilateral one
  upstream.
- **The sync PR is never auto-merged.** The console runs same-origin with a
  page holding a live Bearer token in memory; a pin bump is a supply-chain
  change.

## Motivation

A competitive review (2026-08) of another MCP gateway found that fold's
governance surface — federation, per-tool policy, credential brokering,
audit, traffic protection, protocol-faithful bridging — has no counterpart
there, but two things on its feature list are genuinely absent from fold:

1. **A way to look at a running federation.** fold's operator surface is a
   JSON document, `/health`, and Prometheus. There is no page that shows the
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

### No build step *in fold*

Originally: hand-written HTML/CSS/vanilla-JS embedded via `go:embed`, on the
reasoning that fold's story is a single static binary and the only node
dependency in CI is the conformance suite — an npm toolchain would put a
frontend build into every release for what is a dashboard and a JSON-RPC
console.

That reasoning survives, but it was really an argument about *fold's* build,
not about the console's. Since v1.9 the source lives in `fold-run/fold-console`
and this repo vendors literal files, so fold's build has no frontend step by
construction rather than by restraint — and the console is free to have a
toolchain of its own. What crosses the boundary is always static output.

The consequence worth stating plainly: whatever the console is written in, it
must compile to files that can be committed here. No SSR, no server runtime,
no build-time fetch.

The look is the fold.run design system (dark-only stardust tokens; IBM Plex
Sans + Geist Mono). The fonts are the site's self-hosted OFL latin subsets,
embedded in the binary like every other asset (~127 KB), so the CSP's
no-external-fetches rule holds for typography too.

### Endpoints

- `GET /console/` — static assets (embedded, no data in them, served
  unauthenticated), gated by `server.console.enabled`.
- `GET /api/federation` — read-only JSON snapshot (authenticated when
  gateway auth is on; see below), gated by `server.introspection.enabled`.
- `GET /api/auth-hint` — the deliberately unauthenticated sign-in hint,
  same gate.

Wired in `buildHandler()` alongside `/health`, so host validation and the
body cap wrap them for free. Assets ship with
`Content-Security-Policy: default-src 'self'` — no external fetches, ever.

These were `/console/api/state` and `/console/api/auth` through v1.8, and
this doc argued at the time that they were "additive HTTP endpoints —
allowed in a minor; the frozen wire surface is untouched." That was true
while the console shipped from this repo and could never disagree with them.
It stopped being true when the console moved out and became separately
versioned, so v1.9 renamed them and put the response shape on the frozen
surface. The rename went out **without an alias, in a minor** — the one
release that does that — because fold had no users to protect and the
alternative, a major, would have changed the module path and silently pinned
every published `go run …@latest` to v1.8 forever. The reasoning is recorded
under "API stability" in the README so the exception cannot be mistaken for
the rule.

### Config

Two sibling blocks, both defaulting **off**:

- `server.introspection: { "enabled": false, "groups": [] }` — the read APIs.
- `server.console: { "enabled": false, "oauth": {…} }` — the page. Requires
  `introspection.enabled`; validation rejects the combination that cannot
  work rather than serving a page with no data source.

They are separate because the API and the page are separate surfaces: an
operator scripting the federation snapshot should not have to serve a browser
page, and the allowlist gates the API rather than the page. `oauth` stays
under `console` because its redirect URI is literally `{origin}/console/` —
which is also why `console.oauth` requires `console.enabled`.

The `server` section is construction-wired and Reload-rejected, so the right
reload semantics are inherited without touching the snapshot. Schema and
lockstep test update with the fields.

### Auth

The state API reuses the existing gateway verifier. With
`auth.mode: "required"`, `/api/federation` demands the same Bearer token
as `/mcp`; any valid principal may view. (The viewer allowlist —
`server.introspection.groups` — shipped as the follow-up: an audited 403 for
principals outside the listed groups.) With auth disabled,
the endpoint follows `/health`'s existing trusted-deployment logic and its
redaction discipline: raw connect errors and URLs are never echoed to
untrusted callers, and `secretRef` *names* only — never values — appear
anywhere.

The page takes a pasted token and holds it in memory only (no storage).
That was v1's whole story; the PKCE flow shipped as the follow-up
(`server.console.oauth`): Authorization Code + PKCE against a trusted
direct-mode issuer, an unauthenticated `/api/auth-hint` hint carrying
the public client configuration, and the asset CSP extended with exactly
the issuer's origin in `connect-src`. The paste-token path remains as the
fallback (and the only path for EMA deployments, whose ID-JAGs are not
browser-presentable).

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
