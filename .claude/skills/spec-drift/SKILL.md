---
name: spec-drift
description: Compare a new MCP protocol revision against what fold implements and produce the gap list — what fold must change, what it may decline, and what becomes a deliberate documented gap. Use when a new revision ships, when the conformance drift workflow flags upstream movement, or when bumping the Go SDK across a protocol version.
---

# Protocol revision drift

`/conformance` catches drift in the *suite*. This catches drift in the
*specification* — the changes that land months before a check exists for
them, and the deprecations that never get a check at all.

Run it when a revision ships, when the SDK bumps a protocol version, and
before any deliberate `CONFORMANCE_COMMIT` bump.

## Procedure

**1. Read the changelog, not the diff.**
`https://modelcontextprotocol.io/specification/<new>/changelog.md` lists
major changes, minor changes, and deprecations. Also fetch
`…/deprecated.md` — the removal registry, with the twelve-month window
that tells you how long fold has.

**2. Classify each entry against fold's two faces.** fold is a server to
its clients (`gateway/router.go`, the `mcp.Server` and its middleware) and
a client to its upstreams (`gateway/upstream.go`). A change can hit one,
the other, or both, and the answers differ:

- *Server-side* changes fold must implement or clients break.
- *Client-side* changes are usually the SDK's job — check whether
  `go-sdk` already covers it before writing anything.
- *Intermediary* changes are fold's alone and no SDK will do them: list
  merging, cache scoping, error-code minting, namespacing. **These are the
  ones that get missed.**

**3. Grep before concluding.** A feature fold "doesn't support" often
exists under another name. For each entry, search for the field, method,
and header names:

```bash
grep -rni 'resultType\|input_required\|cacheScope\|ttlMs\|Mcp-Method' --include='*.go' .
grep -rn '\-320[0-9][0-9]' --include='*.go' . | grep -v _test
```

**4. Check the SDK's position.** fold never hand-rolls framing, so for
most wire changes the question is "has the SDK shipped this yet" and the
answer bounds what fold can do. Record the SDK version and the specific
restriction — the `subscriptions/listen` gap in README "Not implemented"
is exactly this shape, and it carries a drift canary in
`gateway/listen_test.go` that fails when the SDK lifts its restriction.
**A gap that waits on the SDK gets a canary that fails when the wait
ends** — otherwise nobody learns the wait is over.

**5. Land each gap in exactly one of three places.** A finding that lands
nowhere is how drift becomes permanent:

| Verdict | Where it goes |
| --- | --- |
| Must implement | An issue and a test; `docs/roadmap.md` if it is not immediate |
| Deliberately declining | README "Not implemented" **and** `docs/roadmap.md`, with the reason |
| Blocked on the SDK | README "Not implemented" + the dependency + a drift canary test |
| Deprecated upstream | `docs/roadmap.md` with the removal window; a plan before the window closes |

## What to check every time

These are the categories that have bitten aggregating gateways, ordered by
how quietly they fail:

- **Error-code allocation.** The `2026-07-28` revision partitioned the
  server-error range: `-32000`–`-32019` implementation-defined,
  `-32020`–`-32099` **reserved for the specification**. Any code fold
  mints outside the implementation-defined band is a future collision.
- **Caching semantics.** fold is a shared intermediary with a Redis-backed
  `ListCache`. New freshness or scope fields (`ttlMs`, `cacheScope`) bind
  fold harder than they bind a plain server — `private` means fold must
  not put it in shared storage.
- **Statefulness.** Revisions have moved toward stateless HTTP. fold's
  session-keyed bridging is the reason it cannot use the SDK's stateless
  mode; re-read that reasoning each revision, because the thing requiring
  bridging may itself have been replaced.
- **Server-initiated traffic.** Sampling, elicitation, logging, and roots
  are the surfaces fold bridges per-client. Changes here (deprecation,
  or replacement by a round-trip pattern) hit `bridgedSession` directly.
- **Required headers and `_meta` keys.** Anything the spec requires on
  every request is something fold must both accept and forward.
- **New required result fields.** fold merges and rewrites list results;
  a new required field is one fold must preserve through the merge.

## Output

A ranked list, each entry with: the changelog item, the spec section, the
fold `file:line` it lands on, which face it hits (server / client /
intermediary), and the verdict from the table above. Then make the doc
edits the verdicts imply — a drift review that ends in prose has not
finished.

The `mcp-spec-auditor` agent does this sweep read-only and in bulk; use it
for the pass, then land the edits yourself.
