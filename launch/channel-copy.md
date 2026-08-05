# channel copy — ready-to-post drafts

Ladders to `messaging.md`. All numbers verifiable: 40/40 conformance (gated per merge), ~0.2 ms added p50 (CI gate < 5 ms), v1.3.0, Apache-2.0, demo.fold.run/mcp, `go run github.com/fold-run/fold/cmd/fold@latest`. "fold" lowercase everywhere, including sentence-initial. No hype adjectives anywhere in this file — receipts only.

Ported from the archived TypeScript repo and rewritten against the Go implementation. The demo is back (Go-backed, 2026-08-05) but era translation is not — the demo demonstrates federation, governed tasks, and the console, never translation. The drafts lead with the demo, the one-command local run, the conformance receipt, and the protocol depth of federated tasks.

---

## 1. Show HN

### Title (lead)

**Show HN: fold – open-source MCP gateway in Go (40/40 conformance)** (66 chars)

Alternates:

- **Show HN: fold – open-source MCP gateway aligned with the 2026-07-28 spec** (72 chars)
- **Show HN: fold – federate MCP servers behind one governed endpoint** (66 chars)

Etiquette notes: no exclamation points, no adjectives, product name lowercase as branded. If the lead title stalls, do not repost the same day.

### Text body (first person, Blake)

> Hi HN — I'm Blake, and fold is an open-source (Apache-2.0) MCP gateway: one endpoint that federates any number of MCP servers, with OAuth, deny-by-default policy, and an audit record on every request. It's a single static Go binary built on the official MCP Go SDK on both sides of the proxy.
>
> Why I built it: every MCP deployment I saw ended up as N clients wired to M servers, each connection with its own auth, its own allowlist (usually none), and no audit trail anywhere. That's the same N×M mess that produced API gateways twenty years ago, so I built the gateway.
>
> The parts that were genuinely hard, and that I'd most like eyes on:
>
> - Federated tasks over opaque ids: the 2026-07-28 tasks extension means a client can poll a task with nothing but an id — no session, no routing hint. fold remembers ownership at mint, falls back to a read-only probe for tasks it never saw, never fans out a mutating method, and binds ownership to the minting principal so another caller's poll answers exactly like an unknown id (no existence leak).
> - Server-initiated traffic through a proxy: sampling, elicitation, progress, and log messages route back over the originating call's own stream, so clients without a standalone stream still hear them.
> - Auth without credential sprawl: the client presents one token; fold does RFC 8693 token exchange per upstream (plus an embedded EMA/ID-JAG token endpoint), so upstream credentials never reach clients.
> - Self-serve federation on Kubernetes: label a Service and it joins the gateway — with default-deny bounds on what credentials a registration may name and where it may send them, because labeling rights must not become secret-exfiltration rights.
>
> Current state, honestly: v1.3.0, solo maintainer. It passes all 40 checks of the official conformance suite (gated in CI on every merge, re-run weekly against the latest SDK release), and adds ~0.2 ms p50 on the proxy path — the CI gate is < 5 ms and the instrument is `make bench` in the repo, so measure it yourself rather than taking my number. The gaps are documented too: `subscriptions/listen` fan-in is blocked on the Go SDK's stateless-only 2026 support, and the README says so.
>
> You can try it without signing up for anything: point any MCP client at https://demo.fold.run/mcp — three public MCP servers (Cloudflare's docs server, GitMCP, and a task-minting demo server) federated behind one endpoint. Call `jobs__start_job`, then poll it with `tasks/get` carrying nothing but the task id — that's the routing problem from the post, live. The read-only console at https://demo.fold.run/console shows the federation as you use it. Rate-limited, unauthenticated, no warranty. Or run your own in one command: `go run github.com/fold-run/fold/cmd/fold@latest --config fold.config.json` (or `docker run ghcr.io/fold-run/fold`).
>
> Repo: https://github.com/fold-run/fold · Docs: https://docs.fold.run
>
> Feedback I'm specifically after: whether the deny-by-default policy model is expressive enough for your org's real rules, edge cases in the task-ownership semantics (especially multi-instance), and what you'd need to see before you'd put this in a production request path.

### Prepared first-comment talking points

Post these as replies when the question appears — not pre-emptively as top-level comments.

**Q: "Why a gateway at all? Build it into the client/server."**
> Because the concerns are N×M. Every client re-implementing auth, policy, audit, and credential handling against every server is how we got API gateways two decades ago. Structurally: a client can't enforce an org's deny-by-default policy on itself, and a server can't audit requests it never sees. The trust boundary belongs in the middle, owned by the platform team. Whether fold specifically is the right middle is a fair separate question — the conformance suite and the code are the evidence I can offer: https://github.com/fold-run/fold

**Q: "What's the latency cost?"**
> ~0.2 ms added p50 on typical hardware; CI fails any merge where the same instrument measures over 5 ms on a shared runner. The methodology is in the repo rather than in my comment so you can pick it apart — `make bench` runs the identical client against the identical upstream directly and through fold, and prints the percentiles. The rate limits and circuit breakers sit off the happy path — they exist to protect upstreams, not to police clients. One honest caveat: there's no throughput number yet because there's no load-test harness in the repo yet, and I'd rather publish nothing than something I can't back.

