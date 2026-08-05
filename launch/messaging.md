# fold — messaging architecture

Source of truth for all marketing copy. Every downstream asset (site, docs framing, channel copy, PR) ladders up to this file. "fold" is always lowercase, including at the start of a sentence.

Ported from the archived TypeScript repo's launch messaging and rewritten against the Go implementation; every claim below is verified against v1.3.0. Dead TS-era claims (Cloudflare Workers runtime, protocol-era translation, SEP-2549 caching, `subscriptions/listen` fan-in, throughput figures) do not appear here and must not be reintroduced without shipped code behind them.

The live demo is back as of 2026-08-05 — **demo.fold.run/mcp** runs the unmodified v1.3.0 binary federating three public MCP servers, with the console at demo.fold.run/console. It is a *federation and governance* demo: never attach the old era-translation story to it, and never present it as a benchmark target (it's rate-limited and containerized — `make bench` is the measurement answer).

---

## 1. Positioning statement

**For platform and AI-infrastructure teams running MCP in production, fold is the enterprise MCP gateway: one governed endpoint between every MCP client and every MCP server — federation, auth, policy, audit, and traffic protection in a single open-source binary — unlike per-server point integrations, hand-rolled proxies, or general API gateways that don't speak the protocol.**

Compressed form (internal shorthand): *fold is the one. fold.run is the execution.*

Category we claim: **the enterprise MCP gateway**. Not "an AI gateway," not "an LLM proxy," not "a tool router." We name the category precisely and defend it with protocol depth.

---

## 2. One-liner variants

| Surface | One-liner |
|---|---|
| **GitHub repo description** | The enterprise MCP gateway. One governed endpoint that federates every MCP server — auth, policy, audit, rate limiting. A single static Go binary. Apache-2.0. |
| **Hacker News (Show HN title)** | Show HN: fold – open-source MCP gateway in Go (40/40 conformance) |
| **Go module / release page** | MCP gateway: federate any number of MCP servers into one governed endpoint. OAuth resource server, EMA/ID-JAG, RFC 8693 token exchange, deny-by-default policy, audit on every request. `go install github.com/fold-run/fold/cmd/fold@latest`. |
| **Twitter/X bio** | the enterprise MCP gateway. one governed endpoint for every MCP server. open source, Apache-2.0. try it: demo.fold.run/mcp |
| **Conference-hallway verbal** | "You know how every MCP client ends up wired to a dozen servers, each with its own auth and its own credentials? fold sits in the middle — clients see one governed server, and federation, auth, policy, and audit happen behind it. Point your client at demo.fold.run/mcp and you're looking at it." |

Rule: every written one-liner must carry at least one verifiable hook (license, conformance number, demo URL, install command, or deployment shape). No adjectives without receipts.

---

## 3. Message house

### Core message

**Every MCP client, every MCP server, one governed endpoint.**

fold is the layer where MCP sprawl becomes infrastructure: clients connect once, and everything behind that connection — identity, policy, audit, traffic protection — is handled by the gateway.

### Pillar 1 — One governed endpoint

*The claim:* fold federates any number of upstream MCP servers into a single virtual server with namespaced tools. Clients configure one URL; platform teams control what's behind it.

*Proof:*
- Live, no-signup demo: **demo.fold.run/mcp** — three public MCP servers federated behind one endpoint, inspectable from any MCP client, with the federation visible live at demo.fold.run/console.
- Namespaced tool federation with deny-by-default, per-principal visibility: two principals hitting the same endpoint see different tool sets, and what a principal can't call it can't see.
- Federated tasks with affinity routing: ownership is remembered at mint (never encoded in the id), located by probe when the record is missing, and bound to the minting principal — another caller's poll answers exactly like an unknown id.
- Server-initiated traffic bridges both ways: sampling, elicitation, progress, and log messages route back over the originating call's own stream.
- Upstreams join without a config change: on Kubernetes, label a Service `fold.run/upstream: "true"` and `fold-discovery` publishes it to the gateway.
- One config document, hot-reloaded: `SIGHUP` or `--watch` swaps the federation atomically without dropping the listener; unchanged upstreams keep their sessions.

### Pillar 2 — Enterprise-grade by default

*The claim:* the controls an enterprise needs are the defaults, not an add-on tier. Nothing passes through fold unauthenticated, unauthorized, or unaudited.

*Proof:*
- OAuth 2.0 resource server, Enterprise-Managed Authorization (ID-JAG exchange), and RFC 8693 token exchange: a client presents one token; fold exchanges per upstream, and upstream credentials never leave the server side.
- Deny-by-default policy: a tool is invisible until a rule grants it to a principal — invisibility plus call-denial is the enforcement pair.
- Audit record on every terminal response — including 401s, denials, and rate limits. Not sampled, not optional.
- Traffic protection built in: global, per-principal, and per-upstream rate limits, plus per-upstream circuit breakers.
- All of it Apache-2.0, in one repo: github.com/fold-run/fold. No enterprise SKU gating the security features.

### Pillar 3 — Aligned with 2026-07-28. And watched, not promised.

*The claim:* fold ships conformant with the current spec, and staying conformant is machinery, not a roadmap item.

*Proof:*
- **40/40 official conformance checks** — the `@modelcontextprotocol/conformance` suite runs against fold fronting the reference server, gated on every merge.
- A weekly drift job re-runs the suite against the *latest unpinned* SDK and conformance release and opens a tracking issue on any failure — spec movement is noticed by CI, not by users.
- Built on the official MCP Go SDK on both sides of the proxy: fold never hand-rolls protocol framing, so wire behavior tracks the SDK's.
- Gaps are documented, not hidden: the README's "Not implemented" section names them (with a drift-canary test that fails the moment the SDK unblocks the biggest one).
- **~0.2 ms added p50 overhead**, with a CI gate at < 5 ms on every merge. Governance that costs less than a DNS lookup.
- **~9,300 req/s sustained** `tools/call` per instance at 64 concurrent client sessions (p99 ≤ 19 ms; 13,400 req/s at 256, zero errors) — methodology and the reproduce-it-yourself harness in docs/benchmarks.md.
- A frozen v1 compatibility contract: config document, CLI, wire surface, and embedder API — with every default a recorded decision (docs/defaults.md).

---

## 4. Key messages per audience

### (a) Senior platform / AI-infra engineers at enterprises

*What they care about:* blast radius, credential handling, audit trails, not owning another bespoke proxy, operational cost of another hop.

*Message:* fold turns N client-to-server MCP connections into one governed edge. Auth is real OAuth with per-upstream token exchange — upstream keys never reach clients. Policy is deny-by-default per principal. Every request is audited. And it deploys like infrastructure you already run: a single static binary or a ~22 MB distroless container, Helm chart with probes and HPA, hot config reload, Prometheus metrics, and OpenTelemetry spans that carry the policy decision.

*Proof to lead with:* token exchange (RFC 8693) mechanics, deny-by-default visibility, audit-on-every-request, ~0.2 ms added p50 (CI-gated), hot reload without dropping the listener, Kubernetes self-serve discovery.

### (b) MCP community / OSS contributors

*What they care about:* protocol correctness, spec depth, license, whether this is a real open project or a lead magnet.

*Message:* fold is a from-the-spec implementation of the hard parts of running MCP behind one endpoint — federated tasks with affinity routing and principal-bound ownership, server-initiated bridging over the originating stream, an embedded EMA authorization server — built on the official Go SDK, Apache-2.0, all in the open. Where the SDK blocks a feature, the README says so and a drift canary watches for the unblock.

*Proof to lead with:* 40/40 conformance gated per merge and re-verified weekly against the latest SDK, the live demo (demo.fold.run/mcp — federated tasks are pollable there right now), docs.fold.run with /llms.txt, `go run github.com/fold-run/fold/cmd/fold@latest` to a running gateway in one command, release artifacts with SBOMs and sigstore build provenance, the documented "Not implemented" section as evidence the claims are real.

### (c) Engineering leaders (VP / staff+ who approve adoption)

*What they care about:* risk of adding a dependency in the request path, compliance posture, lock-in, whether this survives the spec churn.

*Message:* MCP adoption is outrunning MCP governance. fold is the control point: one endpoint your security team can reason about, with authentication, authorization, allowlists, and a complete audit trail — instead of dozens of direct client-to-server connections nobody can enumerate. It's Apache-2.0 with no feature-gated core, it adds ~0.2 ms p50, its conformance is enforced by CI on every merge, and its v1 surface is a frozen compatibility contract.

*Proof to lead with:* audit completeness, credential brokering (keys stay server-side), open license, conformance numbers, supply-chain posture (SBOMs, sigstore provenance, govulncheck in CI), the frozen v1 contract as evidence the dependency won't churn under you.

---

## 5. Objection handling

Tone rule: agree with the legitimate instinct behind the objection first, then answer with specifics. Never defensive, never dismissive.

**"Another gateway/wrapper. This is a proxy with branding."**
Fair instinct — most "AI gateways" are HTTP proxies with an allowlist. The test is protocol depth: fold federates tasks across upstreams with affinity routing and principal-bound ownership, bridges server-initiated sampling and elicitation back over the originating call's stream, filters every list per principal so denied tools are invisible rather than erroring, and embeds an EMA authorization server for ID-JAG exchange. A generic proxy can't do any of those because they require understanding MCP semantics, not just forwarding bytes. Check the conformance suite: 40/40, gated on every merge.

**"Solo-maintainer risk. What happens when you get bored?"**
The honest mitigations: Apache-2.0 means the code is yours regardless; conformance is enforced by CI on every merge and a weekly job tracks the latest SDK automatically, so keeping pace doesn't depend on one person's attention; the v1 surface is a frozen compatibility contract; and the surface area is deliberately a gateway, not a platform. Bus-factor is a real question for any young project — evaluate the repo's automation, fuzzers, and race-detector test suite, not our promises.

**"Why not build this into the client or the server?"**
Because the concerns are N×M. Every client re-implementing auth, policy, audit, and credential handling against every server is exactly how we got API gateways twenty years ago. A client can't enforce an org's deny-by-default policy on itself, and a server can't audit requests it never sees. The trust boundary belongs in the middle, owned by the platform team.

**"What does it cost me in latency?"**
~0.2 ms added p50 on typical hardware, and CI fails any merge that pushes added p50 over 5 ms. Rate limits and circuit breakers are there to protect upstreams, not to slow the happy path. Don't take the number — reproduce it: `make bench` in the repo runs the same instrument CI does, direct vs through-fold, and prints the percentiles.

**"Open-core rug-pull. The auth and audit will end up in an enterprise tier."**
Everything described here — OAuth resource server, Enterprise-Managed Auth, token exchange, policy, audit, rate limiting, the console — is in the Apache-2.0 repo today. There is no private enterprise fork. Apache-2.0 is not revocable for shipped code: if we ever changed course, you keep everything and can fork. Judge the license, not the roadmap.

**"The spec keeps moving. How do I know you stay aligned?"**
Because alignment is machinery, not a sprint: the official conformance suite gates every merge at a pinned version, and a weekly job re-runs it against the latest unpinned SDK and suite, opening a tracking issue the moment anything drifts. fold is built on the official Go SDK on both sides of the proxy, so protocol framing tracks upstream by construction. And where the SDK blocks a feature, the README's "Not implemented" section says so in public — with a canary test that fails when the blocker lifts. Documented gaps over quiet ones.

---

## 6. Words we use / words we avoid

### We say

- **the enterprise MCP gateway** — the category, with the definite article; earn it with proof in the same breath.
- **one governed endpoint** — the core noun phrase; prefer it to "single pane of glass" or "control plane."
- **federate / federation** — for combining upstreams. Not "aggregate," not "mesh."
- **deny-by-default** — the exact policy posture, always hyphenated.
- **principal** — for the authenticated identity; "user" only when it's literally a human.
- **upstream / client** — directional and unambiguous.
- **list caching** — never "response caching"; fold's cache is a TTL over list results.
- **a single static binary** — the deployment story in five words; pair it with the container and Helm variants when the audience is Kubernetes-shaped.
- Specific numbers with units and percentile: "~0.2 ms added p50," "40/40 conformance checks," "CI-gated at < 5 ms."
- Imperatives with a URL or command: "point your MCP client at demo.fold.run/mcp," "run `go run github.com/fold-run/fold/cmd/fold@latest`," "pull `ghcr.io/fold-run/fold`."
- **fold** — lowercase, always, everywhere, including sentence-initial.

### We avoid

- **Hype adjectives:** blazing, seamless, effortless, magical, game-changing, revolutionary, next-gen, supercharge, unlock, unleash.
- **Vague scale words:** "enterprise-ready" without a control named in the same sentence; "secure" as a bare adjective (say *what* is enforced); "performant" (give the number).
- **AI-washing:** "AI-powered gateway," "intelligent routing" — fold is deterministic infrastructure for AI systems, not an AI system.
- **Category mush:** "platform," "ecosystem," "solution," "AI middleware" — we are a gateway.
- **Unverifiable comparatives:** "the fastest MCP gateway," "the most secure" — we publish our numbers and let readers compare.
- **Numbers without instruments:** every figure in copy must trace to a runnable instrument in the repo (`make bench`, `make loadtest`) — the current defensible set: ~0.2 ms added p50, ~9,300 req/s at 64 connections (p99 ≤ 19 ms), 40/40 conformance. Quote `tools/call` throughput, never `tools/list` (it rides the list cache).
- **Retired TS-era claims:** protocol-era translation, SEP-2549 caching, Cloudflare Workers as a runtime, `subscriptions/listen` fan-in. Gone from the product, gone from the copy. (demo.fold.run is back — Go-backed — but carries none of these claims: it demonstrates federation and governance, not translation.)
- **False modesty and filler:** "we think," "arguably," "simply," "just." State it or cut it.
- **Roadmap-as-fact:** never present unshipped work in present tense. If it isn't in the repo, it isn't in the copy.

### Voice check (before/after)

- Before: "fold seamlessly unifies your entire MCP ecosystem with enterprise-grade security."
- After: "fold federates your MCP servers into one endpoint. Deny-by-default policy, OAuth with per-upstream token exchange, and an audit record on every request."
