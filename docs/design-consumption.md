# Design: quotas, budgets, and consumption metering

Status: **implemented** across four phases. This recorded the design before
implementation and now serves as the decision record; the operator-facing
documentation lives in [configuration.md](configuration.md) and
[operations.md](operations.md). It
covers the [roadmap](roadmap.md)'s Horizon 1 headline — the one governance
pillar where fold had nothing.

Three things in the original proposal changed under implementation: the
`Budget` interface returns a struct rather than tuples, `responseBytes` was
dropped from metering, and the enforcement point moved. Each is recorded
inline where the original text stands.

## Motivation

Every limit fold enforces is a request count over a *sliding* minute that
resets rather than accumulates: `server.rateLimit`, its per-principal variant,
and `upstream.rateLimit` all resolve to `internal/ratelimit` or its Redis twin.
That is a good rate guard and a poor answer to the question an enterprise
actually asks, which is not "how fast" but "how much, this month, and by whom."

There are no quotas, no budgets, and no consumption record beyond Prometheus
counters and the per-request audit trail — neither of which carries a
consumption dimension. An operator asking "what has this team spent" has
nothing to read, and an operator asking "stop them at 10,000 calls" has nothing
to configure.

## What fold can and cannot govern

This is the load-bearing section, because the market's framing does not
transfer and copying it would produce a dishonest feature.

The market's answer to AI cost governance is token-based rate limiting: an LLM
gateway sits on the prompt/completion path, counts tokens, and throttles on
them. **fold is not on that path.** It proxies MCP traffic — tool calls,
resource reads, prompt fetches — between an agent and the servers it uses. The
model, its prompts, and its completions are somewhere else entirely.

So fold can govern three things, and must refuse the fourth:

1. **Volume over a period** — how many calls, to what, by whom, this
   hour/day/month. Fully observable, and the clearest gap.
2. **Payload size** — bytes served, tools returned per list. Observable, and
   the honest proxy for what a tool result costs once it lands in a model's
   context.
3. **Usage an upstream reports itself** — if a server publishes counters in
   `_meta`, fold can carry and record them.
4. **Token counts fold cannot see** — refused. Putting a tokenizer on the proxy
   path to estimate the cost of a payload that some *other* component will send
   to some *other* model would be a guess sold as a number, and it would put
   per-request tokenization behind the latency gate. If token budgets are the
   requirement, the component that talks to the model is the one that owns them.

Stating this plainly is the feature. An operator who understands that fold
governs MCP consumption, not model spend, can compose it with an AI gateway
that governs the other half. One who is sold "AI cost control" and discovers it
means "call counting" has been misled.

## Why a budget is not a rate limit with a longer window

The obvious implementation — give `Limiter` a period and pass a month — is
wrong, and this is the finding that shapes the work.

`internal/ratelimit` is a **two-bucket sliding window**: it weights the
previous window by overlap so the admitted rate stays smooth across boundaries.
That is exactly right for a rate guard, where a hard boundary would let a
caller spend a full window's budget twice in two adjacent instants.

It is exactly wrong for a budget. "10,000 calls this month" must be a fixed,
calendar-aligned counter that resets at a boundary an operator can predict,
explain to a customer, and reconcile against. A sliding month has no reset — a
caller who exhausted it is admitted again gradually, in proportion to how much
of the trailing month has elapsed. That is not a budget; it is a rate limit
with a very long window, and no one would recognize it as the thing they asked
for.

Budgets therefore need a new primitive alongside `Limiter`, not a parameter on
it. Nothing existing fits: `Store` is get/set with no atomic increment, `Once`
is a TTL set-if-absent, and `Limiter` is the sliding window above.

**`state.Budget`** — a fixed-window accumulating counter:

```go
// Budget admits consumption against a fixed, calendar-aligned allowance that
// resets at the period boundary.
type Budget interface {
    // Add records n units and reports whether they fit.
    Add(ctx context.Context, n int64) BudgetResult
    // Used reports the window's state without consuming any of it.
    Used(ctx context.Context) BudgetResult
}

type BudgetResult struct {
    Allowed     bool
    Used, Limit int64
    // Resets is when the window rolls over — not a retry delay, because the
    // answer to an exhausted monthly budget is "not until the 1st", and a
    // caller that backs off by this amount would sleep for a fortnight.
    Resets   time.Time
    Degraded bool // decided per-instance; shared state was unreachable
}
```

`Used` is not optional garnish. A budget that can only reject is a budget an
operator discovers at 100%; the whole value is knowing at 80%.

*(Implementation note: the first sketch returned bare tuples. It had nowhere to
put `Degraded`, which the fail-open decision below makes mandatory, so both
methods return one struct instead.)*

## What counts as one unit

The open question that most changes the feature, named rather than assumed.

A downstream request is not a uniform unit of consumption. One `tools/list`
fans out to *every* upstream in the federation (`fanOut` in `gateway/router.go`)
— in a 20-upstream federation that is twenty upstream invocations behind one
client request. Counting downstream requests would make the cheap call and the
expensive call cost the same.

Three candidate units:

| Unit | Reads as | Problem |
|---|---|---|
| Downstream requests | "10,000 calls to fold" | A list costs the same as a ping; hides the fan-out entirely |
| **Upstream invocations** | "10,000 calls to your servers" | Matches what is actually consumed; one client request can spend many |
| Bytes served | "10 GB of tool results" | Closest to context cost, hardest to explain and to set |

