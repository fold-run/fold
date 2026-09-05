# fold Helm chart

Deploys [fold](https://github.com/fold-run/fold), the enterprise MCP gateway,
from `ghcr.io/fold-run/fold`. See [docs/deploy.md](../../../docs/deploy.md)
for the full deployment guide (allowedHosts, probes, TLS, Redis, secrets).

## Install

From the OCI registry (published with every release):

```bash
helm install fold oci://ghcr.io/fold-run/charts/fold \
  -n fold --create-namespace -f my-values.yaml
```

The chart versions independently of the gateway: `--version 0.1.3` pins the
chart, while the gateway build it deploys is the chart's `appVersion` (or
`image.tag`, if you set one). `helm show chart oci://ghcr.io/fold-run/charts/fold`
prints both.

From a repo checkout, unchanged:

```bash
helm install fold deploy/helm/fold -n fold --create-namespace -f my-values.yaml
```

Minimal `my-values.yaml`:

```yaml
config:
  upstreams:
    - id: github-tools
      url: https://mcp.platform.acme.com/mcp
      namespace: gh
  server:
    allowedHosts: ["gw.example.com"]
```

The chart refuses to render without a config: set either `config` (inline,
becomes a ConfigMap) or `existingConfigMap` (your own ConfigMap with the
document under the key `fold.config.json` — then `probes.hostHeader` is also
required).

## Observability

```yaml
metrics:
  serviceMonitor: { enabled: true, labels: { release: prometheus } }
  prometheusRule: { enabled: true, labels: { release: prometheus } }
  dashboard:      { enabled: true }
```

The first two need the prometheus-operator CRDs (the chart fails the render
with a clear message rather than installing something inert if they are
missing). `dashboard` renders a ConfigMap labelled `grafana_dashboard: "1"`
for the Grafana sidecar; without the sidecar, import
[`dashboards/fold-overview.json`](dashboards/fold-overview.json) directly.
Thresholds for the alerts live under `metrics.prometheusRule` — the latency
one has no universal value, since the measurement includes your upstreams'
own time. See [docs/operations.md](../../../docs/operations.md#dashboards-alerts-and-slos).

## Secrets

fold's config document never holds secret material; auth strategies name
environment variables via `secretRef` fields. Create a Kubernetes Secret with
those variables and point `envFrom` at it:

```yaml
envFrom:
  - secretRef:
      name: fold-upstream-secrets   # keys: GH_TOOLS_API_KEY, FOLD_OKTA_SECRET, ...
```

## Values

The important knobs (see `values.yaml` for the full set with comments):

| Key | Default | Notes |
|---|---|---|
| `config` | `{}` | Inline fold config document; rendered to a ConfigMap. Changing it rolls the Deployment (checksum annotation). |
| `existingConfigMap` | `""` | Alternative to `config`. Requires `probes.hostHeader`. |
| `envFrom` / `env` | `[]` | Inject `secretRef` env vars from Secrets. |
| `redis.url` / `redis.existingSecret` | unset | `REDIS_URL` for fleet-shared state. Only needed with >1 replica; fails open. |
| `probes.hostHeader` | derived | Host header for httpGet probes; must pass the gateway's allowedHosts check. Auto-derived from inline `config.server.allowedHosts`, else `localhost`. |
| `validateInitContainer.enabled` | `true` | Runs `fold --validate` before the gateway starts. |
| `replicaCount` | `2` | Ignored when `autoscaling.enabled`. |
| `strategy` | `maxUnavailable: 0, maxSurge: 1` | A serving pod is never removed before its replacement is Ready. `revisionHistoryLimit` (5) keeps ReplicaSets for `helm rollback`. |
| `lifecycle.preStopSleepSeconds` | `5` | Sleep between endpoint removal and SIGTERM so kube-proxy and the ingress stop routing first. Kubernetes 1.30+ `sleep` action (distroless has no shell); omitted on older clusters. |
| `server.drainTimeout` | `""` (fold default 10 s) | fold's `--drain-timeout`. Requires fold ≥ v1.16.0 when set. `terminationGracePeriodSeconds` must exceed preStop + drain or the render fails. |
| `priorityClassName` | `""` | Evict the gateway last. |
| `probes.readiness.mode` / `.path` | `http` / `/health` | `tcp` keeps pods in rotation on process liveness alone during a total upstream outage so clients get fold's `-31041` instead of an ingress 503 — see docs/deploy.md. |
| `extraVolumes` / `extraVolumeMounts` | `[]` | Private CA bundle (+ `SSL_CERT_FILE` in `env`), or a writable path for the `file` audit sink / `deadLetterPath` under the read-only root. |
| `autoscaling.behavior` | slow scale-down | HPA `behavior`; default removes at most one pod a minute after five minutes of low load, because scale-down cuts live SSE sessions. |
| `metrics.listener.externalConfigSetsMetricsAddr` | `false` | Required confirmation to use the metrics listener with `existingConfigMap`, whose `server.metricsAddr` the chart cannot inject. |
| `tests.*` | enabled, curl image | `helm test <release>` runs the packaged smoke script (initialize → tools/list → /health → /metrics) against the Service; `tests.tool` adds a `tools/call`. |
| `ingress.*` | disabled | Remember SSE annotations (long-lived streams) and that ingress hosts must appear in `allowedHosts`. |
| `autoscaling.*` / `podDisruptionBudget.*` / `metrics.serviceMonitor.*` | disabled | Standard optional resources. |

## Development

```bash
make helm-check   # helm lint --strict (values.schema.json enforced) + template render over deploy/helm/fold/ci/*.yaml at two Kubernetes versions
```

`values.schema.json` types every value, so a typo (`replicaCount: "2"`,
`probes.readiness.timeout`) fails `helm lint`/`install` instead of rendering
silently; `additionalProperties: false` at every level the chart owns means
an unknown key is an error too. Free-form objects the chart passes through to
Kubernetes (`config`, `resources`, `strategy`, `affinity`, …) stay untyped.

The `ci/` shapes each render at Kubernetes 1.31 (with the `preStop` sleep
action) and 1.28 (without). Two shapes are asserted to **fail**:
`metrics.listener.enabled` with `existingConfigMap` but without
`metrics.listener.externalConfigSetsMetricsAddr: true`, and a
`terminationGracePeriodSeconds` not exceeding `lifecycle.preStopSleepSeconds`
plus the drain — `ci/existing-configmap-metrics-values.yaml` is the passing
form of the first.

After an install, `helm test <release>` runs `files/smoke.sh` in a pod; the
same script is `scripts/smoke.sh` in the repo for running by hand.
