# Design: policy depth

Status: **shipped** (`policy.rules[].deny`, `allow[].args`, `allow[].toolKind`
in `policy/policy.go`). Kept as the record of the three questions deny rules,
argument-level constraints, and destructive-operation gating each raise, and
how they were settled before being built — for the [roadmap](roadmap.md)'s
Horizon 2 item 8, which carries the two things implementation decided that
this record did not anticipate.

## Motivation

The engine is allow-only and first-match, matching an exact server id, exact
method names, and name globs. That is enough to express "engineering may call
these tools on that upstream", which is most of what operators ask for. Three
requests recur that it cannot express, each forcing the same workaround:
enumerate.

| What an operator wants | What they must write today |
|---|---|
| "everything on `billing` except `refund_*`" | every allowed name, forever, updated whenever the upstream adds a tool |
| "may `deploy`, but only to staging" | nothing — the grant is per name, and `deploy` is one name |
| "read anything, write nothing" | every read-only tool by name, re-derived per upstream |

The enumeration is not merely tedious. It fails *open* on change: a new tool
appears upstream, nobody updates the allowlist, and the gap is invisible until
someone notices the capability is missing — or, with a wildcard, until someone
notices it is not.

## What this is, and is not

Three additions to the existing engine, not a new one. Every current guarantee
holds: deny-by-default remains the posture, policy remains the thing that
decides invocations, list filtering plus call denial remains the enforcement
pair, and audit remains the single exit door.

