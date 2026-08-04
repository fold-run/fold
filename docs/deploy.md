# Deploying fold

fold is a single static binary with no local state: it never writes to disk,
terminates no TLS, and keeps everything cross-request either in memory or in
an optional Redis. That makes every deployment shape the same three
decisions — how the config document reaches the process, how the `secretRef`
environment variables reach it, and what sits in front of it for TLS.

- **Docker / compose** — simplest; the image is ~18 MB distroless.
- **Kubernetes** — the [Helm chart](../deploy/helm/fold) encodes the
  probe/allowlist details below.
- **VM / bare metal** — prebuilt binaries + systemd.

Whatever the shape, run `fold --validate` against the config in CI or a
pre-start hook: it parses and validates the document and exits (it never
resolves secrets, so it needs no credentials).

## Docker

```bash
docker run --rm -p 8080:8080 \
  -e FOLD_CONFIG="$(cat fold.config.json)" \
  -e ML_SEARCH_API_KEY=... \
  ghcr.io/fold-run/fold:latest --host 0.0.0.0
```

- `FOLD_CONFIG` takes either a file path or the JSON document itself —
  inlining it avoids a volume mount entirely.
- `--host 0.0.0.0` is required in a container: the binary binds `127.0.0.1`
  by default, which is unreachable through published ports.
- Secrets referenced by the config's `secretRef` fields are ordinary
  environment variables (`-e NAME=...` or `--env-file`).
- The image runs as nonroot on distroless static; `--read-only` works.

Images are multi-arch (linux/amd64, linux/arm64), tagged `latest` and per
release (`v0.4.0`).

## docker compose

[`compose.yaml`](../compose.yaml) at the repo root runs the gateway with
`./fold.config.json` mounted, plus an optional Redis under a profile:

```bash
cp fold.config.example.json fold.config.json   # then edit
docker compose up -d
curl -fsS http://localhost:8080/healthz

docker compose --profile redis up -d           # with shared-state Redis
```

The fold service has no compose healthcheck — distroless images carry no
shell or curl to run one with. Probe `/healthz` from the host or your
monitoring instead.

## Kubernetes (Helm)

The chart lives in [`deploy/helm/fold`](../deploy/helm/fold) (repo-only for
now — install from a checkout):

```bash
helm install fold deploy/helm/fold -n fold --create-namespace -f my-values.yaml
```

```yaml
# my-values.yaml
config:
  upstreams:
    - id: github-tools
      url: https://mcp.platform.acme.com/mcp
      namespace: gh
      auth: { strategy: static, secretRef: GH_TOOLS_API_KEY, header: x-api-key, scheme: "" }
  server:
    allowedHosts: ["gw.example.com"]

envFrom:
  - secretRef:
      name: fold-upstream-secrets   # holds GH_TOOLS_API_KEY
```

How the pieces map:

- **Config** — either inline under `config:` (the chart renders it into a
  ConfigMap, and a checksum annotation rolls the Deployment when it changes)
  or `existingConfigMap:` naming a ConfigMap you manage with the document
  under the key `fold.config.json` (then config changes need a reloader
  controller or `kubectl rollout restart`, and `probes.hostHeader` becomes
  required — see below).
- **Secrets** — the config document never contains secret material; its
  `secretRef` fields name environment variables. Put the values in a
  Kubernetes Secret and inject with `envFrom`. The validate init container
  (on by default) needs none of them.
