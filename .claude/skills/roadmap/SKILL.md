---
name: roadmap
description: Groom fold's gap list and roadmap — README "Not implemented", docs/roadmap.md's three horizons, non-goals, deprecations on the clock, and the drift canaries that fail when a gap closes. Use when closing or widening a gap, when deciding whether something is a non-goal, or when a canary test fires.
---

# The gap list and the roadmap

These are not marketing documents. `gateway/era_test.go` and
`gateway/listen_test.go` fail with messages that **point the reader at
them**, and CLAUDE.md's fourth rule makes updating them a merge condition.
A gap here is load-bearing text.

Two surfaces, different jobs:

- **README "Not implemented"** — what fold does not do *today*, with the
  reason. Read by someone evaluating fold, so each entry has to survive a
  skeptic: name the gap, name the cause (structural, waiting on someone
  else, or deliberate), and say what would close it.
- **docs/roadmap.md** — what fold intends, in three horizons plus
  "Non-goals" and "Deprecations on the clock". No dates, deliberately.

Every "Not implemented" entry has a roadmap counterpart. The README says
what is missing; the roadmap says whether it is coming.

## The four vetoes

`docs/roadmap.md` "How to read this" names what kills an item regardless of
demand:

1. the v1 compatibility contract and the frozen defaults,
2. the invisibility rule,
3. the added-latency gate,
4. audit as the single exit door.

**A feature that requires breaking one does not get built — it gets a
non-goal entry with the reasoning written down.** That conversion is the
main judgement this skill exists for. Content inspection is the worked
example: it is a non-goal because inspecting bodies means buffering and
rewriting, which conflicts with (2) and (3) — and the entry still offers
the fold-shaped answer (the decision hook) rather than stopping at "no".

A non-goal entry that only says no is incomplete. Say what the veto was and
what someone with that need should do instead.

## Closing a gap

When a change closes one, the same PR does all of it:

- [ ] Remove or narrow the README "Not implemented" entry — narrowing is
      common and honest: three consequences, one addressed, two named.
- [ ] Update the roadmap counterpart (move it out of its horizon, or strike
      it).
- [ ] **Find the canary.** If the gap was waiting on someone else, there is
      probably a test that fails when the wait ends. Flip or delete it, and
      never "fix" one by loosening the assertion.
- [ ] CHANGELOG entry if it ships in a release.

## The canary pattern

The repo's answer to "waiting on the SDK" is a test that fails when the
wait ends, so nobody has to notice:

- `gateway/listen_test.go` — fails when the SDK lifts the stateless-only
  restriction on the 2026-07-28 protocol.
- `gateway/era_test.go` — asserts the gateway *refuses* an era it was never
  audited for, and its failure message enumerates the work to do first
  (relayed results carry no `resultType`, `resources/subscribe` and
  `logging/setLevel` are advertised though the era retired them, the
  bridged-session apparatus has no counterpart there).

Two things make these work and both are easy to lose:

- **Assert the outcome, not the mechanism.** `era_test.go` pins that the
  request was *refused*, not how — the SDK already changed the shape once
  (HTTP 400 → JSON-RPC error), and a canary asserting the status would have
  failed on that bump for a reason nobody cares about, "which is how a
  canary gets deleted rather than read."
- **The failure message is the handoff.** It should tell whoever hits it
  what to do, not just that something changed.

Widening a gap deserves a canary too. If a new gap waits on an external
decision, leave a test that fires when the decision lands.

## Adding to the roadmap

- **Horizon 1** — scoped tightly enough to estimate.
- **Horizon 2** — themes, with the design questions named rather than
  answered.
- **Horizon 3** — gated: honest about what is waiting on someone else.
- **Deprecations on the clock** — anything with an external end date.
  `/spec-drift` and `/mcp-spec` feed this one; a deprecation the
  specification has scheduled belongs here the day it is announced, not the
  day it bites.

Ordering changes with what operators ask for, so a new item does not need
to justify its position — only its horizon.

## Checks

```bash
go test ./internal/doclint    # every Markdown link and anchor resolves
go test ./gateway -run 'TestEra|TestListen'
```

`doclint` is why a heading rename in the roadmap can break the README:
links between them are checked, including anchors.
