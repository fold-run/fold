# Roadmap

Direction, not dates. This document says what fold intends to build, in what
order, and — more usefully — what it does not intend to build and why. The
[changelog](../CHANGELOG.md) is the record of what shipped; this is the
record of what is being aimed at.

Everything here is bound by the v1 compatibility contract. Within v1, config
changes are **additive only** and every default is frozen ([defaults
review](defaults.md)), so each item below is scoped to add fields rather than
change meanings. Anything that would require breaking that contract is a v2
conversation, and there is no v2 planned.

The framing comes from a 2026-08 review of how the broader API-gateway market
pitches MCP governance. That pitch organizes into four pillars — security and
access control, cost and consumption, developer and agent experience, and
observability and automation — layered on a plugin runtime, declarative config
with drift detection, and a write-capable admin API. Reviewed honestly, fold
leads on security, has a total gap on cost, deliberately declines most of what
the market calls developer experience, and is a last mile short on
observability. That asymmetry is what this roadmap is organized around.

## Non-goals

The list of what fold declines is what makes the rest of this document
coherent, so it comes first. None of these are "not yet" — they are decisions.

- **LLM and model routing.** fold governs MCP traffic, not model traffic.
  Multi-provider proxying, prompt/completion transformation, and model
  failover belong to an AI gateway sitting at a different layer; an
  installation can run both, and they compose better as separate concerns than
  as one binary that does neither cleanly.
- **Inline content inspection** — DLP, PII filtering, prompt-injection
  detection. Restated from the README: inspecting bodies means buffering and
  rewriting traffic, which conflicts with the invisibility rule and the latency
  gate, and inline detection is a product of its own. The answer is the
  external decision hook (Horizon 2), not built-in scanning.
- **A write control plane.** No admin API, no create/edit/delete of upstreams,
  no policy editing in the console. Registration is discovery's job — pull-
  based, validated whole, auditable. A write path would be a second, competing
  registration path. The reasoning is on record in
  [design-console.md](design-console.md); the roadmap invests in richer
  *reads* instead.
- **Request and response transformation.** Templating, payload shaping,
  caller-facing header rewriting. Collides directly with the invisibility
  rule: behavior through fold matches hitting the upstream directly, and the
  conformance suite enforces it on every merge. (Attaching an upstream's own
  credential header on the way out is credential brokering, not
  transformation — the caller never sees it either way.)
- **Generating or hosting upstream MCP servers.** fold governs endpoints that
  already exist. Turning REST APIs into MCP servers, and running the resulting
  processes, is a different product.
- **A plugin runtime.** No embedded VM, no plugin SDK, no plugin hub. fold has
  one extension seam today — the Go embedding surface
  ([embedding.md](embedding.md)) for in-process users — and intends to add
  exactly one more: the out-of-process decision hook below. A plugin runtime is a
  second, less-inspectable configuration language, and it puts arbitrary code
  behind the latency gate.

## Where fold stands

| Pillar | fold today | What the roadmap adds | What fold declines |
|---|---|---|---|
| **Security and access control** | The strongest surface. Deny-by-default ABAC policy with the invisibility-plus-denial enforcement pair, first-class tenants (a visibility subset evaluated before policy, a shared bucket, an allowance, and the tenant in every record), five upstream credential strategies including RFC 8693 brokering, EMA, RFC 9728 metadata, host validation, and audit as the single exit door. | Depth, not parity: deny rules, argument-level constraints, destructive-operation gating, and the external decision hook. | Inline content inspection. Structural enforcement instead. |
| **Cost and consumption** | Accumulating calendar budgets at three scopes (server, upstream, tenant) alongside the sliding-window limits, metering of fan-out and items served, and per-tenant consumption series — so "what did this customer spend this month" is a query. (This row read "effectively nothing" before the Horizon 1 work below.) | The headline theme: accumulating quotas and budgets, consumption metering, and making the context-cost win fold already has legible. | Billing and monetization. fold measures; something else bills. |
| **Developer and agent experience** | A read-only console (dashboard plus an MCP test console that is a plain client against `/mcp`), and pull-based discovery for registration. | A searchable federated catalog and an effective-permissions view — both reads — and MCP Apps parity, which is a downgrade to remove rather than a feature to add. | The write registry, the self-serve storefront, and monetization. |
| **Observability and automation** | Good instrumentation, no interpretation: frozen Prometheus metric names, OTel server and client spans, an opt-in `ServiceMonitor` in the chart, and audit to stdout or a webhook. | Packaged dashboards, alert rules, and SLOs; audit sinks with retry and reach; CRDs and a config-diff CLI. | — |

