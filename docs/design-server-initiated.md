# Design: governing server-initiated requests

Status: **proposed**. This records the design for governing the traffic that
flows *from* an upstream *to* the caller — `sampling/createMessage` and
`elicitation/create` — which fold bridges today and does not govern. It settles
the questions that surface raises before any of it is built.

## Motivation

fold's posture is deny-by-default, and it applies to exactly one direction.
Every client→upstream invocation passes host validation, authentication, rate
limits, tenancy, and policy before it reaches an upstream, and leaves exactly
one audit event. The reverse direction passes nothing.

Bridged sessions (`gateway/upstream.go`, `bridgedSession`) exist so an upstream
can reach the originating client: sampling, elicitation, logging, and progress
ride back over the in-flight call's own stream. The handlers that do this
(`gateway/gateway.go`, `bridgeOptions`) forward unconditionally, mirroring
whatever capabilities the downstream client declared at initialize. Grep the
policy package for either method and there is nothing to find.

Two consequences an enterprise reviewer will reach before we do:

| An upstream can | Because |
|---|---|
| spend the caller's model budget, unbounded, by calling `sampling/createMessage` in a loop | no limiter, no budget, and no policy governs the reverse path |
| put an arbitrary prompt in front of the caller's human via `elicitation/create` | the elicitation is forwarded verbatim; fold has no opinion on who may ask |

Neither is exotic. Sampling exists so servers can borrow the client's model —
the borrowing is the feature — and elicitation exists so servers can ask the
human a question. Both are legitimate, both are expensive, and both are
currently ungoverned in a gateway whose entire claim is that it governs.

There is a second, quieter argument. This capability is only reachable at all
because fold keeps per-client bridged sessions; a gateway that proxies lists
and calls without session bridging has no reverse path to govern and cannot
grow one without building the bridging first. This is depth in the direction
fold already went alone.

## What this is, and is not

An extension of the existing engine to a second direction, not a second engine.
Deny-by-default reasoning, the subject matcher, the audit exit door, and the
minted error registry all carry over unchanged.

