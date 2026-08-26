---
name: observability
description: Add, rename, or retire a fold metric, span attribute, or audit event field — the bidirectional lockstep against the packaged dashboard and both alert files, the v1 freeze on names and label sets, and the docs that track them. Use when touching gateway/metrics.go, otel.go, audit/, the Grafana dashboard, or either alert file.
---

# Changing what fold reports about itself

Three surfaces, one rule each, and only one of them has a test.

| Surface | Declared in | Held in lockstep by |
| --- | --- | --- |
| Prometheus metrics | `gateway/metrics.go` | `gateway/observability_pack_test.go`, both directions |
| Trace attributes | `gateway/otel.go` | nothing — `docs/operations.md` "Tracing" is the record |
| Audit event fields | `audit/audit.go` | nothing — `docs/operations.md` "Audit events" is the record |

## Metrics: the pack is checked both ways

`observability_pack_test.go` compares the names in `metrics.go` against the
three files that query them:

- `deploy/helm/fold/dashboards/fold-overview.json`
- `deploy/helm/fold/templates/prometheusrule.yaml` (prometheus-operator)
- `deploy/observability/alerts.yml` (plain Prometheus, the compose stack)

**Forward:** every `fold_*` the pack references must be declared. A query
naming a metric that does not exist draws an empty panel and arms an alert
that can never fire — the two failure modes that look exactly like health.

**Backward:** every declared metric must appear somewhere in the pack, so a
new one forces the question "does this belong on the dashboard" instead of
shipping unobserved. `metricNamesUnexercised` is the escape hatch and is
currently empty; adding to it should cost a reason in the diff.

And `TestBothRuleFilesCarryTheSameAlerts` — the two rule files are the same
alerts for two deployment shapes. Change one, change the other.

### Two traps in how the check reads the code

- It extracts **quoted string literals** matching `"fold_[a-z_]+"` from
  `metrics.go` only. A name built by concatenation, or declared in another
  file, is invisible to the test — it will pass while shipping a metric
  nothing watches.
- Histogram series are matched after stripping `_bucket` / `_sum` /
  `_count`, so query `fold_request_duration_seconds_bucket` freely; the
  base name is what must be declared.

It reads the source rather than scraping a live registry on purpose: a
`CounterVec` with no observations exposes nothing, so a scrape would
under-report exactly the metrics that have never fired.

## The v1 freeze is on names *and* label sets

README "API stability" freezes both. That is what makes publishing these
queries safe, and it has a consequence that catches people:

**Adding a label to an existing metric is a breaking change** — it breaks
every dashboard and recording rule built on it. New metric *names* are
additive and permitted. This is why tenant series are
`fold_tenant_requests_total` and `fold_tenant_upstream_calls_total` rather
than a `tenant` label on `fold_requests_total` (see the comment in
`metrics.go` and `docs/design-tenancy.md`, "the record").

Renaming or removing a metric is a v1 break and needs its own conversation.

**Cardinality is the operator's own choice or it does not ship.** The
`upstream` and `tenant` labels are bounded by the config document. Nothing
may be labelled by a value the gateway does not choose — principal, tool
name, task id, session id. Per-principal state belongs in
`internal/bounded`, not in a label.

## The degradation metrics have a house pattern

`fold_state_degraded_total`, `fold_budget_degraded_total`,
`fold_panics_total` — fail open, loudly, and alert on non-zero. When you
add a path that degrades rather than refuses, it gets a counter in this
family and an alert in both rule files. A fleet quietly enforcing N copies
of one limit is the thing an operator cannot otherwise see.

## Checklist

- [ ] Name declared as a quoted literal in `gateway/metrics.go`
- [ ] `Help` text says what it counts *and what it excludes* — the existing
      ones do, e.g. `fold_tenant_requests_total` points at
      `fold_requests_total` for the rest
- [ ] Referenced from the dashboard, or from both rule files, or justified
      in `metricNamesUnexercised`
- [ ] If it earns an alert: added to **both** `prometheusrule.yaml` and
      `deploy/observability/alerts.yml`
- [ ] No unbounded label; no new label on an existing metric
- [ ] `docs/operations.md` — the "Metrics" table, and "The SLOs" /
      "What the alerts assume" if an alert changed
- [ ] Chart bump if `deploy/helm/` changed (`/preflight`, and Chart.yaml's
      `version` is an immutable OCI tag)

```bash
go test ./gateway -run 'TestPackReferencesOnlyDeclaredMetrics|TestEveryMetricAppearsInThePack|TestDashboardIsWellFormed|TestBothRuleFilesCarryTheSameAlerts'
make helm-check          # the pack ships in the chart
```

## Spans and audit fields

Neither has a test. Both are contracts anyway.

**Spans** (`gateway/otel.go`): attributes follow OpenTelemetry semantic
conventions where one exists — `enduser.id` for the principal, not
`fold.principal` — and fold's own namespace otherwise (`fold.upstream`,
`fold.outcome`, `fold.policy.decision`, `fold.policy.rule`). A new
attribute goes in `docs/operations.md` "Tracing". Never put a credential,
a token, or argument content on a span.

**Audit fields** (`audit/audit.go`): the event is the compliance record and
its JSON shape is consumed by sinks fold does not control, so a field is
added — never renamed, never repurposed — with `omitempty`, and documented
under `docs/operations.md` "Audit events". Audit is the single exit door:
if a change creates a new terminal outcome, it emits exactly one event,
denials included.
