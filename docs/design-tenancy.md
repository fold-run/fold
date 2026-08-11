# Design: the tenant object

Status: **implemented** — all six phases. Resolution, the cardinality question
settled by measurement, enforcement of the tenant's budget and rate limit, the
visibility subset, the record, and the docs. This
records the design for the [roadmap](roadmap.md)'s Horizon 2 tenancy item, and
settles a question three shipped features have now deferred: what a tenant *is*
in fold.

## Motivation

Tenancy in fold today is emergent. An operator who wants "team A sees these
tools, gets this allowance, and appears separately in audit" assembles it from
four unrelated mechanisms:

| What they want | What they configure today |
|---|---|
| Which tools a team sees | `policy.rules[].subjects.groups` and `allow[]` |
| A team's blast radius | `server.rateLimit.perPrincipalPerMinute` |
| A team's allowance | `upstreams[].budget` — but keyed per upstream, not per team |
| Who may view the console | `server.console.groups` |
| Which team owns an upstream | `upstreams[].owner` — metadata, not a boundary |

Each is correct on its own. Together they mean the word "tenant" appears
nowhere in the config document, the audit stream, or the metrics, so the
question "what did team A consume this month" has no answer fold can give —
only "what did principal `sub-8f3a` consume", which is one person or one agent.

[design-consumption.md](design-consumption.md) named this explicitly: budgets
key on identity, and the useful dimension is a team rather than a principal.
That is the gap this closes.

## What a tenant is, and is not

**A tenant is a named set of principals, plus the governance that applies to
them as a group.** It is a *label on identity*, resolved at authentication
time from claims the IdP already asserts.

It is emphatically **not** a new authentication mechanism, a new trust anchor,
or a second authorization engine. Every existing guarantee holds unchanged:

- The verified `auth.Principal` remains the only identity fold trusts, and it
  still comes from the IdP's signature. A tenant is derived from it, never
  presented alongside it — a caller cannot assert a tenant any more than they
  can assert a group.
- Policy remains deny-by-default and remains the thing that decides
  invocations. Tenancy does not gain a parallel allow path.
- Audit remains the single exit door.

The one-sentence version, and the line this design exists to hold: **a tenant
groups principals; it does not authenticate them.**

## Resolution

A tenant is matched from the same verified claim material policy already
matches on, using the same `subjects` shape — reusing it rather than inventing
a second matcher, so there is one way to say "these callers" in the document:

```jsonc
"tenants": [
  {
    "id": "acme",
    "subjects": { "claims": { "org_id": "acme-prod" } },
    "budget": { "period": "month", "upstreamCalls": 500000 },
    "rateLimit": { "requestsPerMinute": 2000 },
    "upstreams": ["billing", "crm"]        // optional: visibility subset
  }
]
```

**A principal resolves to at most one tenant.** Overlap is a config error
rather than something resolved by precedence: "the first matching tenant wins"
is the kind of rule that silently sends a customer another customer's
allowance when someone reorders a list. Resolution happens once per request,
alongside principal extraction, and the result rides the context like the
principal does.

*(Implementation note: this section first said overlap was "rejected at
validation". It cannot be, and the correction matters. Deciding whether two
selectors overlap means deciding whether **some** principal could satisfy
both — and for group selectors that is nearly always true, since a principal
may hold groups from several lists at once. Rejecting statically on that would
refuse almost every real document. So validation catches what is genuinely
checkable — duplicate ids, byte-identical selectors, references to upstreams
that do not exist — and genuine ambiguity is caught at resolution, against a
real principal, where it is decidable. A principal matching two tenants is
**refused**, not assigned: serving it would mean picking, and picking is the
failure this design exists to prevent. It is an internal error rather than a
minted code, because a client cannot act on it — the operator's configuration
is what is wrong.)*

**An unmatched principal has no tenant.** It is governed exactly as today —
global limits, policy, per-upstream budgets. Tenancy is additive, so an
existing deployment behaves identically until it declares a tenant.

## What a tenant carries

Four things, each replacing an assembly rather than adding a new concept:

- **`budget`** — the dimension consumption governance actually wants. Charged
  in the same place the server and per-upstream budgets are, and subject to
  the same "only invocations that reach the upstream" rule.
- **`rateLimit`** — a per-tenant bucket, where today `perPrincipalPerMinute`
  gives one bucket per *person*. Ten agents on one team currently get ten
  allowances; this makes them share one, which is what "team A cannot flood
  team B" actually means.
- **`upstreams`** — an optional visibility subset, evaluated *before* policy.
  Policy stays the authority on what may be invoked; this bounds what the
  tenant can see at all, which is the coarse cut operators reach for first and
  currently express by enumerating rules.
