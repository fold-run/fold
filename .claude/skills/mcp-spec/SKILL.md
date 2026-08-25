---
name: mcp-spec
description: Look up the normative MCP specification text for a method, field, header, or error code before implementing or changing it in fold. Use when a change touches wire behavior, when a reviewer asks "does the spec actually say that", or when answering a protocol question — never answer MCP protocol questions from memory.
---

# Reading the MCP specification

fold's whole value proposition is that it is invisible: behavior through
the gateway matches hitting the upstream directly. That is a claim about
a specification, so protocol questions get answered from the
specification — not from memory, not from the SDK's Go doc comments, and
not from how fold currently behaves.

**Fetch the page. Quote the normative sentence. Then write code.**

## The pin

fold targets **`2026-07-28`**, the revision `config.protocol` accepts
(`config/config.go`) and the one the Go SDK negotiates. Every URL below
carries the revision in its path — keep it there. A bare
`/specification/` URL redirects to whatever is current and will silently
answer a different question than the one you asked.

Append `.md` to any page for clean markdown (`…/server/tools.md`).
`https://modelcontextprotocol.io/llms.txt` is the full page index.

## Where things are

| Question | Page |
| --- | --- |
| Method shapes, `_meta`, error codes, **error-code allocation policy** | `/specification/2026-07-28/basic/index` |
| Lifecycle, statelessness, `server/discover` | `/specification/2026-07-28/server/discover` |
| Streamable HTTP, headers, stream semantics | `/specification/2026-07-28/basic/transports/streamable-http` |
| stdio framing | `/specification/2026-07-28/basic/transports/stdio` |
| Tools, `inputSchema`/`outputSchema` | `/specification/2026-07-28/server/tools` |
| Resources, URIs, `resources/read` | `/specification/2026-07-28/server/resources` |
| Prompts | `/specification/2026-07-28/server/prompts` |
| **`ttlMs` / `cacheScope` (`CacheableResult`)** | `/specification/2026-07-28/server/utilities/caching` |
| Pagination (cursors) | `/specification/2026-07-28/server/utilities/pagination` |
| `subscriptions/listen` | `/specification/2026-07-28/basic/patterns/subscriptions` |
| **MRTR — `input_required`, `inputRequests`** | `/specification/2026-07-28/basic/patterns/mrtr` |
| Progress, cancellation | `/specification/2026-07-28/basic/patterns/progress`, `…/cancellation` |
| Elicitation, sampling, roots | `/specification/2026-07-28/client/…` (all three **deprecated**) |
| Authorization, resource-server discovery, DCR | `/specification/2026-07-28/basic/authorization/…` |
| Security considerations | `/specification/2026-07-28/basic/authorization/security-considerations` |
| What changed this revision | `/specification/2026-07-28/changelog` |
| What is scheduled for removal | `/specification/2026-07-28/deprecated` |
| Raw types | `/specification/2026-07-28/schema` |

Extensions live **outside** the versioned tree — MCP Apps at
`/extensions/apps/overview`, tasks at `io.modelcontextprotocol/tasks`.
Tasks moved out of the core protocol in this revision, which is why
`gateway/tasks.go` implements an extension and not a spec method.

## The three fold-specific traps

1. **`MUST` vs `SHOULD` vs `MAY` changes the answer.** A `SHOULD` fold
   declines to follow is a documented decision (README "Not implemented");
   a `MUST` it declines to follow is a conformance bug. Quote the modal
   verb in the finding — "the spec says" is not a finding.

2. **fold is a *shared intermediary*, not just a server.** Several rules
   are addressed to intermediaries specifically and are easy to read past
   when you are thinking of fold as an `mcp.Server`: `cacheScope: private`
   bounds what fold may put in a shared Redis `ListCache`; `ttlMs` is an
   upstream's freshness hint that fold's own `cacheTtlMs` should not
   silently outlive. When a rule names caches, proxies, or intermediaries,
   it is addressed to fold twice over.

3. **fold is a client too.** Anything in `/client/` and in the transport
   pages applies to fold's upstream-facing side (`gateway/upstream.go`),
   where the SDK is the client and fold configures it. Check both
   directions before concluding fold is compliant.

## Reporting what you find

Cite `file:line` on fold's side and the spec section number on the other,
and say which of these it is:

- **Conformance bug** — fold violates a `MUST`. Fix it, and add a test with
  a real SDK peer (`integration-test-author`).
- **Deliberate gap** — fold declines a `SHOULD`/`MAY` for a documented
  reason. It belongs in README "Not implemented" and `docs/roadmap.md`, or
  it does not exist.
- **Drift** — the spec moved and fold has not. That is `/spec-drift`.

Related: `/conformance` runs the executable suite (40/40 checks) — it
catches what it tests, and this skill covers what it does not.