## Horizon 1 — the next minors

Concrete, scoped, and additive. These close the cost gap and finish the last
mile on observability.

### 1. Quotas and budgets — **shipped**

Today's limiter is a sliding minute that resets. An enterprise asking "what can
this team spend this month" has nothing to configure. Add accumulating periods
— hour, day, month — as siblings of the existing per-minute fields at the three
scopes that govern MCP traffic (`server.rateLimit`, its per-principal variant,
and `upstream.rateLimit`), so today's config keeps meaning exactly what it
means. The fourth per-minute limiter, `auth.ema.tokenRateLimitPerMinute`, is
anti-amplification on an unauthenticated endpoint rather than a consumption
budget, and stays as it is.

Designed in [design-consumption.md](design-consumption.md), which settles a
question this entry got wrong. Budgets are **not** `state.Limiter` with a
longer period: that limiter is a two-bucket *sliding* window, and a sliding
month has no reset — an exhausted caller is readmitted gradually as the
trailing month elapses, which nobody would recognize as the budget they
configured. Budgets need a fixed, calendar-aligned accumulating counter, so
they arrive as a new `state.Budget` primitive alongside the limiter rather than
as a parameter on it. The record also settles what counts as one unit (upstream
invocations, since one `tools/list` fans out to every upstream) and why an
exhausted budget earns its own error code rather than reusing `-31040`.

### 2. Consumption metering — **shipped**

The market pillar this answers is "cost visibility," and fold's honest
position is that it is the wrong component to bill from but the right one to
measure from. Add metrics and additive `audit.Event` fields covering what a
gateway can actually observe: how many upstreams a request fanned to, list
payload bytes served, tool count served after policy filtering, and — where an
upstream reports usage in `_meta` — the pass-through counters it published.

fold reads what upstreams report and never synthesizes it. There is no
tokenizer in the gateway; counting tokens fold cannot see would be a guess
sold as a number, and it would put a tokenizer on the hot path. Designed
together with budgets in [design-consumption.md](design-consumption.md), which
draws the line the market's framing blurs: fold governs MCP consumption, not
model spend, and an installation that needs both runs both.

### 3. Tool-set shaping — **shipped**

The market frames oversized tool lists as "context bloat" and answers it with
semantic tool selection. fold already solved it structurally and did not
advertise it: per-principal policy filtering means a caller's `tools/list` is
the subset they are authorized for, computed at egress with no model in the
loop.

It is now legible and bounded. `fold_list_items_total{method,stage}` counts
what upstreams offered, what the caller was served, and what a cap removed —
the reduction as a ratio, which is the only form in which it means anything.
`policy.rules[].maxItems` bounds a rule's contribution to a list.

The cap is deliberately a bound rather than a curation. fold drops whatever
falls past it in merge order, because choosing *which* tools to keep would be
the semantic selection this roadmap declines, and pretending otherwise would
put a ranking model on the egress path. Truncation is therefore announced
three ways — result `_meta`, the audit event, and the metric — since a cap
that quietly removed capability would be worse than no cap at all. And it
bounds visibility only: a name withheld from a list is still callable if
policy allows it, because a list bound is not an authorization boundary and
must not become a second, weaker policy engine.

Nothing new landed on the hot path: the filter consults the decision the
per-principal filtering already made.

### 4. Audit reach — **shipped**

Audit is the single exit door, which makes it the integration point for every
SIEM — but it shipped as stdout or a webhook with no retry, so a receiver
blipping lost events.

Shipped: retry with backoff and equal jitter (on by default — the old
one-attempt behaviour was the defect), a dead-letter path for what delivery
finally gives up on, the `file` sink with size-based rotation, and
`fold_audit_events_total{sink,outcome}` so a gap in the trail is visible rather
than inferred. Jitter is not decoration: every instance in a fleet watches the
same receiver and fails at the same instant, so unjittered retries turn a
stumble into a synchronised stampede.