- **Identity in the record** — `tenant` in every audit event and as a metric
  label, which is what makes "what did team A consume" answerable.

  *(Implementation note: "as a metric label" could not be done as written, and
  the correction is the same shape as the overlap one above. The v1
  compatibility contract freezes metric names **and label sets** (README, "API
  stability"), so adding a `tenant` label to `fold_requests_total` would break
  every dashboard and recording rule built on it — the contract is one of the
  things this roadmap says vetoes a feature regardless of demand. New metric
  names are additive, which the contract permits, so the tenant dimension
  ships as its own series: `fold_tenant_requests_total{tenant,outcome}` and
  `fold_tenant_upstream_calls_total{tenant}`, the latter counting the same
  unit a tenant budget is charged in. Untenanted traffic records nothing
  rather than a `tenant=""` series, and a test asserts the frozen metrics
  never grow the label.)*

Deliberately **not** carried: credentials, issuers, or any auth configuration.
That would make a tenant a trust anchor, which is the thing the previous
section refuses.

## Where it lives

Reloadable, in the routing snapshot. Tenants are the kind of thing that
changes when a customer signs up, and requiring a restart for that would push
operators toward a write API — the control plane fold has repeatedly declined
([design-console.md](design-console.md)).

That makes `tenants` an exception worth stating: `server.rateLimit` and
`server.budget` are construction-wired, but a *tenant's* limits reload. The
distinction is deliberate — the server-wide allowance is the operator's own
ceiling and should not move under a running gateway, while a tenant's is
customer-facing configuration that changes on a business cadence.

## The cardinality problem — settled

The one open question this design named rather than assumed. **Answer: (1),
index the common case** — measured, not argued, and the measurement is in
[benchmarks.md](benchmarks.md#tenant-resolution-cardinality). What follows is
the original framing, then what the numbers said.

Per-tenant limiters and budgets are keyed by tenant id, and tenant ids come
from config, so they are bounded by the document — unlike `principalLimits`,
which is keyed by identifiers the gateway does not choose and therefore needs
`internal/bounded`. That much is easy.

What is not obvious is **how many tenants a document can hold before
resolution costs something**. Matching a principal against N tenant
definitions is linear, and it happens on every authenticated request. For ten
tenants that is free; for ten thousand — a plausible number for a SaaS
fronting its customers — it is per-request work behind the latency gate.

Two candidate answers, to be settled by measurement before implementation:

1. **Index the common case.** Most tenant definitions will match on a single
   claim equalling a single value, which is a map lookup rather than a scan.
   Build that index at snapshot time and fall back to a scan only for
   definitions that need it.
2. **Bound the tenant count** and say so, treating "thousands of tenants"
   as a case for a control plane fold does not have.

(1) is preferable if the index is simple; (2) is honest if it is not. This
gets benchmarked at the same federation sizes `BenchmarkFederatedListTools`
uses, and the answer goes in [benchmarks.md](benchmarks.md).

### What the measurement said

`BenchmarkResolveTenant` measured the scan at 10, 100, 1,000, and 10,000
declarations. The scan cost ~42 ns per declaration, which is 450 µs for ten
thousand single-claim tenants — on every authenticated request, against a
gateway whose whole added p50 is ~200 µs. The prediction was right about where
it stops being free, and the number was bad enough that (2) would have meant
telling a SaaS with ten thousand customers to run something else.

The index is simple, so (1) it is — and it covers one shape more than
candidate (1) named. Groups look unindexable, since a principal holds many of
them and any may match; the resolution is that the two indexes are keyed from
opposite sides. The claim index is keyed by *what the tenant requires* and
probed with the principal's value; the group index is keyed by what the
principal *holds* and probed with each of their groups. Both are map lookups,
only the direction differs. So the set holds two indexes plus a scan list for
compound selectors, and resolution is flat in the number of tenants for every
shape a large document actually repeats.

Two properties keep this from being a second matcher, which is the risk an
index of a policy selector runs:

- **The index narrows; policy decides.** Every candidate a lookup produces is
  still put to `policy.MatchSubjects`, unchanged. The index cannot admit a
  principal the matcher would reject; the only bug it could introduce is a
  missed candidate, and `TestIndexedResolutionAgreesWithFullScan` checks
  resolution against a brute-force scan over a generated cross-product of
  principals.
- **Only exactly-representable keys are indexed.** A claim selector is indexed
  only when its required value is a JSON scalar, because map-key equality has
  to be the same equality the matcher uses — pointers compare by address in a
  map and by pointee in `reflect.DeepEqual`, and a wrong key is a missed
  match. Everything else falls to the scan.

**What stays linear, stated plainly:** compound selectors — issuer *and*
claims, or groups *and* claims — are matched one by one, at ~38 ns each. A
document holding thousands of *those* pays the original cost, and the guidance
is (2) for that residue: keep compound selectors in the tens, and express
per-customer tenancy as one claim or one group. That is what an IdP asserts
for a customer anyway.

## Migration

Additive under the v1 contract. `perPrincipalPerMinute` keeps working and
keeps meaning per-*principal* — it is not redefined, because silently changing
what an existing limit counts would be a contract break wearing a feature's
clothes. Operators who want per-team buckets declare tenants; those who want
per-person buckets keep what they have; those who want both get both.

## Implementation phases

1. **Resolution** — `tenants` config, validation including the overlap
   rejection, snapshot placement, and the resolved tenant on the request
   context. No enforcement yet. Run the `/reloadable-state` checklist.
   **Shipped.**
2. **The cardinality benchmark** — settle the open question above before
   anything depends on the answer. **Shipped**: `BenchmarkResolveTenant`, and
   the index it justified.
3. **Enforcement** — per-tenant budget and rate limit, reusing the existing
   primitives and charge points. **Shipped.** The budget is charged where the
   server and per-upstream ones are, narrowest allowance first (upstream →
   tenant → server), so a refusal never spends a wider allowance and only
   invocations that reach an upstream are counted. The bucket is enforced
   with its siblings in the HTTP layer — a 429 with `Retry-After`, widest
   bucket first (global → tenant → per-principal) — because a flood should
   be refused before it costs any routing work.

   One thing this phase had to settle that the design did not name: a tenant
   is rebuilt on every reload, including reloads with nothing to do with
   tenancy, and under the in-memory provider the counter *is* the object. So
   a tenant's budget and bucket are carried across a reload whenever that
   dimension's configuration is unchanged — otherwise adding an upstream
   would hand every customer a fresh month, and reloads are meant to be
   routine. Changing an allowance does start a new counter, which is what an
   upstream's budget already does.
4. **Visibility** — the `upstreams` subset, evaluated before policy.
   **Shipped**, and one word of the plan turned out to matter: the subset
   filters the *fan-out*, not the merged result. An upstream the tenant
   cannot see is never asked, so it costs no request, no budget, and no
   partial-failure entry when it happens to be down — and the property is
   tested by counting hits at the fixture rather than by reading the
   response. The same cut applies wherever a request selects an upstream:
   the four list methods, named invocations (before the policy engine, so
   nothing outside the subset ever reaches a rule), `resources/read` on both
   the affinity and probe paths, `resources/subscribe`, `completion/complete`,
   `logging/setLevel`, and the task methods.

   Each surface keeps the refusal posture it already had, which matters more
   than a single uniform answer. Tools, prompts, completion, and resources
   answer `-32042` — the subset is a coarser cut of the same decision, so it
   reuses the policy code rather than minting a fifth. Tasks answer
   "no upstream owns that id", exactly as they already do for another
   principal's task, because on that surface the refusal must not reveal
   existence. The URI-ownership index is shared across principals, so both
   resource paths check the subset before using it: a URI another tenant
   listed must not resolve by affinity.
5. **The record** — `tenant` in audit events and as a metric label, plus the
   console's federation view. **Shipped**, with the metric-label correction
   recorded above: the dimension arrives as two new series rather than as a
   label on frozen ones. The audit field landed with resolution in phase 1.
   The console's federation view is now the *viewer's* — a tenant with a
   subset sees its own upstreams, counts included, and its own limits, which
   also closes the gap where a dashboard would have shown a customer the
   topology its own traffic is refused.
6. **Docs** — README config section, `operations.md`, `defaults.md`,
   `security-model.md` (rewriting "Tenant isolation under load", which
   currently describes the emergent version). **Shipped.** The
   security-model section is now "Tenant isolation" and describes the object
   rather than the assembly; `operations.md` gains the queries that answer
   "what did this customer consume" and "is this customer being refused";
   `defaults.md` records why there is no default tenant, why an absent
   subset means every upstream, and why an absent budget means unlimited;
   and the example config carries a tenant, so the shape is in the file
   operators copy from.

## Explicitly out of scope

- **Tenant-scoped credentials or issuers.** A tenant groups principals; it
  does not authenticate them.
- **Per-tenant upstreams that only that tenant can reach.** The `upstreams`
  subset is a visibility filter over the shared federation, not a private
  federation per tenant. Genuinely isolated upstream sets are separate
  gateway deployments, which is cheaper and more auditable than a
  multi-tenant router pretending they are isolated.
- **Tenant self-service.** No API for a tenant to change its own limits; that
  is the write control plane, still declined.
- **Billing.** Same position as [design-consumption.md](design-consumption.md):
  fold emits the record, something else prices it.