**Recommendation: upstream invocations**, because it is the thing whose cost is
real and the thing an upstream owner would recognize. Downstream requests
remain visible as a metric, so the fan-out ratio itself becomes observable —
which is a diagnostic worth having on its own. Bytes are metered (below) but
not budgeted in the first cut; budgeting them well needs operating experience
with the meter first.

## Failing open is a different decision for budgets

Every Redis operation in `internal/state` is bounded at 500 ms and **fails
open**: a shared-state outage degrades enforcement rather than availability.
For a rate limit that is plainly right — a blip should not take the gateway
down, and the blast radius is a few seconds of unthrottled traffic.

For a budget the calculus differs, because the failure is unbounded in a
direction that costs money: a Redis outage during which every instance fails
open is an outage during which the budget does not exist.

The proposal keeps **fail-open**, for consistency with every other shared
primitive and because the alternative — failing closed — converts a dependency
blip into a total refusal of service, which is a worse failure for a governance
component that sits in front of everything. But it must not be silent: a budget
check that could not reach shared state emits its own audit outcome and
increments a distinct metric, so "we were unbudgeted for nine minutes" is a
thing an operator can see and alert on rather than infer. That visibility is
the price of choosing availability here, and it is worth paying explicitly.

## Config

Additive under the v1 contract; every existing field keeps its meaning.

```jsonc
"server": {
  "rateLimit": { "requestsPerMinute": 600 },        // unchanged
  "budget": {                                        // new
    "period": "month",                               // hour | day | month
    "upstreamCalls": 100000
  }
},
"upstreams": [{
  "rateLimit": { "requestsPerMinute": 120 },        // unchanged
  "budget": { "period": "day", "upstreamCalls": 5000 }
}]
```

A per-principal budget is the one an enterprise actually wants, and it is the
one this cannot do well yet: budgets key on whatever identity is available, and
today that is a principal — one human or one agent — not a team. **The useful
dimension arrives with the tenant object** (roadmap Horizon 2), at which point
`tenants[].budget` is the natural home and the per-principal variant becomes
the special case rather than the shape. Shipping per-principal budgets first is
still worth it, but the sequencing is worth knowing: this feature gets better
when tenancy lands, and the config above is deliberately shaped so
`tenants[].budget` can join it without moving anything.

Defaults: **no budget unless configured**. A default budget would be a default
outage waiting for a busy month, and `docs/defaults.md` gets a row saying so.

## Metering

Enforcement is the smaller half. Metering is what an external system bills
from, and fold's job is to record faithfully and bill nothing.

Additive `audit.Event` fields — the contract permits adding, and audit is
already the single exit door every terminal response passes through:

- `upstreamCalls` — invocations behind this request (the fan-out, ≥ 1)
- `itemsServed` — items returned after policy filtering, on list methods
- `usage` — counters an upstream published in `_meta`, carried verbatim

*(Implementation note: `responseBytes` was proposed here and dropped. fold does
not serialize the result — the SDK does, downstream — so measuring it would
mean an extra `json.Marshal` per request purely to produce a number, trading
the allocation discipline the proxy path is gated on for a figure an operator
can already get from their ingress logs. `itemsServed` is the free half of the
same question, and it is the half that actually predicts context cost.)*

New metrics alongside the existing `fold_*` set: upstream invocations by
upstream, a response-size histogram, budget consumption as a fraction of
allowance, and the fail-open counter above.

**fold measures; something else bills.** No pricing, no invoices, no plans, no
currency anywhere in the gateway. The audit stream and `/metrics` are the
integration surface, and they are already the ones operators ship to a SIEM and
a TSDB.

## Status

All four phases have landed: the `state.Budget` primitive, config and snapshot
placement, enforcement with `-32044`, and metering. The per-tenant dimension
noted above still waits on the tenant object.

## Implementation phases

1. **`state.Budget`** — the interface plus both providers, memory and Redis,
   with calendar-aligned periods and an atomic increment-and-test. Redis
   fail-open emits the distinct signal above. Tests against miniredis, per repo
   rule 1, including a period rollover and a concurrent-instance race.
2. **Config and snapshot** — the fields above, JSON Schema in lockstep, and the
   resolved budgets living in the routing snapshot like every other reloadable
   value. Run the `/reloadable-state` checklist; budgets are per-upstream and
   per-server, so the snapshot placement is the whole question.
3. **Enforcement** — the check on the upstream-invocation path, its own audit
   outcome, and a minted error code. `-32040` is the *rate* limit; an exhausted
   budget is a different condition with a different remedy (wait for the reset,
   not retry shortly), so it earns **`-32044`** and an entry in the README's
   error-code registry.
4. **Metering** — audit fields and metrics, including the fan-out ratio.
5. **Docs** — `configuration.md` and the README's error-code section,
   `operations.md` for the
   new fields and metrics, `defaults.md` for the no-budget default, and a
   roadmap update.

## Explicitly out of scope

- **Token counting and estimation.** See above; the component talking to the
  model owns this.
- **Billing, invoicing, pricing, plans.** fold emits usage; a billing system
  consumes it. Putting money in the gateway would make every pricing change a
  gateway deploy.
- **Budget enforcement without shared state.** A single-instance budget is a
  per-instance budget, which for a fleet is not a budget. Configuring one
  without Redis warns at startup rather than pretending.
- **Cost attribution beyond what fold observes.** fold knows what it proxied.
  What that cost anyone in currency is a question for a system that knows
  prices.