The `otlp-logs` sink followed, built on the OTel SDK's own pipeline rather
than on a hand-written encoder. That was a deliberate reversal: the first plan
was to emit OTLP JSON directly and avoid a v0.x dependency, which reads as
prudent until you notice where hand-rolled OTLP goes wrong — proto3 JSON
renders 64-bit fields as strings, severity is a numeric enum with a specified
text mapping, attribute values are tagged unions. Those are details the
exporter already gets right. fold keeps the accounting instead: the exporter is
wrapped so every export is counted and failed batches are dead-lettered, which
is what keeps this sink's failures as visible as the others'.

These are built-in types, not an extension point: `audit.Sink` stays
explicitly non-API under the compatibility contract. The `audit.sinks[].type`
enum widens in `config/config.go` with the JSON Schema kept in lockstep by
test, as usual.

### 5. Packaged observability — **shipped**

The metrics were good and their names are frozen by the v1 contract, which
makes dashboards safe to publish — so they are published: a Grafana dashboard
(16 panels across service level, latency, upstreams, governance, and ingress),
a `PrometheusRule` set of eight alerts, and documented SLOs, all in the chart
beside the existing `servicemonitor.yaml`, with
[operations.md](operations.md#dashboards-alerts-and-slos) explaining what they
mean. An operator used to get a `/metrics` endpoint, a template to scrape it
with, and interpretation nowhere.

Two things the entry did not say. The availability SLO counts only the
outcomes fold *failed* — `denied`, `rate_limited`, and `budget_exhausted` are
excluded, because an SLO that counts correct refusals pages someone for
tightening a policy and teaches the team to ignore the alert. And the pack is
tested against the code in both directions: every metric a panel or rule names
must exist, and every metric fold exports must appear somewhere in the pack.
A renamed metric fails the build instead of yielding a panel that draws
nothing and an alert that can never fire.

### 6. Publish the Helm chart to an OCI registry — **shipped**

The chart was complete — deployment, service, ingress, HPA, PDB, an opt-in
`ServiceMonitor`, a config-validating init container, and a `make helm-check`
gate — but installed only from a repo path. It now publishes to
`oci://ghcr.io/fold-run/charts/fold` on every release tag, which closed the
only literal `TODO` in the tree.

Two things the entry did not anticipate. The release job **gates** rather than
just packages: it lints and renders every `ci/` value set, and refuses to
publish a chart whose `appVersion` does not name the tag being released —
because a chart deploys the gateway image by default, so a mismatched pair
would ship an install that silently runs a different version than the release
it accompanied. And publishing is also reachable by `workflow_dispatch` for a
named tag: the chart versions independently of the gateway, so a chart-only
fix must not need a gateway release to reach the registry.

## Horizon 2 — themes

Directional. Each names the open design question rather than pretending it is
settled.

### 7. Reach: stdio and local upstreams — **shipped**

The largest adoption gap in fold, and it came from the ecosystem rather than
from the competitive review: config validation accepts only `http` and `https`
upstream URLs, so fold could not front an MCP server that runs as a local
process — which is most of them.

Closed by `fold-stdio`, a sidecar shim that runs one stdio server and exposes
it over streamable HTTP. The gateway did not change: a shimmed server is an
ordinary `http://` upstream, so credentials, health checks, load balancing,
policy, and audit all apply with no special case, and nothing in the frozen
config surface moved. Operator guide: [stdio.md](stdio.md). The reasoning, the
argument that settled the shape, and the two proposals that did not survive
implementation: [design-stdio.md](design-stdio.md).

### 8. Policy depth — **shipped**

The engine is allow-only and first-match, matching on an exact server id, exact
method names, and name globs. Three additions, in rough order of demand:

- **Deny rules with explicit precedence**, so a broad allow can be carved
  without inverting into an enumeration.
- **Argument-level constraints** matching on paths within `arguments` — the
  difference between "may call `deploy`" and "may call `deploy` against
  staging."
- **Destructive-operation gating** keyed on the MCP tool annotations
  (`readOnlyHint`, `destructiveHint`) that fold currently ignores, so a policy
  can express "read anything, write nothing" without naming every tool.

Argument matching reads the request fold already parses to route, so it stays
inside the invisibility rule — it decides, it does not rewrite.

All three shipped, in that order. What the design record did not anticipate,
and implementation had to decide: a `deny` carrying argument constraints
cannot be evaluated at list time either, and hiding there would apply the rule
far more broadly than written — "no deploys to production" would remove
`deploy` entirely — so allow and deny make *opposite* choices about the same
missing information, each in the direction that matches what the rule says.
And there is no separate annotation index for `toolKind`: at list time fold
already holds the tool, and at call time the cached list answers, so a second
index would have been a third thing to invalidate in step with the cache.

Designed in [design-policy-depth.md](design-policy-depth.md), which settles the
question each part raises rather than leaving it to implementation. Deny wins
globally rather than by document order, because a rule whose correctness
depends on where it was pasted will eventually be pasted in the wrong place —
with the no-deny short-circuit preserved, so documents that use none pay
nothing. Argument constraints cannot filter a list (there are no arguments at
list time), so they make a tool visible-but-conditionally-callable, which is a
real weakening of the invisibility pair and is written down rather than
discovered. And destructive gating reads annotations supplied by the very
upstream being gated, so it is documented as a hygiene control for federations
you operate — not a boundary against a hostile server, where naming the tools
remains the answer.

### 9. The external decision hook

The README has pre-committed the shape: an opt-in policy endpoint on the
ingress and egress path. Building it does two jobs at once. It is the escape
hatch for organizations where inline inspection is genuinely table stakes,
without putting a scanner inside the gateway. And it is fold's answer to the
plugin runtime — one out-of-process seam, with a wire contract that can be
reviewed, instead of arbitrary code in the request path.

Non-negotiables: off by default, latency-budgeted with a documented cost,
fail-open or fail-closed as a deliberate configuration choice, and its own
audit outcome so a hook denial is as visible as a policy denial.

Designed in [design-decision-hook.md](design-decision-hook.md), which draws
the line the feature lives or dies on: the hook **decides**, and cannot
rewrite. A hook that could edit traffic would be the transformation non-goal
with an extra hop — fold buffering and mutating bodies, behavior through the
gateway no longer matching the upstream, and the conformance suite enforcing a
fiction. A hook that wants content changed refuses the call and says why.

Three things the record settles rather than leaves to implementation. The
ingress stage sits *after* policy, so the hook's allow is necessary but never
sufficient and it never sees traffic the gateway has already refused. Both
`timeoutMs` and `onError` are required with no defaults, because "deliberate
configuration choice" means refusing to start without one — and because
guessing between fail-open and fail-closed would be wrong half the time, in a
direction the operator discovers during an incident. And sending arguments and
results to a second endpoint is a data-egress decision, which the
documentation states in those words rather than leaving an operator to notice.

### 10. Tenant as a first-class object — **shipped**

Tenancy used to be emergent — issuer-pinned policy rules, per-principal
limits, and a console group allowlist added up to isolation, but there was no
tenant object, and `Owner{org, team, contact}` is metadata on an upstream
rather than a boundary.

`tenants[]` binds verified claims to an upstream subset, a budget, a rate
limit, and a line in the audit record: the thing operators were assembling by
hand, in one declaration — and the dimension the budgets shipped in Horizon 1
actually wanted. Designed in [design-tenancy.md](design-tenancy.md), which
holds one line above all: a tenant groups principals, it does not authenticate
them. The design record also carries the two corrections implementation forced
on it, which is the more useful half to read.

Landed in phases: resolution (the config, its validation, and
the resolved tenant on the request context), and enforcement — a tenant's
budget charged where the server and per-upstream ones are, and one rate-limit
bucket shared by every principal in the tenant, which is what
`perPrincipalPerMinute` cannot express; and the visibility subset, which
bounds what a tenant sees by filtering the fan-out — an upstream outside it is
never asked — and refuses named invocations before the policy engine sees
them; and the record — the tenant in every audit event, two tenant-scoped
metric series (a label on the existing ones would have broken the frozen label
sets, so the dimension arrives as new names), and a console federation view
that is the viewer's rather than the operator's; and the docs. **Complete** —
what remains for tenancy is whatever operators ask for next, not a plan. The
open question the design named rather than assumed was
cardinality, and it is now **settled by measurement**: matching a principal
against N definitions was linear and reached 450 µs per request at ten
thousand tenants, so the single-dimension selector shapes are indexed at
snapshot time and resolve flat — 10,000 tenants cost the same as 10
([benchmarks.md](benchmarks.md#tenant-resolution-cardinality)). Compound
selectors still scan, which is documented rather than hidden.

### 11. Catalog reads

The counter-proposal to a write registry. A federated, searchable catalog of
tools, prompts, and resources with owner metadata attached, plus an
effective-permissions view answering "what would this principal see" without
minting a token for them.

Strictly read-only, and strictly through the same paths as any other client —
the console has no privileged access today and gains none here.

### 12. GitOps and drift

Config-as-code with a published JSON Schema is already fold's model, and
discovery is already a pull-based reconciliation loop. Two things are missing
next to what the market ships: a Kubernetes-native declaration and a way to
see drift.

Add `Upstream` and `Policy` CRDs with an operator producing the discovery
document fold already polls, and add two subcommands to a CLI that today has
`--validate` and stops there: `fold diff`, comparing a running gateway's config
against a file, and `fold lint`.

### 13. Governing server-initiated requests

Deny-by-default currently applies to one direction. Every client→upstream
invocation is authenticated, limited, tenanted, policy-checked, and audited;
the traffic bridged back the other way — `sampling/createMessage` and
`elicitation/create` — passes none of it. An upstream can spend the caller's
model budget in a loop, or put an arbitrary prompt in front of the caller's
human, and fold forwards both without an opinion or a record.

Closing it is depth in the direction fold already went alone: the reverse path
exists only because of per-client bridged sessions, so a gateway without
session bridging has nothing here to govern.

Designed in [design-server-initiated.md](design-server-initiated.md), which
settles the three questions it raises. The new decision defaults to **allow**,
because sampling flows today in every deny-by-default deployment and folding it
under the existing knob would break working installs on upgrade — the frozen
defaults are not negotiable, so the tightening is opt-in and the checklist is
where operators are told to take it. The enforcement pair has a reverse
analogue worth taking: an upstream that may not sample is never shown that the
client can, with the per-request handler check as the actual boundary because a
capability declared at initialize goes stale on reload. And budgets count
requests rather than tokens, since fold cannot see the model call the client
makes — the same line [design-consumption.md](design-consumption.md) drew for
the forward path.

Content questions — "refuse an elicitation that asks for a password" — stay out
and become a call site for the decision hook above.

### 14. Pinning upstream definitions

A tool definition is the instruction set a model acts on and the annotations a
policy decides with, and fold re-reads it from the upstream on every cache
refill without comparing it to anything. The approval that mattered — a human
reading a tool list and accepting the federation — is pinned to nothing, so a
tool can acquire new instructions after acceptance and be served with the same
namespace, grant, and trail as the one that was approved. This is the
ecosystem's most-published attack, and the answers on offer elsewhere are
scanners.

fold's is arithmetic: hash what an upstream advertises, keep the baseline in
`state.Store` so a fleet agrees and a rolling restart does not silently
re-pin, and report a difference. It never asks whether a description is
malicious, only whether it is the same one — which keeps inline content
inspection declined rather than smuggled in, and puts the whole cost on the
list-refill path where `json.Unmarshal` already lives.

Designed in [design-definition-pinning.md](design-definition-pinning.md),
which is explicit that trust-on-first-use cannot vouch for the first
definition, and which stops short of a full plan on purpose. Detection
(`warn`) has no open questions and is what the roadmap intends to ship.
Prevention (`block`) does: legitimate change and attack are the same bytes, so
blocking needs a way to adopt a new definition, and every candidate either
invents a write path into running state or hands operators a hand-copying
chore. That choice belongs to whoever needs prevention, not to the design.

### 15. MCP Apps — **shipped**

The [MCP Apps extension](https://modelcontextprotocol.io/extensions/apps/overview)
(SEP-1865) lets a tool carry a pointer to an interactive HTML interface that
the host sandboxes and renders in place of text. It is shipping in Claude,
Claude Desktop, VS Code Copilot, Microsoft 365 Copilot and Goose, and fold has
no awareness of it at all.

That is not a feature gap, it is an invisibility-rule violation, and it is
measured rather than theorised: an upstream gating on the client capability
logged `extensions=null` from `fold-gateway` and served its text-only fallback
to a host that had declared app support to fold one hop earlier. The same
upstream renders an app on a direct connection and returns plain text through
the gateway, with nothing in the trail saying why.

The routing half **shipped first**, because it was the defect a federation hits
today rather than insurance against one it might: the extension requires a
`ui://` URI to be unique only within a server and the published template ships
one with no server segment, so two upstreams built from it advertise the same
interface URI — and fold served a read of it from whichever upstream listed it
last, with the winner depending on request history. Those URIs are now minted
per namespace on every egress surface and resolved back on read, subscribe and
update, which is the sole documented exception to fold never rewriting a URI
and is argued as one in the design record.

The handshake half **shipped after it**. fold proxies the declaration rather
than configuring it: a named invocation rides a per-client session that now
declares what its client declared, and lists ride root sessions keyed by a
normalized capability profile — so a federation whose clients are all one kind
pays exactly what it paid before, a mixed one keeps two sessions per upstream,
and every client gets the list the upstream would have handed it directly. The
design record expected that to be the expensive half and it was not: root
sessions were already a keyed map, because a caller-derived credential has
needed one session per principal since it landed.

Designed in [design-mcp-apps.md](design-mcp-apps.md), which is as clear about
what fold should not do here. It adds **no config field**, opt-out included: an
operator who does not want an upstream's app variants is expressing a policy,
and a handshake setting that works by misreporting the client would be a
control with the wrong name. It mints fold-scoped `ui://` URIs but
declines to chase one embedded in a *tool result*, because following it there
is response-body rewriting. And it refuses to paper over app-initiated calls with a
bare-name fallback, which would weaken the namespace contract for every caller
and hand an app whatever tool owns that name across the whole federation. The
record also carries the design it replaced — a per-upstream flag plus
compensating egress filters — because the reason that shape was wrong is the
most useful thing in it.

The two gaps that remain after all of that are the specification's, and they
are in the gated horizon below.

## Horizon 3 — gated

Not scheduled. Each is blocked on something outside fold's control, or waiting
on demand that has not appeared.

| Item | Gate |
|---|---|
| SEP-2575 `subscriptions/listen` fan-in | The Go SDK serves the 2026-07-28 protocol only on stateless HTTP servers, which session-keyed bridging cannot use. The drift canary in `gateway/listen_test.go` fails when that lifts — the gap is instrumented, not merely noted. |
| Typed task API | The SDK does not yet model the task lifecycle, so `tasks/*` are forwarded as opaque JSON against fold's documented contract. Swaps to typed wire types when they ship; routing is unchanged. |
| MCP Apps: app-initiated calls and cross-server app isolation | The extension gives an app no way to learn the name its host knows a tool by, so an app that hardcodes one cannot call it through a namespace; and it defines no marker distinguishing an app-initiated `tools/call` from a model-initiated one, so the host's cross-server block has no analogue behind a gateway where every upstream shares one connection. Both need answers in the extension specification — every aggregator has the second hole and none can close it alone. Parity is item 15 above and is not gated on either. |
| `roots` | Not implemented, no position taken. Demand-gated. |
| mTLS and API-key inbound auth, RFC 7591 dynamic client registration | JWKS bearer is the only inbound credential today. Demand-gated. |
| `state.Provider` implementations beyond memory and Redis | The interface exists and is the right seam; nothing has asked for a third. |
| Discovery producers beyond Kubernetes | The document format is public and any producer works — a registry, a script writing to object storage. `fold-discovery` ships because Kubernetes was the common case, not because it is the only supported one. |

## How to read this

No dates, and ordering will change with what operators actually ask for.
Horizon 1 is scoped tightly enough to estimate; Horizon 2 is themes with the
design questions named; Horizon 3 is honest about what is waiting on someone
else.

What will not change is the set of things that veto an item regardless of
demand: the v1 compatibility contract and the frozen defaults, the invisibility
rule, the added-latency gate, and audit as the single exit door. A feature that
requires breaking one of those does not get built — it gets a non-goal entry
above, with the reasoning written down.