- **Redis** — set `redis.existingSecret` (or `redis.url`) to populate
  `REDIS_URL` when running more than one replica; see
  [Redis for fleets](#redis-for-fleets).

### allowedHosts and health probes

`server.allowedHosts` is the gateway's DNS-rebinding protection: any request
whose `Host` (or `Origin`) hostname is not on the allowlist is answered
`403` — and that includes `/healthz` and `/metrics`, not just `/mcp`. When
unset, the allowlist is the localhost set; when set, it **replaces** the
default rather than extending it. The port is stripped before matching, and
`["*"]` disables the check (only acceptable behind a trusted proxy that
sets/validates Host itself).

Kubelet probes send `Host: <podIP>:<port>`, which no sane allowlist
contains, so the chart's httpGet probes send an explicit `Host` header:
`probes.hostHeader` if set, else the first non-`"*"` entry of the inline
config's `allowedHosts`, else `localhost`. If you manage config outside the
chart, you must set `probes.hostHeader` to a hostname your allowlist admits —
the chart refuses to render otherwise, because the failure mode is a silent
403 loop where pods never become ready.

The same rule applies to any external health checker (load balancer target
checks, uptime monitors): whatever hostname they send must be on the
allowlist.

### Probes

`/healthz` is not a trivial endpoint: every call pings all upstreams
concurrently with a 5-second internal budget and returns `503` when none are
reachable. The chart's probe defaults follow from that:

- **Readiness**: `httpGet /healthz`, period 15 s, timeout 8 s (above the 5 s
  internal budget) — pods only receive traffic while at least one upstream
  is reachable.
- **Liveness**: plain TCP connect, deliberately *not* `/healthz` — liveness
  should detect a wedged process, not restart pods because upstreams are
  down, and shouldn't generate upstream traffic every few seconds.
- **Startup**: `httpGet /healthz` with a ~2-minute budget for first upstream
  connects and JWKS fetches.

Shutdown: on SIGTERM the gateway drains for up to 10 s, then exits;
long-lived SSE streams are cut at that bound. The chart sets
`terminationGracePeriodSeconds: 30` to stay clear of it.

## TLS and ingress

fold does not terminate TLS — put an ingress controller, load balancer, or
reverse proxy in front of it. Two things matter at that layer:

1. **SSE**: MCP responses ride long-lived SSE streams. Raise idle/read
   timeouts and disable response buffering:
   - ingress-nginx: `nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"`,
     `nginx.ingress.kubernetes.io/proxy-buffering: "off"`
   - AWS ALB: raise `idle_timeout.timeout_seconds` well above 60
   - Traefik: raise `respondingTimeouts.readTimeout`/`idleTimeout`
   - nginx (plain): `proxy_read_timeout 3600s; proxy_buffering off;`
2. **Host**: the public hostname the proxy forwards must be in
   `server.allowedHosts`.

## Redis for fleets

A single replica needs no Redis: rate limits, circuit breakers, list caches,
and EMA replay protection live in memory. With multiple replicas, set
`REDIS_URL` (or `server.redisUrl`) so those behave fleet-wide — otherwise
each replica enforces its own rate-limit window, trips its own breaker, and
an EMA ID-JAG could be redeemed once per replica.

Operationally forgiving by design: every Redis operation is bounded at
500 ms and fails open, so a Redis outage degrades to per-instance state
instead of taking the gateway down. A bad URL is a boot failure (validated
with a PING at startup). Any managed Redis- or Valkey-compatible service
works; the chart deliberately ships no Redis subchart — bring your own or a
managed offering.

## VM / systemd

Grab a binary from the [releases page](https://github.com/fold-run/fold/releases)
(tar.gz per OS/arch, with checksums, SBOMs, and build provenance), or
`go install github.com/fold-run/fold/cmd/fold@latest`.

```ini
# /etc/systemd/system/fold.service
[Unit]
Description=fold MCP gateway
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/fold --config /etc/fold/fold.config.json --host 0.0.0.0 --log-format json
# secretRef env vars (and optionally REDIS_URL), e.g. ML_SEARCH_API_KEY=...
EnvironmentFile=/etc/fold/env
ExecStartPre=/usr/local/bin/fold --config /etc/fold/fold.config.json --validate
DynamicUser=yes
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
Restart=on-failure
# The gateway drains for up to 10s on SIGTERM.
TimeoutStopSec=15

[Install]
WantedBy=multi-user.target
```

`chmod 600 /etc/fold/env`. The binary never writes to disk, so
`ProtectSystem=strict` needs no `ReadWritePaths`. Terminate TLS in front
(nginx/caddy/LB) per the section above.

## Audit and logs

The two output streams are separable by design:

- **stdout** — audit events (one JSON line per terminal response, including
  denials) when the `stdout` sink is configured. Point your log pipeline's
  stdout collector at your SIEM.
- **stderr** — operational logs (`log/slog`; `--log-format json`).

For direct SIEM delivery use the `webhook` sink instead (asynchronous,
batched; buffered events are dropped on overflow rather than adding request
latency — keep stdout as the durable copy if that matters). If no `audit`
section is configured, nothing is emitted.

`GET /metrics` serves Prometheus metrics; the chart has an optional
ServiceMonitor (`metrics.serviceMonitor.enabled`).

## Production checklist

- [ ] `server.allowedHosts` pinned to your public hostname(s), not `["*"]`
- [ ] `auth.mode: "required"` with your IdP's issuer (anonymous gateways
      have no per-principal policy or rate limits)
- [ ] `policy.defaultDecision: "deny"` with explicit allow rules
- [ ] TLS terminated in front; SSE timeouts/buffering configured
- [ ] Redis configured when running more than one replica
- [ ] An `audit` sink configured and shipped somewhere durable
- [ ] `fold --validate` gating config changes in CI/CD
- [ ] Kubernetes: PodDisruptionBudget on, resource limits sized, probe Host
      header matches the allowlist
- [ ] Alerts on `fold_upstream_breaker_state`, `fold_http_rejections_total`,
      and `/healthz` degradation
