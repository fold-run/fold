# Roadmap

Direction, not dates. This document says what fold intends to build, in what
order, and — more usefully — what it does not intend to build and why. The
[changelog](../README.md#changelog) is the record of what shipped; this is the
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
| **Security and access control** | The strongest surface. Deny-by-default ABAC policy with the invisibility-plus-denial enforcement pair, five upstream credential strategies including RFC 8693 brokering, EMA, RFC 9728 metadata, host validation, and audit as the single exit door. | Depth, not parity: deny rules, argument-level constraints, destructive-operation gating, and the external decision hook. | Inline content inspection. Structural enforcement instead. |
| **Cost and consumption** | Effectively nothing. Every limit is a request count over a sliding minute that resets rather than accumulates; there are no quotas, no budgets, and no notion of spend. The per-request audit trail and Prometheus counters are the only usage record, and neither carries a cost dimension. | The headline theme: accumulating quotas and budgets, consumption metering, and making the context-cost win fold already has legible. | Billing and monetization. fold measures; something else bills. |
| **Developer and agent experience** | A read-only console (dashboard plus an MCP test console that is a plain client against `/mcp`), and pull-based discovery for registration. | A searchable federated catalog and an effective-permissions view — both reads. | The write registry, the self-serve storefront, and monetization. |
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
exhausted budget earns its own error code rather than reusing `-32040`.

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

### 3. Tool-set shaping

The market frames oversized tool lists as "context bloat" and answers it with
semantic tool selection. fold already solves it structurally and does not
advertise it: per-principal policy filtering means a caller's `tools/list` is
already the subset they are authorized for, computed at egress with no model
in the loop.

The work is making that legible and bounded — a pre- and post-filter tool
count metric so operators can see the reduction, and an optional per-rule cap.
Nothing new lands on the hot path; the filtering already runs.

### 4. Audit reach

Audit is the single exit door, which makes it the integration point for every
SIEM — but it ships as stdout or a webhook with no retry, so a receiver
blipping loses events. Add retry with a dead-letter path, plus `file` (with
rotation) and `otlp-logs` sink types.

These are built-in types, not an extension point: `audit.Sink` stays
explicitly non-API under the compatibility contract. The `audit.sinks[].type`
enum widens in `config/config.go` with the JSON Schema kept in lockstep by
test, as usual.

### 5. Packaged observability

The metrics are good and their names are frozen by the v1 contract, which
makes dashboards safe to publish — so publish them. A Grafana dashboard, a
`PrometheusRule` set, and documented SLOs, shipped under `deploy/` and wired
into the chart next to the existing `servicemonitor.yaml`, with
[operations.md](operations.md) pointing at them. Today an operator gets a
`/metrics` endpoint, a template to scrape it with, and interpretation nowhere.

### 6. Publish the Helm chart to an OCI registry

The chart is complete — deployment, service, ingress, HPA, PDB,
an opt-in `ServiceMonitor`, a config-validating init container, and a `make
helm-check` gate — but installs only from a repo path. Publishing it closes the
only literal `TODO` in the tree.

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

### 8. Policy depth

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

Argument matching would read the request fold already parses to route, so it
would stay inside the invisibility rule — it decides, it does not rewrite.

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

### 10. Tenant as a first-class object

Tenancy today is emergent — issuer-pinned policy rules, per-principal limits,
and a console group allowlist add up to isolation, and
[security-model.md](security-model.md) explains how, but there is no tenant
object. `Owner{org, team, contact}` is metadata on an upstream, not a
boundary.

A `tenants[]` object binding verified claims to an upstream subset, a budget,
a rate limit, and a line in the audit record makes the thing operators already
assemble by hand into one declaration — and gives the budgets shipped in
Horizon 1 the dimension they actually want. Designed in
[design-tenancy.md](design-tenancy.md), which holds one line above all: a
tenant groups principals, it does not authenticate them.

Landing in phases. Shipped so far: resolution (the config, its validation, and
the resolved tenant on the request context), and enforcement — a tenant's
budget charged where the server and per-upstream ones are, and one rate-limit
bucket shared by every principal in the tenant, which is what
`perPrincipalPerMinute` cannot express. Remaining: the visibility subset
(`tenants[].upstreams`, accepted today but not yet a boundary — see README
"Not implemented"), the tenant as a metric label, and the docs pass. The open
question the design named rather than assumed was
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

## Horizon 3 — gated

Not scheduled. Each is blocked on something outside fold's control, or waiting
on demand that has not appeared.

| Item | Gate |
|---|---|
| SEP-2575 `subscriptions/listen` fan-in | The Go SDK serves the 2026-07-28 protocol only on stateless HTTP servers, which session-keyed bridging cannot use. The drift canary in `gateway/listen_test.go` fails when that lifts — the gap is instrumented, not merely noted. |
| Typed task API | The SDK does not yet model the task lifecycle, so `tasks/*` are forwarded as opaque JSON against fold's documented contract. Swaps to typed wire types when they ship; routing is unchanged. |
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