**Q: "Solo maintainer. Why would I depend on this?"**
> It's the right thing to worry about, and I won't argue you out of it with promises. The concrete mitigations: it's Apache-2.0, so shipped code is yours regardless of what happens to me; conformance is enforced by CI on every merge and a weekly job re-runs it against the latest unpinned SDK, opening an issue on any drift, so keeping pace doesn't depend on my attention; the v1 config, CLI, wire surface, and embedding API are a frozen compatibility contract; and the scope is deliberately a gateway, not a platform, to keep the surface maintainable. Evaluate the repo's automation and test coverage (fuzzers, race-detector churn tests, govulncheck) rather than my assurances — that's what they're there for.

---

## 2. MCP community post (Discord #showcase / GitHub Discussions)

> **fold — open-source gateway that federates MCP servers into one governed endpoint**
>
> Sharing here because two pieces are directly useful to people in this community even if you never run a gateway:
>
> **If you're implementing the tasks extension:** fold federates tasks across N upstreams, and the design notes are all in the open — ownership remembered at mint instead of encoded in the id, a read-only probe for tasks the gateway never saw, mutations never fanned out, and ownership bound to the minting principal so a denial is indistinguishable from a miss. You can poke at it live: mint a job on https://demo.fold.run/mcp and poll it from a fresh session. If you're building anything that sits between a task-minting server and its clients, the write-up and the code may save you a design cycle: https://fold.run/blog/federating-mcp-tasks/
>
> **If you care about spec correctness:** fold passes 40/40 checks of the official conformance suite, fronting the reference server, gated in CI on every merge and re-run weekly against the latest SDK release. It's built on the official Go SDK on both sides of the proxy, and the gaps are documented in the README's "Not implemented" section (with a canary test that fails when the SDK unblocks them) — I would genuinely rather you find more gaps than tell me it looks good. An issue with a transcript is the most useful thing you could send me: https://github.com/fold-run/fold
>
> Apache-2.0, all of it — auth (OAuth resource server, EMA/ID-JAG, RFC 8693 token exchange), deny-by-default policy, audit, rate limiting, the console — no separate enterprise repo. Docs at https://docs.fold.run (there's an /llms.txt). `go run github.com/fold-run/fold/cmd/fold@latest` if you want it local.
>
> — Blake

Etiquette: post once in #showcase, don't cross-post to general channels; answer every technical reply, including the critical ones — especially the critical ones. GitHub Discussions version: same text, title "fold: MCP federation gateway in Go — conformance scrutiny welcome", posted as a Show and Tell.

---

## 3. Product Hunt (for later use — do not launch same day as Show HN)

### Tagline (lead)

**One governed endpoint for every MCP server** (43 chars)

Alternates:

- **The open-source enterprise MCP gateway** (39 chars)
- **Federate and govern every MCP server** (37 chars)
- **Auth, policy, and audit for all your MCP servers** (49 chars)

### Description (~250 chars)

> fold federates your MCP servers into one governed endpoint: OAuth with per-upstream token exchange, deny-by-default policy, audit on every request, rate limits. One static Go binary. Apache-2.0, 40/40 conformance. Try it: demo.fold.run/mcp

(240 chars)

### First-comment maker's note (~150 words)

> Hi PH — Blake here, maintainer of fold.
>
> MCP adoption inside companies has outrun MCP governance: clients wired directly to a dozen servers, each connection with its own credentials and no audit trail. fold puts one gateway in the middle. Clients configure a single URL; behind it you get OAuth with per-upstream token exchange (upstream keys never reach clients), deny-by-default policy per principal, an audit record on every request, rate limits, and circuit breakers.
>
> It deploys like infrastructure you already run: a single static Go binary or a ~22 MB distroless container, a Helm chart, hot config reload, Prometheus metrics. It passes all 40 checks of the official MCP conformance suite, gated in CI on every merge.
>
> No signup needed to evaluate: point any MCP client at demo.fold.run/mcp — three public servers, one endpoint, and the console at demo.fold.run/console shows the federation live. Or `go run github.com/fold-run/fold/cmd/fold@latest` puts it in front of your own first server in a minute. Everything is Apache-2.0 at github.com/fold-run/fold. I'll be here all day for questions.

---

## Sequencing note

1. Anchor blog post live first (it's the link target for everything): https://fold.run/blog/federating-mcp-tasks/ — already published.
2. Show HN the same morning (post before 9am ET, weekday; body links the post and the repo).
3. MCP Discord #showcase + GitHub Discussions the **day after** HN: one community venue per day; the Discord post should come from a name people saw contributing the day before, and by then early HN feedback has confirmed the quick-start holds up on machines that aren't mine.
4. Product Hunt held back 1–2 weeks — a second wave, not a splitter of the first. Never solicit upvotes anywhere; ask people to try the demo or run the one-liner instead.
