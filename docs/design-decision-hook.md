# Design: the external decision hook

Status: **proposed**. This records the design for the [roadmap](roadmap.md)'s
Horizon 2 decision hook — the one out-of-process seam fold intends to add —
and settles what it may see, what it may do, and what happens when it is slow
or wrong.

## Motivation

Three design records now defer to this feature by name. Server-initiated
governance sends content questions here ("refuse an elicitation that asks for
a password"). Definition pinning sends judgement here (it compares digests and
declines to read descriptions). The roadmap's content-inspection non-goal has
pointed here since v1.0. It is the escape hatch that lets fold keep saying no
to a scanner in the gateway.

It does a second job that matters as much. Every gateway fold is compared to
ships a plugin runtime, and fold [declines](roadmap.md#non-goals) one — on the
grounds that a plugin runtime is a second, less-inspectable configuration
language with arbitrary code behind the latency gate. Declining is only
credible if there is *one* seam where an organization can put its own
judgement. This is that seam, and the fact that it is a wire contract rather
than a code-loading mechanism is the whole argument: an HTTP endpoint can be
reviewed, versioned, rate-limited, and run by a different team than the one
running the gateway.

## What it is, and is not

The hook **decides**. It returns allow or deny, and fold does one of two
things with a request: forward it verbatim, or refuse it. That is the line
that keeps the [invisibility rule](../README.md) intact.

It emphatically **cannot rewrite**. No redacted arguments, no filtered
results, no injected headers. Response transformation is a
[non-goal](roadmap.md#non-goals), and a hook that could edit traffic would be
that non-goal with an extra hop: fold would be buffering and mutating bodies,
behavior through the gateway would stop matching the upstream, and the
conformance suite would be enforcing a fiction. A hook that wants content
changed refuses the call and tells the caller why.

It is also not a policy engine. Policy decides first and the hook never sees
what policy already refused — partly because that is cheaper, but mostly
because the hook is a supplement to deny-by-default and not a replacement for
it. An organization that turns the hook off must still be governed.

---

## 1. Where it sits

Two stages, named for what they inspect:

| Stage | Position in the pipeline | Sees |
|---|---|---|
| `ingress` | after policy, before the per-upstream guards and the proxy | the invocation and its arguments |
| `egress` | after the upstream answers, before audit | the result |

Placing ingress *after* policy is deliberate. A denied call costs no hook
round trip, the hook's operator is not handed traffic their organization has
already refused, and the hook cannot accidentally widen a grant — its allow is
necessary but never sufficient.

Placing egress *before* audit keeps audit the single exit door: a hook denial
is a terminal response like any other and leaves exactly one event.

Egress does not require new buffering. A tool result already arrives as one
JSON-RPC response, so the hook sees what fold was about to forward. Streamed
notifications (progress, logging) are not results and are not inspected —
holding those would change the timing an upstream chose, which is the
invisibility rule again.

## 2. The wire contract

`POST` to a configured URL, JSON in and JSON out, versioned from the first
release because this is a public interface the moment anyone writes a hook:

```jsonc
// request
{
  "version": "1",
  "stage": "ingress",                 // | "egress"
  "method": "tools/call",
  "name": "delete_repo",              // bare name, as the upstream knows it
  "upstream": "github",
  "principal": { "sub": "alice", "issuer": "https://corp.okta.com", "groups": ["eng"] },
  "tenant": "acme",
  "arguments": { "repo": "prod-infra" },   // ingress only, verbatim
  "result": { ... }                        // egress only, verbatim
}

// response
{ "decision": "deny", "reason": "matched DLP rule 12" }
```

`reason` is returned to the caller in the error message. That is a deliberate
disclosure: unlike a policy denial, where the expected value is the operator's
configuration, a hook denial is usually about the caller's own content, and a
refusal nobody can act on generates a support ticket instead of a fix. An
operator who disagrees returns an empty reason.

The envelope grows only additively, like every other fold wire surface. A hook
that ignores unknown fields keeps working.

## 3. What it is allowed to see, and the disclosure that is

The hook receives arguments and results verbatim. **That is a data-egress
decision an operator is making, and the documentation must say so in those
words** — the same traffic fold otherwise proxies to exactly one upstream is
now also sent to a second endpoint, and principal claims go with it.

Two controls, both defaulting to the *smaller* disclosure:

- `stages` selects `ingress`, `egress`, or both. Nothing runs unless named.
- `methods` selects which invocations are inspected. Absent means every named
  invocation, which is the useful default once someone has opted in at all.

fold does not offer a redaction knob for what it sends. A partial body is a
scanner's blind spot, and choosing what to strip is content judgement — the
thing being delegated. An organization that cannot send a body to its own hook
should not enable the stage.

## 4. Cost, and the gate

This is an HTTP round trip on the proxy path, which is the one place fold
measures. The honest framing: **the hook has a latency cost that no
implementation trick removes**, and the design's job is to bound it and make
it visible rather than to pretend otherwise.

- `timeoutMs` is required, with no default. A hook without a bound is a
  gateway without one.
- The added-latency gate keeps measuring the hook-free path, because that is
  what almost every deployment runs. A second benchmark reports the hook's
  cost against a local no-op endpoint, so the floor is documented rather than
  guessed.
- One connection pool, keep-alives on, no per-request TLS handshake. The
  client is dedicated, like the JWKS and discovery clients, with redirects
  refused: a hook that can redirect fold's decision request elsewhere is a
  hook that can be repointed by whoever answers it.
- No caching of decisions. The hook exists because the answer depends on
  content, so a cache keyed on anything cheaper than the content is a cache
  that returns the wrong answer.

## 5. When the hook fails

`onError` is **required** — `"allow"` or `"deny"`, no default. The roadmap
calls this a deliberate configuration choice, and the way to make a choice
deliberate is to refuse to start without it. Both readings are legitimate: a
compliance deployment wants traffic to stop when inspection stops, and an
availability-first deployment wants the gateway to keep serving. Guessing on
an operator's behalf would be wrong half the time, and wrong in a direction
they discover during an incident.

Timeouts, connection failures, non-2xx responses, and unparseable bodies all
take this path, and all count in `fold_hook_decisions_total{stage,outcome}`
with `outcome="error"` so a hook that is failing open is visible rather than
merely quiet. `FoldHookErrors` joins the packaged alerts.

A hook that is *slow* rather than broken is the more dangerous case, because
fail-open turns it into an invisible bypass. The timeout is therefore not
advisory: the request is abandoned at the bound and the configured failure
decision applies.

## 6. Audit

A hook denial gets its own outcome, `hook_denied`, rather than reusing
`denied`. An operator reading the trail needs to know whether their policy or
their inspector refused a call — the remedies are in different systems, and
often different teams. The event carries the stage and the hook's reason.

Hook errors that fail *open* are recorded on the event that proceeded
(`hookOutcome: "error"`), because "this call was allowed without inspection"
is exactly the fact a compliance review needs and exactly the fact a
fail-open deployment will otherwise lose.

## 7. Not in v1

- **Rewriting anything.** Section "What it is, and is not".
- **Multiple hooks / chains.** One seam, or it becomes the plugin hub through
  the back door. Fan-out belongs behind the operator's own endpoint, where it
  can be reviewed.
- **gRPC or a Go plugin interface.** HTTP+JSON is reviewable by people who do
  not write Go, and the embedding surface already exists for in-process users.
- **Inspecting notifications.** Progress and logging are not results; holding
  them changes timing the upstream chose.
- **Hooking list results.** Lists are already filtered per principal by
  policy, and a hook on the fan-out path multiplies its cost by the size of
  the federation. If someone needs it, it arrives as a third stage with its
  own measurement.

## Where it lives

Snapshot state, like everything reloadable: the hook's URL, stages, methods,
timeout, and failure decision live in the routing snapshot, so a reload swaps
them atomically and in-flight requests finish against the configuration they
started under. The HTTP client is construction-wired, alongside the other
outbound clients.

## Compatibility

Additive: one new optional config section, one new audit outcome, one new
metric family. Absent, nothing in the request path changes — the check is a
nil test against the snapshot, which is the same shape as every other opt-in
guard fold has.

## Implementation phases

1. **Ingress.** Config and validation (including the two required fields), the
   dedicated client, the wire contract, the decision, the audit outcome, the
   metric, and the benchmark that documents the cost. **Shipped**, with two
   scope decisions this record did not make.

   *`resources/read` is not inspectable yet, and says so.* Its probe fallback
   tries several upstreams for one URI, so inspecting it would multiply one
   decision by the size of the federation. Naming it in `methods` is a config
   error rather than a silent no-op — a method fold does not inspect is a hook
   an operator believes is running and is not. It joins with the egress stage,
   where a resource's content is what mattered anyway.

   *Opting in takes two acts.* Configuring an endpoint does not put it on the
   request path; a stage must be named. That is deliberate for a feature whose
   misconfiguration is either an outage or an unnoticed bypass.

   The floor, measured against a local no-op endpoint: **~42 µs per decision**,
   107 allocations. Real inspectors cost more, and the number worth publishing
   is the one fold controls. The added-latency gate is unmoved at 181 µs,
   because a deployment without a hook pays a nil check.
2. **Egress.** The result stage, and the honest accounting of what it costs on
   large results. **Shipped**, and the record was missing the thing that
   matters most about it.

   *By egress the upstream has already acted.* A denial there withholds the
   disclosure, not the effect — the row is deleted, the message is sent, and
   the caller is told the result was withheld. Egress is a data-loss control;
   stopping an action means refusing it at ingress. The error message says so
   in those words, because "denied" otherwise reads as "did not happen", and
   an organization deploying egress as a control on what its agents can *do*
   has misread the feature. This belonged in the design and was not there.

   *Oversize results are not truncated.* A partial body is precisely the blind
   spot an inspector must not be handed, so a result past 1 MiB takes the
   `onError` path — which under `"deny"` means refusing results nobody could
   have inspected. Fail-safe, and documented rather than discovered. The bound
   is a constant today; it becomes a field when someone needs it to be.
3. **The reverse path.** Sampling and elicitation as a hook stage, which is
   what [design-server-initiated.md](design-server-initiated.md) defers here —
   "refuse an elicitation that asks for a password" is the case it names.
4. **Docs.** README (the non-goal paragraph has promised this endpoint since
   v1.0 and should now point at it), security-model (the hook is a new trust
   boundary and a new data-egress path), deploy checklist, operations.

## The question this record does not settle

Whether the hook should be able to *demand* rather than merely decide — a
`"decision": "deny"` that also tells the caller how to comply is different in
kind from one that returns free text, and the difference matters for whether
an agent can retry successfully. That is a wire-contract question with a
compatibility cost, and it should be answered by someone with a real hook in
front of them rather than in advance.
