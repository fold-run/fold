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
| `ingress.*` | disabled | Remember SSE annotations (long-lived streams) and that ingress hosts must appear in `allowedHosts`. |
| `autoscaling.*` / `podDisruptionBudget.*` / `metrics.serviceMonitor.*` | disabled | Standard optional resources. |

## Development

```bash
make helm-check   # helm lint + template render over deploy/helm/fold/ci/*.yaml
```