It is **not** inspection. fold does not read the sampling messages, score the
elicitation prompt, or judge intent — that is the roadmap's
[declined](roadmap.md#non-goals) inline content inspection, and the answer
stays the external decision hook. What is decided here is structural: *may this
upstream ask this caller for this kind of thing, and how often.*

---

## 1. Where the traffic actually is

Only bridged sessions carry it. `rootSession` (`gateway/upstream.go:600`)
declares list-changed and resource-updated handlers and nothing else, so the
SDK answers a sampling or elicitation request on that session with "method not
supported" — the shared session cannot be a bypass. The whole surface is the
two handlers in `bridgeOptions`, which is a small, closed enforcement point.

## 2. Whose decision is it

The reverse request arrives on an SDK client goroutine with no request context
of its own. `invocationCtx` (`gateway/gateway.go:1097`) already resolves it to
the in-flight invocation's context, and that context is the downstream request
context pushed by `pushCallCtx` at `gateway/router.go:306` and `:376` — the same
one `authorize` reads. So `auth.PrincipalFromContext` works inside the handler,
and the decision has both dimensions it needs:

> may upstream **U** ask principal **P**'s client to sample?

The upstream identity needs one small change to reach the handler.
`bridgeFor(req)` closes over the downstream session only; the thunk it returns
is resolved inside `bridgedSession`, which is per-upstream. Both call sites have
`u` in hand, so `bridgeFor(req, u)` is the whole change.

## 3. Config shape

Reuse `allow[]`/`deny[]` with the existing `methods` field. There is nothing to
name in either request, so a rule is server-and-method only:

```jsonc
{
  "policy": {
    "defaultDecision": "deny",
    "serverInitiatedDecision": "deny",
    "rules": [
      {
        "id": "research-may-sample",
        "subjects": { "groups": ["research"] },
        "allow": [ { "server": "corpus", "methods": ["sampling/createMessage"] } ]
      }
    ]
  }
}
```

`serverInitiatedDecision` is one new field, `"allow"` | `"deny"`, defaulting to
`"allow"`.

**The default must be `allow`, and that is not a lapse of nerve.** Today,
sampling flows through every deployment including the ones with
`defaultDecision: "deny"`. Folding the reverse direction under the existing
knob would mean the next minor silently breaks working sampling for every
deny-by-default operator — a change of meaning, which the v1 contract forbids
and the [frozen defaults](defaults.md) review forbids twice. The new field is
what makes the tightening opt-in, and the deploy checklist is where we tell
operators to set it.

Two alternatives were considered and rejected. Reusing `defaultDecision` is the
above. *Implicit arming* — reverse traffic stays allowed until any rule in the
document mentions a reverse method, then flips to deny-by-default — preserves
behavior too and needs no new field, but it makes a document's meaning depend
on a rule somewhere else in it, which is the same failure mode deny-precedence
rejects in [policy depth](design-policy-depth.md).

## 4. Refusal on the wire

A denied reverse request returns a JSON-RPC error **to the upstream**, not to
the client, and reuses `-32042` (policy denied). No new code is minted: the
semantics are identical and the [registry](../README.md) stays at four.

This is spec-shaped rather than adversarial. A client is entitled to decline a
sampling or elicitation request — the protocol expects human-in-the-loop
refusal — so an upstream already has to handle the no. fold is answering the way
a careful client would.

One honest consequence: the caller sees the *effect* of the refusal, not the
refusal itself. A tool that depended on sampling returns whatever it returns
when the client says no, which may be a degraded result and may be an error the
upstream mints. The audit event is where the denial is legible, which is the
same place a policy denial has always been legible.

## 5. The enforcement pair, in reverse

Policy's inbound guarantee is a pair: invisibility plus denial. A caller does
not see a tool they cannot call *and* cannot call it by guessing the name.

The reverse direction has an analogue, and it is worth taking.
`bridgeOptions` mirrors the client's declared capabilities into the upstream
session. Intersect that mirror with policy and an upstream that will never be
allowed to sample **never learns the client can** — it sees a client with no
sampling capability and does not ask. Same shape, same benefit: the upstream
cannot probe for a capability it was not granted.

The honest weakening, written down rather than discovered: capabilities are
declared once, at initialize, and a bridged session outlives the request that
created it. A policy reload during that session's life leaves the declared
capability stale. So the mirror is a hint, not the boundary — **the handler
check is the boundary**, and it is evaluated per request against the current
snapshot. Both halves ship together or neither means anything.

## 6. Rate limits and budgets

A separate bucket from `upstream.rateLimit`, not a share of it. Mixing
directions in one bucket means a chatty upstream's sampling starves the
caller's own invocations, which is a denial of service wearing the costume of a
quota. `state.Limiter` is the primitive; Redis-backed where configured, with
the local fallback the hardening release added, so an outage degrades to
per-instance rather than to nothing.

Budgets count **requests, not tokens**. fold cannot see the model call — the
client makes it, against its own provider, with its own key — so any token
number fold reported would be synthesized. That is the line
[design-consumption.md](design-consumption.md) already drew for the forward
path, and it does not bend because the direction reversed.

## 7. Audit

Reverse requests are terminal responses, so they belong on the exit door. Three
additive changes:

- `Method` carries `sampling/createMessage` or `elicitation/create`. The wire
  surface is additive-only; new method strings are allowed, and consumers that
  switch on known methods are unaffected.
- One new field, `direction`, `omitempty`, set only on reverse events. Absence
  means inbound, so every event emitted today is byte-identical.
- Outcomes reuse `denied`, `ok`, `rate_limited`, `budget_exhausted`. No new
  outcome is needed, which is the point of them being a closed set.

Metrics arrive as **new series names**, not new labels on existing ones — the
label sets are frozen by the v1 contract, and tenancy already set the precedent
of paying for a new dimension with new names.

## 8. What stays out

**Elicitation content.** The obvious next ask is "refuse an elicitation that
asks for a password," and the obvious implementation reads `requestedSchema`
for field names and formats. That is one short step from reading the prompt
text, and the step after it is a scanner. Declined here. The reverse path
should instead become a **call site for the external decision hook**
([roadmap](roadmap.md) Horizon 2, item 9) — an operator who needs content-aware
elicitation review gets it out-of-process, latency-budgeted, with its own audit
outcome, like every other content question.

**Logging and progress notifications.** They are fire-and-forget, carry no
response, and have no decision to make — an allow/deny surface for them would
be ceremony. The real risk they carry is volume, and volume is the limiter's
job, so they are counted by the reverse limiter and governed no further.

**`roots`.** Not implemented, no position taken — unchanged by this.

## Where it lives

The check is in `bridgeOptions`, against the routing snapshot loaded per
request, so it reloads like everything else in the snapshot. The engine gains
one method — reverse decisions cannot reuse `Visible`, which is about list
items — and `serverInitiatedDecision` is a snapshot field, not a Gateway field.

Cost is one policy evaluation and one limiter check on a path that is already
making a network round trip to a model or a human. It is not the proxy hot
path, and the added-latency gate does not see it. The forward path gains
nothing.

## Compatibility

Additive throughout. One new config field with a behavior-preserving default,
one new optional audit field, new metric names, no new error code, no change to
any existing default. A deployment that upgrades and changes nothing behaves
exactly as it does today.

## Implementation phases

1. **Decide and refuse** — `bridgeFor(req, u)`, the policy check in both
   handlers, `serverInitiatedDecision`, `-32042` to the upstream, audit events
   with `direction`. Integration tests with a real SDK upstream that samples.
2. **The pair** — intersect declared capabilities with policy at session
   connect; test that a denied upstream sees no sampling capability *and* is
   refused if it asks anyway.
3. **Volume** — the reverse limiter and its metric, notifications counted.
4. **Docs** — security-model trust boundaries, the deploy checklist line that
   tells operators to set `serverInitiatedDecision: "deny"`, and the README
   pipeline section, which currently describes one direction.
