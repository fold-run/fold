# fold — public launch plan (relative-day template)

Launch type: public launch of an open-source project, anchored on Hacker News.
Primary CTA everywhere: **hit demo.fold.run/mcp** (secondary: `go run github.com/fold-run/fold/cmd/fold@latest`, star github.com/fold-run/fold).
Message: one line, from messaging.md — *fold is the open-source enterprise MCP gateway: every MCP client, every MCP server, one governed endpoint — 40/40 official conformance, gated in CI on every merge.*
Owner of everything: Blake. Assets marked **[done]** are already live and need no work beyond a final check.

Ported from the archived TypeScript repo's Aug 3–14 plan and rewritten against the Go implementation. Structural changes from the TS plan: the demo is back (Go-backed as of 2026-08-05 — the unmodified released binary, v1.4.1 today, in a container at demo.fold.run) but demonstrates federation/governance, not era translation; and the Cloudflare DevRel channel is gone with the Workers runtime, replaced by Go-ecosystem channels. Dates are relative (L = launch day) — pin L to a Tue/Wed/Thu morning and the rest follows.

---

## 1. Launch thesis

The 2026-07-28 spec release rewrote MCP days ago, and fold can prove alignment rather than claim it: a published 40/40 run of the official conformance suite, gated in CI on every merge and re-verified weekly against the latest SDK — plus a live gateway anyone can point a client at, no signup. That proof gap decays the moment any competitor ships their own conformance run, so we anchor on Hacker News promptly rather than polishing for another cycle. The supporting hook is the deployment story the TS version never had: a single static Go binary — no runtime, no sidecar stack — that a skeptical engineer can have running in under a minute after trying the demo. Everything else — community, syndication, newsletters — is sequenced to convert the HN moment into durable adoption rather than to create separate moments we don't have the hands to run.

## 2. Goals & metrics (by end of L+9)

Realistic for a solo OSS launch with zero paid budget. The leading indicator is the HN result; everything else follows from it.

| # | Metric | Target | Why this number |
|---|--------|--------|-----------------|
| 1 | HN Show HN result | Front page (top 30) for 2+ hours; 100+ points | The single lever that drives every other metric; achievable for a spec-timely infra post with a one-command quickstart |
| 2 | GitHub stars | +400 (stretch +800 if HN top 10) | Typical range for a front-page Show HN on dev infra |
| 3 | Demo traffic | 1,000+ MCP requests through demo.fold.run in the window (Workers analytics on `fold-demo`) | The differentiating metric — proves people *tried it*, not just read about it |
| 4 | Installs | 500+ combined GitHub release downloads + ghcr.io pulls in the window | The "people ran it themselves" proxy, one step deeper than the demo |
| 5 | Quality inbound | 5+ substantive conversations with platform/AI-infra engineers (GitHub issues/discussions, email, DMs) | The strategic audience; 5 real threads beats 50 drive-by stars |

