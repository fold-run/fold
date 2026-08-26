---
name: helm-chart
description: Change fold's Helm chart safely — the three ci values shapes make helm-check renders against, the probe Host-header trap, Chart.yaml's two version lines, and the docs that track values. Use when editing anything under deploy/helm/, adding a value, or when make helm-check fails.
---

# Changing the Helm chart

`deploy/helm/fold` is a published artifact: the release workflow packages it
to `oci://ghcr.io/fold-run/charts/fold` and attests its provenance. A
published OCI tag is immutable, so a mistake here cannot be amended in
place — only superseded.

## The gate

```bash
make helm-check
```

Three things, and the third is the one people forget exists:

1. `helm lint` against `ci/default-values.yaml`.
2. `helm template` against **every** `ci/*.yaml`, with
   `--api-versions monitoring.coreos.com/v1` so the ServiceMonitor renders
   without a cluster.
3. **The required-config guard**: rendering with *no* config must **fail**.
   A chart that installs with no upstreams would start a gateway that
   federates nothing, so the failure is the feature. If your change makes a
   bare `helm template` succeed, you have broken it.

Not part of `make check` — it needs helm on PATH, and the contributor
toolchain stays Go-only. CI runs it as its own `helm` job.

## The three ci values files are three deployment shapes

They are not examples; they are the render matrix. A new value should be
exercised by at least one, and a value that changes rendering in a shape
none of them covers needs a fourth or an extension to one.

| File | Shape it pins |
| --- | --- |
| `default-values.yaml` | Minimal: inline config, no `allowedHosts` |
| `existing-configmap-values.yaml` | Operator-managed ConfigMap, Redis by URL |
| `full-values.yaml` | Ingress, HPA, PDB, ServiceMonitor, Redis via secret, secrets via `envFrom` |

## The probe Host-header trap

fold's DNS-rebinding protection covers `/health` like every other path, so
a kubelet probe carries a `Host` the allowlist must admit. The chart
resolves this three different ways, and each ci file exists to pin one:

- No `allowedHosts` → the probe Host falls back to `localhost`.
- `allowedHosts` set → it derives from `allowedHosts[0]`.
- `existingConfigMap` → **`probes.hostHeader` is mandatory**, because the
  chart cannot see inside an operator's ConfigMap to find the allowlist.

Any change to probes, `allowedHosts` handling, or the ConfigMap path has to
keep all three true. `docs/deploy.md` has "allowedHosts and health probes"
and "Probes" sections that say so in prose — they move together.

The same trap bites the compose observability profile for a different
reason: Prometheus scrapes with `Host: fold`, so `/metrics` needs the
service name in the allowlist. Same protection, different caller.

## Chart.yaml carries two versions and they mean different things

- **`version`** — the chart's own, and the OCI tag it publishes under. Bump
  it on **any** change under `deploy/helm/`, patch-level unless the chart's
  interface changed. A reused tag is a different chart wearing one name.
- **`appVersion`** — the gateway a default install actually deploys.
  `values.yaml` ships `image.tag: ""` and the deployment renders
  `{{ .Values.image.tag | default .Chart.AppVersion }}`, so this is not a
  label — it is what users get. It also stamps
  `app.kubernetes.io/version` on every object. `/fold-release` bumps it,
  and the release workflow's `chart` job refuses to publish when it does
  not name the tag.

They move independently: a chart fix that ships no new gateway is still a
chart release.

## What else a value touches

- `deploy/helm/fold/README.md` — the values table. It is the chart's
  documentation for anyone installing from OCI without this repo.
- `docs/deploy.md` — "Kubernetes (Helm)" and the "Production checklist".
- `templates/NOTES.txt` if the post-install instructions change.
- Metrics that appear in `dashboards/` or `templates/prometheusrule.yaml`
  are the observability pack — see `/observability`, and note the pack test
  lives in `gateway/`, not here.

## Before done

```bash
make helm-check
helm template fold deploy/helm/fold -f deploy/helm/fold/ci/full-values.yaml | less   # read the output
```

Render and *read* it. `helm-check` proves the templates parse; only reading
the manifest proves they say what you meant.