It is emphatically **not** a policy *language*. No expressions, no CEL, no
embedded VM — that is the plugin runtime the roadmap
[declines](roadmap.md#non-goals), wearing a different hat. Each addition below
is a fixed matcher an operator can read at a glance, because a policy document
whose meaning requires evaluation in your head is a policy document people get
wrong.

---

## 1. Deny rules

```jsonc
{
  "id": "no-refunds",
  "subjects": { "groups": ["support"] },
  "deny": [ { "server": "billing", "names": ["refund_*", "credit_*"] } ]
}
```

`deny[]` is shaped exactly like `allow[]` and matches the same way. A rule may
carry either or both.

### Precedence: deny wins, globally

**Any matching deny refuses the invocation, regardless of rule order.** This is
the decision, and the alternative is worse in a specific way.

First-match precedence — scan rules top to bottom, first `allow` or `deny` that
matches decides — is the smaller change and reads naturally. It also makes
safety depend on document order, which means reordering a list, or appending a
broad allow at the bottom, silently widens access. That is the same failure
[design-tenancy.md](design-tenancy.md) refused when it declined "first matching
tenant wins": a rule whose correctness depends on where someone pasted it is a
rule that will eventually be pasted in the wrong place.

Deny-wins is also what operators arrive already knowing, from IAM and from
firewalls: an explicit deny is not overridable by an allow. Nobody has to learn
fold's variant.

The cost is that evaluation cannot stop at the first allow — every rule must be
checked for a matching deny. Mitigated exactly: the engine computes at
construction whether *any* rule carries a deny, and when none does, evaluation
is the current first-match short-circuit, unchanged. A document that uses no
deny rules pays nothing, which matters because that is every document that
exists today.

### What deny does not do

A deny rule does not deny *listing* separately from invoking. It participates
in the same decision, so a denied name is invisible and uncallable, together —
the enforcement pair stays one pair.

---

## 2. Argument constraints

```jsonc
{ "server": "deploy", "names": ["deploy"], "args": { "environment": "staging" } }
```

`args` is a map of dotted JSON path → required value, matched against the
request's `arguments` object. Every entry must match. Values compare exactly,
with the same type-exact rule the policy engine already applies to token claims
(`claimEqual`), and for the same reason: `"1"` and `1` are different, and a
lenient comparison silently widens the grant.

Paths are dotted (`target.environment`), with no wildcards and no array
indexing. If that turns out to be too little, it can widen later; a matcher
that starts expressive cannot be narrowed.

### The asymmetry this creates, stated up front

**A tool constrained by arguments is visible but conditionally callable.**
There are no arguments at list time — `tools/list` has no request to inspect —
so argument constraints cannot filter a list. The tool appears; a call with
non-matching arguments is denied.

This is a genuine weakening of the invisibility pair, and it should be written
down rather than discovered. The pair's guarantee becomes: *a caller never sees
a tool they cannot call at all*, rather than *never sees anything they might be
refused*. An operator who needs the stronger property still has it — grant by
name, not by argument.

The denial names the constraint path that failed, and **not** the value that
failed it: the value came from the caller, but the expected value is the
operator's configuration, and error messages are a fine place to leak a policy
document one field at a time.

### Cost, and where it lands

fold forwards `arguments` as raw JSON today and never parses it — that is part
of why the proxy path is allocation-light. Argument matching means unmarshalling
on the call path.

So it is conditional: the engine knows at construction whether any rule carries
`args`, and when none does, nothing is parsed and the call path is byte for byte
what it is now. When some rule does, the arguments are unmarshalled once per
call and only for calls that reach a rule carrying constraints. `make bench`
gates the result either way.

---

## 3. Destructive-operation gating

```jsonc
{ "server": "*", "methods": ["tools/call"], "toolKind": "readOnly" }
```

`toolKind` matches on the MCP tool annotations fold currently ignores:
`readOnly` requires `readOnlyHint: true`; `nonDestructive` additionally admits
tools whose `destructiveHint` is false. This is what lets a rule say "read
anything, write nothing" without naming every tool.

Two properties of the annotations decide the design, and both cut against the
obvious implementation.

### The annotations are supplied by the thing being gated

An upstream declares its own tools' hints. A server that labels
`delete_everything` as `readOnlyHint: true` is trusted by exactly this gate. So:

**`toolKind` is a hygiene control, not a security boundary.** It is for
federations of servers your organization operates, where the annotations are
honest and the risk is an operator forgetting to update an allowlist. Against
an upstream you do not control — a vendor's MCP server, anything arriving via
discovery — the boundary remains `names`, and the documentation must say so in
those words rather than implying the gate holds.

fold could add a trust flag (`upstreams[].trustToolAnnotations`) so gating only
applies where an operator vouches for the source. That is deferred, not
rejected: it is one field, and it can be added when someone wants the
distinction. Shipping it in v1 would suggest the untrusted case is handled,
when what actually handles it is naming the tools.

### The spec's defaults are fail-safe, and fold should not "improve" them

Per the MCP schema, `readOnlyHint` defaults to **false** and `destructiveHint`
defaults to **true**. An unannotated tool is therefore *not* read-only and *is*
destructive. A `toolKind: "readOnly"` rule denies every unannotated tool, which
is the correct direction and the opposite of what a "treat missing as harmless"
implementation would do. Take the defaults as written.

### Annotations are not available where the decision is made

At call time fold holds a name, not a tool. Annotations arrive with
`tools/list`, which fold caches per upstream — except where it deliberately does
not: an upstream whose credential is caller-derived (`passthrough`,
`token-exchange`) has list caching disabled, because one caller's list must
never serve another.

So for those upstreams a `toolKind` rule has no annotation to consult without a
per-call round trip, which is not acceptable on the invocation path.

**Unknown annotation denies.** If a rule gates on `toolKind` and fold cannot
establish the tool's annotations from the snapshot it holds, the invocation is
refused with the ordinary policy denial. Fail-safe in the direction the rest of
the engine already fails, and it makes the limitation visible the first time
someone tries it rather than silently permissive. The consequence is documented
plainly: `toolKind` and caller-derived credentials do not compose, and an
operator wanting both grants by name.

---

## Where it lives

The policy engine is built per routing snapshot and swaps atomically on reload;
nothing here changes that. All three additions are compiled at construction —
deny presence, argument-constraint presence, and the per-upstream annotation
index — so the per-request path reads precomputed state, in keeping with
"reloadable state is one atomic snapshot".

The annotation index is built from the same cached list the egress path already
decodes, keyed by upstream and bare name, and invalidated with it. No new
fetch, no new cache.

## Compatibility

Additive under the v1 contract. `deny`, `args`, and `toolKind` are new optional
fields; a document without them evaluates exactly as it does today, on the same
first-match short-circuit, with nothing parsed that is not parsed now. The
minted error-code registry is unchanged: every refusal here is `-31042`, because
every refusal here is policy denying an invocation, and inventing codes per
reason would make clients handle distinctions they cannot act on.

## Implementation phases

1. **Deny rules.** Config, the deny-wins evaluation with the no-deny
   short-circuit preserved, list filtering and invocation together, and a
   benchmark showing the unused case unchanged. **Shipped**, and the benchmark
   says what was promised: a deny-free document decides in 73.8 ns against
   72.7 ns before the change, zero allocations either way, while a document
   whose deny rule matches the caller pays 92.7 ns for the full scan. The
   schema needed `anyOf` to express "allow or deny, at least one", since
   `allow` stopped being required.
2. **Argument constraints.** Config, conditional parsing, call-path enforcement,
   the visible-but-conditional asymmetry documented in the README's policy
   section, and `make bench` on a document that uses them. **Shipped**, with
   two things this record did not settle.

   *A deny carrying `args` does not hide.* The asymmetry above was written for
   allow clauses; the deny case is the mirror and needed the opposite choice
   for the same missing information. Hiding on an unevaluable deny would apply
   the rule far more broadly than written — "no deploys to production" would
   remove `deploy` entirely — so an argument-carrying deny stays invisible at
   list time and refuses at call time.

   *Preserving the old path took keeping the old matcher.* Folding the
   arg-aware logic into the existing matcher behind a mode flag cost ~4% on
   the per-item decide path, which is measurable because policy is evaluated
   once per item of every list it filters. The two matchers are therefore
   separate, and a document with no `args` anywhere runs the original one:
   76.8 ns against a 75.2 ns baseline (the residual is the wider `Decision`
   struct), zero allocations, versus 94.3 ns for a document that constrains
   arguments and walks a dotted path.
3. **Destructive gating.** The annotation index on the snapshot, `toolKind`
   matching, unknown-denies, and the honest documentation of what the gate is
   and is not. **Shipped**, with one shape change: there is no separate
   annotation index. At list time fold is already holding the tool, so the
   annotations are read from the item itself; at call time they are resolved
   from the same cached list the egress path decodes, by a lookup that runs
   only when a rule actually gates on kind. A second index keyed by upstream
   and name would have been a third thing to invalidate in step with the
   cache, for a linear scan of an already-decoded slice on a path that is
   about to make a network call anyway.

   Unknown-denies covers more than the caller-derived case this record
   anticipated: a tool the upstream does not list cannot be judged either, and
   both answer the same way.
4. **Docs.** README policy section, `docs/security-model.md` (the enforcement
   pair's revised statement), `defaults.md`.

Each phase is independently useful and independently shippable, which is how
tenancy went and why it stayed reviewable.

## Explicitly out of scope

- **A policy expression language.** No CEL, no scripting, no arbitrary
  predicates. See the roadmap's plugin-runtime non-goal; a language is that
  non-goal with better marketing.
- **Regex name matching.** Globs are what the document uses today, they are
  readable at a glance, and regex on caller-influenced input is a denial-of-
  service surface fold does not need.
- **Response-shaped rules** — deciding on what an upstream returned. That is
  content inspection, which the roadmap declines, and it would mean buffering
  responses in a gateway whose invisibility rule forbids it.
- **Semantic or model-driven tool selection.** Restated from item 3: fold
  bounds and filters; it does not rank.