Track daily in a plain text log: stars, release-asset download counts, ghcr pull count, GitHub traffic/referrer graphs. 15 min/evening. (The fold.run properties run no analytics — GitHub's numbers are the instrument.)

## 3. Day-by-day sequence

Total effort budgeted at 4–8 hrs/day; launch day is a full day. Times are US Pacific.

### Pre-launch: L−3 → L−1

**L−3 (~2 hrs, optional but recommended)**
- Confirm the anchor post. **[done]** — *"Federating MCP tasks: affinity routing over opaque ids"* is live at fold.run/blog/federating-mcp-tasks/ with permalinks pinned to the repo. Fresh-eyes pass for anything stale; it is the link target for everything below.

**L−2 (~6 hrs)**
- **Demo redeploy to the current release** (30 min). The demo container pins a release in `apps/demo/Dockerfile` in the fold.run repo; redeploy it to the newest one before launch, then update the two version claims that describe it (messaging.md §demo, and this file's header). The copy must never name a version the demo is not actually running — that is the one claim a visitor can falsify in ten seconds with `curl demo.fold.run/health`.
- **Demo hardening** (1.5 hrs). demo.fold.run must survive an HN spike: confirm the 300 req/min rate limit answers cleanly, trip each upstream mentally — what does a visitor see when cf-docs or gitmcp is down? (`_meta["run.fold/partialFailure"]` plus the remaining namespaces still serving — that's the product working; know the story before Wednesday.) Confirm fold.run/status is green and the console loads. Decide now what "demo degraded" looks like so launch morning isn't the first time you find out.
- **Quickstart hardening** (1.5 hrs). The second touch after the demo: run `go run github.com/fold-run/fold/cmd/fold@latest --config fold.config.json` cold on a clean machine (no Go module cache), and `docker run ghcr.io/fold-run/fold` on amd64 and arm64. Time both; the README's "60 seconds" claim must be true on a cold cache or it changes. Verify the error a user sees with a bad config is the validation message, not a stack trace.
- **Repo hygiene final pass** (2 hrs). Verify: README answers "what/why/try it in 60 seconds" above the fold; repo description matches the messaging.md one-liner; CONTRIBUTING + issue templates exist; the conformance receipt (CI run link) is prominent. **Two hard gates from the channel copy:** the bench methodology must be public (**[done]** — `bench/latency_test.go` + docs.fold.run/benchmarks) and the community CTA targets GitHub Discussions (link a Discord only if a live server exists by launch day).
- **Awesome-list PRs** (1 hr). Open PRs to awesome-mcp-servers / awesome-mcp adding fold with the one-line description. PRs take days to merge — opening now means they land during launch week. Follow each list's contribution format exactly; one-line entry, no sales copy.
- **Uptime check** (15 min). fold.run/status is public and MCP-pings the demo every 5 minutes — confirm all targets green before pointing an HN thread at the properties.

**L−1 (~5 hrs)**
- **MCP community presence, quiet mode** (1.5 hrs). Join/re-engage official MCP Discord and MCP GitHub Discussions *as a contributor*: answer two or three open questions about 2026-07-28 or the Go SDK where you genuinely know the answer. No links to fold unless directly relevant. Credibility pre-seeding — the announcement post should come from a name people saw yesterday, not a stranger.
- **Launch-day kit** (2 hrs). Pre-write and stage from channel-copy.md: Show HN title + body + first comment, X/Twitter thread (5–7 tweets: spec change → hard parts → receipts → quickstart), LinkedIn post (platform-engineer framing from messaging.md §4a), objection-response crib sheet (messaging.md §5 printed/pinned — you will use it verbatim in HN comments). Dry run the quickstart one final time, cold.
- **Go/no-go check** (15 min, end of day): quickstart clean on two platforms; receipts one click deep; crib sheet ready; calendar cleared. Any red → slip launch by a day or two, not later.

### Launch day: L (Tue/Wed/Thu only)

**6:00am** — Final smoke test: blog post, docs, status page, demo handshake from two different MCP clients, console, `go run` quickstart, all links in the post.
**7:00am** — **Submit Show HN.** Title (final, per channel-copy.md): `Show HN: fold – open-source MCP gateway in Go (40/40 conformance)`. URL: the blog post (not the bare repo — the post carries the argument; repo and demo are one click in). Immediately add the first comment: 3 short paragraphs — why you built it, what's technically interesting (federated task ownership, server-initiated bridging, credential brokering), and an explicit invitation to connect any MCP client to demo.fold.run/mcp right now. 7:00–8:00am PT midweek is the highest-odds window: enough early US-East traffic to build velocity before the front page turns over.
**7:15am** — Post the X thread and LinkedIn post. Do **not** link the HN thread from either, and never ask for votes anywhere — HN's voting-ring detection penalizes it and the community torches it.
**7:30am–7:00pm** — **Live in the HN thread all day.** This is the day's only real job. Answer every substantive comment within minutes; lead with agreement per messaging.md §5 tone rule; link receipts, not adjectives. Watch fold.run/status and the fold-demo Workers analytics; if an upstream degrades, say so in-thread before someone else does. If someone reports the quickstart failing on their platform, treat it as the day's top-priority bug: reproduce, acknowledge in-thread, fix or scope it honestly before evening.
**Midday** — If the post is on the front page, add a short comment when something notable happens in the demo traffic ("~N MCP requests through the demo since this morning" from Workers analytics) — concrete, verifiable, not a victory lap.
**Evening** — Log metrics. Reply to every GitHub issue/discussion opened today, even briefly. Queue tomorrow's community posts.

Effort: full day, ~10 hrs. Nothing else is scheduled.

### Follow-through: L+1 → L+9

Community posts are deliberately spaced one channel per day so no community sees fold arriving as a blast.

**L+1 (~4 hrs)**
- **MCP Discord** — one post in the appropriate show-and-tell/projects channel, from channel-copy.md §2: written fresh for Discord, not pasted from HN. Then stay present and answer questions.
- **MCP GitHub Discussions** — a substantive post oriented to implementers: notes from implementing federated tasks and server-initiated bridging on the official Go SDK, with fold as the working example. Contribution first, announcement second.
- Continue HN thread replies (day-2 tail), triage new issues.

**L+2 (~3 hrs)**
- **r/mcp** — self-post framed as "implementation notes", linking the blog post. Reddit rewards the founder answering questions in-thread; budget an hour for it.
- **lobste.rs** — submit the blog post (if you have an invite; if not, skip — never ask publicly for one during a launch). Norms: technical substance only, tag correctly, be present in comments, no marketing tone.
- Ship a small visible improvement from launch feedback (a doc fix, an issue closed) — signals the repo is alive to the week's new watchers.

**L+3–L+4, weekend (light, ~1 hr/day)**
- Reply to issues/discussions/DMs. Log metrics. Draft week-2 pitches (below). No new channel posts on the weekend — dead traffic, spends a channel for nothing.

**L+5 (~4 hrs)**
- **Syndication**: republish the anchor post to dev.to (and Hashnode if the account exists) with `canonical_url` set to fold.run — never before day 5; the canonical original must win the search index first.
- **Newsletter pitches** (now, because the HN result is the proof): short, personal notes — not press releases — to TLDR (webdev/AI editions), Latent Space, **Golang Weekly**, and 2–3 MCP/AI-infra curations. Three sentences: what fold is (one-liner), the timing hook (a published 40/40 conformance run, CI-gated), and the HN/star/install numbers. No follow-up nagging; one polite bump the following week is the max.

**L+6 (~3 hrs)**
- **Go-ecosystem angle** (replaces the TS plan's Cloudflare DevRel beat): fold is a substantial production consumer of the official MCP Go SDK — engage where the SDK maintainers and users already are (the SDK repo's discussions, Gophers Slack #golang-newbies is wrong but a relevant channel like #general-frameworks or an MCP channel if one exists). Contribute the "what we hit building a gateway on the SDK" notes — including the documented `subscriptions/listen` blocker, which is directly actionable feedback for the SDK team. **r/golang** — post only with a genuinely Go-shaped framing ("a single-binary MCP gateway: what the stdlib + official SDK gave us"); if the framing feels forced, skip.
- Check awesome-list PRs; nudge politely if unreviewed.

**L+7 (~3 hrs)**
- **Podcast outreach** (long-lead planting, not this-week wins): pitch 3–4 shows where MCP infrastructure fits (Latent Space, Software Engineering Daily, Go Time, MCP-specific pods). Angle: "what the 2026-07-28 rewrite actually means for teams running MCP in production" — the founder as spec sherpa, not vendor. Expect bookings next month; that's fine.
- **r/LocalLLaMA** — post *only if* framed for that audience: governing local/self-hosted MCP servers behind one endpoint, single binary, no cloud dependency (a genuinely better fit for the Go build than it was for the TS one). If the framing feels forced, skip.

**L+8 (~3 hrs)**
- Follow-ups: newsletter bumps only where there was any reply; respond to podcast threads; merge community PRs if any arrived (fast-merge the first external PR — it converts a contributor).
- **Second content beat**: publish a short follow-up post — the most defensible Go deep-dives are the discovery security model ("labeling rights must not become secret-exfiltration rights") or principal-bound task ownership. Post to X/LinkedIn; do not re-run the community circuit for it.

**L+9 (~2 hrs)**
- Retrospective against §2 targets: what converted (GitHub referrer data), which channel produced the 5+ quality conversations, what the inbound asked for (this is the seed of the commercial roadmap).
- Thank-yous: everyone who commented substantively, merged a PR, or made an intro. Individually, briefly.
- Decide the next beat (next minor release, the discovery deep-dive, or first podcast) so momentum has a destination.

## 4. Channel rationale and rules of engagement

**Anchor blog post → Show HN (KEEP, the spine).** HN is the one channel where "solo engineer implements a hard spec correctly, with receipts" is the house genre. Rules: submit the essay, not the repo; founder present in-thread all day; never solicit votes, shares, or "support" anywhere; answer criticism with the messaging.md §5 tone — concede the legitimate instinct, then receipts. One submission; resubmission rules in §6.

**MCP Discord + GitHub Discussions (KEEP).** Contribute before, announce after. These are the people who can validate or dismantle the conformance claim in public — arriving as a known contributor changes the reception entirely. Rules: contribute answers before dropping links; one announcement post per venue, ever; then presence, not promotion.

**Awesome-list PRs (KEEP, pre-launch).** Zero-noise, compounding discovery. Rules: match the list's format exactly; the one-liner, nothing more.

**r/mcp (KEEP).** Small but exactly the ICP. Rules: implementation-notes framing, founder answers everything, one post.

**lobste.rs (KEEP, conditional).** Higher signal-to-noise than HN for infra. Rules: invite-only — post only if you have an account; substance-only culture; a day or two after HN so it doesn't read as a blast.

**X/Twitter + LinkedIn founder posts (KEEP).** Owned, free, and where platform-engineering leaders actually see things (LinkedIn especially, per messaging.md §4c). Rules: thread with receipts on X; the engineering-leader message on LinkedIn; never link the HN thread on launch morning.

**dev.to/Hashnode syndication (KEEP, week 2 only).** Pure reach extension. Rules: canonical_url to fold.run always; wait until the original has indexed.

**Newsletters (KEEP, week 2, Golang Weekly added).** TLDR/Latent Space placement is the best free reach into AI-infra engineers; Golang Weekly is the Go-native equivalent and a single-binary infra tool is squarely its beat. Rules: three-sentence personal pitch; numbers, not adjectives; one bump max.

**Go SDK community + r/golang (NEW — replaces Cloudflare DevRel).** The TS plan's sleeper channel was Cloudflare amplifying a Workers showcase; that runtime is gone. The Go-native equivalent: fold is a serious production consumer of the official MCP Go SDK, and "here's what we hit building on it" (including the documented listen-stream blocker) is a contribution the SDK community values — with fold's credibility as the byproduct. Rules: contribute findings, not links; r/golang only with a Go-shaped framing.

**Podcasts (KEEP, week 2, Go Time added).** Long-lead; nothing lands inside the window. Rules: pitch the spec-migration topic, not the product.

**r/programming (CUT).** Too generic; niche protocol infrastructure gets buried or dogpiled, and it duplicates HN's audience at lower quality.

**r/LocalLLaMA (CONDITIONAL).** Better fit than it was for TS — a single self-hostable binary with no cloud dependency is this audience's shape — but still adjacent to the ICP. Post only with a genuinely local-first framing; otherwise skip with no regret.

**Product Hunt (SKIP — see §5).**

## 5. Product Hunt: skip this cycle

Position unchanged from the TS plan, and the reasoning got stronger: **do not launch on Product Hunt in this window, and probably not for this product in its current form.** PH's audience is product-curious generalists and indie-SaaS buyers; fold's buyer is a platform engineer who evaluates via the repo, the conformance run, and the quickstart — none of which PH surfaces well. A dev-infra OSS tool typically converts PH traffic into vanity upvotes, not stars, installs, or quality inbound, and a mediocre PH result becomes a permanent public artifact. The one future scenario where PH earns its day: a hosted/managed fold offering with self-serve signup — that's a PH-shaped product. Revisit then. (The tagline and maker's note are staged in channel-copy.md §3 for that day.)

## 6. Risk & contingency

**HN post flops (off front page within an hour, <10 points).**
- Do nothing rash. Leave it; never delete-and-repost the same day, never ask anyone to vote.
- Same week: continue the plan unchanged — Discord, r/mcp, and lobste.rs do not depend on HN.
- Relaunch rules: HN tolerates one re-submission of a post that got no traction. Wait ~1 week, revise the angle (lead with the quickstart: "Show HN: a single-binary MCP gateway — federate your servers in one command", or lead with the governance story), and resubmit **Tue/Wed, 7:00am PT**. Also email hn@ycombinator.com asking for second-chance pool consideration — moderators do this routinely for substantive posts that missed their window.
- If the resubmission also stalls: the content still works; shift weight to the Go-ecosystem channel and newsletters, which don't care about HN score.

**Demo degrades or goes down under load.**
- Prevention is L−2's hardening pass: fold's own 300 req/min rate limit, per-upstream circuit breakers, and the uptime monitor's 5-minute MCP ping (which doubles as the container keep-warm).
- If an upstream trips during launch: the gateway degrades visibly and gracefully — that's the product working. Say so in the HN thread immediately and turn it into a demonstration: "gitmcp is timing out right now; note `_meta[\"run.fold/partialFailure\"]` in tools/list and the other two namespaces still serving — that's the breaker."
- If the demo is fully down (container or Cloudflare incident): post the status in-thread within minutes, point to the `go run` quickstart as the alternative first-touch, fix, then post the resolution. A founder narrating an incident honestly on HN gains credibility; silence loses it. fold.run/status is the public receipt either way.

**Quickstart breaks in public.**
- Prevention is L−2's cold-cache runs on both architectures. The remaining risk surface is platform-specific: an OS/arch combination that wasn't tested, a Go toolchain version mismatch, a proxy-hostile corporate network.
- If a thread reports it: acknowledge in-thread within minutes, reproduce immediately, and either fix (a doc note, a release patch) or scope it honestly ("broken on X, tracked in #NN, the container path works there"). A founder narrating a fix honestly on HN gains credibility; silence loses it.
- The container is the fallback first-touch: `docker run ghcr.io/fold-run/fold` has no toolchain dependency — keep it in the crib sheet as the answer to "go run failed."

**Conformance claim challenged publicly ("40/40 is marketing math").**
- The most likely substantive attack, and the receipts are strong. Respond with messaging.md §5's prepared answer: conformance is gated in CI on every merge at a pinned suite version, a weekly job re-runs it against the latest unpinned SDK, and the run link is public. Invite them to run the suite themselves — `make conformance` in the repo is the same command CI executes.
- If someone finds a real gap: thank them in-thread, open the issue publicly within the hour, fix or scope it honestly. "40/40 on the official checks, and here's the edge case @user found, tracked in #NN" is *stronger* than an unchallenged claim. Never litigate. The README's "Not implemented" section is the standing proof that documented gaps are house style.
- Pre-commitment: before launch, pin the latest conformance run link in the README so the receipt is one click away all day.

## 7. What we deliberately do NOT do in these two weeks

- **No paid anything** — ads, sponsorships, paid newsletter placements. Paid reach into a skeptical infra audience before organic proof exists converts terribly and taints the "judge the repo" positioning.
- **No outbound sales or commercial motion** — no pricing page, no "book a demo," no enterprise-tier teasers. The open-source credibility being built is the future commercial funnel's foundation; monetization signals now feed the open-core rug-pull objection directly (messaging.md §5). Quality inbound gets a conversation, not a pitch.
- **No press-release/PR-agency motion** — journalists don't move dev-infra adoption; the channels above reach the ICP directly.
- **No multi-community carpet-bombing** — one venue per day, written natively for each. Simultaneous cross-posting is the fastest way to convert goodwill into spam-flags in small communities.
- **No new feature work during launch week** — the repo ships responsiveness (issue triage, doc fixes, fast first-PR merges), not features. The conformance story is about reliability; a launch-week regression would be the worst possible irony.
- **No numbers without instruments** — per messaging.md §6, every figure in any thread must trace to `make bench` or `make loadtest`; under pressure, link docs/benchmarks.md rather than improvising a comparison.
- **No Product Hunt** — per §5.
- **No roadmap promises in any thread** — messaging.md's rule stands under pressure: if it isn't in the repo, it isn't in the copy, including HN comments at 9pm on launch day.
