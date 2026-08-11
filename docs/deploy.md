# Deploying fold

fold is a single static binary with no local state: it never writes to disk,
terminates no TLS, and keeps everything cross-request either in memory or in
an optional Redis. That makes every deployment shape the same three
decisions — how the config document reaches the process, how the `secretRef`
environment variables reach it, and what sits in front of it for TLS.

- **Docker / compose** — simplest; the image is ~22 MB distroless.
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
release (`v0.7.0`).

## docker compose

[`compose.yaml`](../compose.yaml) at the repo root runs the gateway with
`./fold.config.json` mounted, plus optional services under profiles:

```bash
cp fold.config.example.json fold.config.json   # then edit
docker compose up -d
curl -fsS http://localhost:8080/health

docker compose --profile redis up -d           # with shared-state Redis

# With a local stdio MCP server behind the shim (see docs/stdio.md). The
# token goes to both services: the shim authenticates with it, and the
# gateway resolves the upstream's `secretRef` from its own environment.
SHIM_TOKEN=$(openssl rand -hex 16) docker compose --profile stdio up -d
```

The Makefile wraps the same thing, generating `SHIM_TOKEN` into `.env` once
so a restart does not invalidate the running shim's:

```bash
make compose-up                                  # stdio + observability
make compose-up PROFILES=                        # gateway only
make compose-up PROFILES="stdio redis observability"
make compose-logs
make compose-down
```

`fold.config.json`, `.env`, and `./data` (whatever the filesystem server is
pointed at) are local operator state and are gitignored.

Dockerfiles live in [`deploy/docker/`](../deploy/docker) — `fold.Dockerfile`,
`discovery.Dockerfile`, and `stdio.Dockerfile`. The compose file references the
published images by default; the `build:` stanzas to run from source are
commented in place.

The fold service has no compose healthcheck — distroless images carry no
shell or curl to run one with. Probe `/health` from the host or your
monitoring instead.

## Kubernetes (Helm)

The chart publishes to `oci://ghcr.io/fold-run/charts/fold` with every
release, and also lives in [`deploy/helm/fold`](../deploy/helm/fold):

```bash
helm install fold oci://ghcr.io/fold-run/charts/fold \
  -n fold --create-namespace -f my-values.yaml

# or from a checkout, unchanged:
helm install fold deploy/helm/fold -n fold --create-namespace -f my-values.yaml
```

Two version numbers, deliberately separate: the chart's own `version` is what
`--version` pins, and `appVersion` is the gateway build it deploys by default
— a chart fix that ships no new gateway is still a chart release. The release
workflow refuses to publish a chart whose `appVersion` does not name the tag
being released, so the pair cannot drift apart in the registry.

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

For a single host, `docker compose --profile observability up` brings up
Prometheus and Grafana with fold's dashboard already loaded — the same
dashboard file the chart ships, mounted rather than copied. It needs one
config line, because DNS-rebinding protection covers `/metrics` too and
Prometheus scrapes by service name: `"server": { "allowedHosts": ["localhost",
"fold"] }`. Without it the target is answered 403 and reads as down.

Set `metrics.listener.enabled=true` and the chart gives fold a second port for
`/metrics` and `/health`, sets `server.metricsAddr` in the rendered config, and
scrapes that port — which is what makes the ServiceMonitor work without adding
`"*"` to `allowedHosts`. Keep the port off any public ingress; it is guarded by
network scope, not by a Host check.

Observability ships with the chart: `metrics.serviceMonitor.enabled` for
scraping, `metrics.prometheusRule.enabled` for the alert rules, and
`metrics.dashboard.enabled` to hand the Grafana sidecar the packaged
dashboard. What they mean, and the SLOs behind them, are in
[operations.md](operations.md#dashboards-alerts-and-slos).

How the pieces map:

- **Config** — either inline under `config:` (the chart renders it into a
  ConfigMap, and a checksum annotation rolls the Deployment when it changes)
  or `existingConfigMap:` naming a ConfigMap you manage with the document
  under the key `fold.config.json` (then `probes.hostHeader` becomes
  required — see below). With an externally managed ConfigMap, add
  `server.extraArgs: ["--watch"]` so fold hot-reloads the mounted document
  when Kubernetes syncs it (the mtime poll handles the atomic-rename update
  ConfigMap mounts perform) — no reloader controller or rollout needed for
  the reloadable sections (see [Hot reload](#hot-reload)).
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
`403` — and that includes `/health` and `/metrics`, not just `/mcp`. When
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

`/health` is not a trivial endpoint: every call pings all upstreams
concurrently with a 5-second internal budget and returns `503` when none are
reachable. The chart's probe defaults follow from that:

- **Readiness**: `httpGet /health`, period 15 s, timeout 8 s (above the 5 s
  internal budget) — pods only receive traffic while at least one upstream
  is reachable.
- **Liveness**: plain TCP connect, deliberately *not* `/health` — liveness
  should detect a wedged process, not restart pods because upstreams are
  down, and shouldn't generate upstream traffic every few seconds.
- **Startup**: `httpGet /health` with a ~2-minute budget for first upstream
  connects and JWKS fetches.

Upgrading from v1.4 or earlier: the path was `/healthz`, which still works
as a deprecated alias (identical response, `Deprecation: true` header, one
log line on first use). Nothing breaks on upgrade, but move probes,
load-balancer target checks, and uptime monitors to `/health` — the alias
goes away in the next major.

Shutdown: on SIGTERM the gateway drains for up to 10 s, then exits;
long-lived SSE streams are cut at that bound. The chart sets
`terminationGracePeriodSeconds: 30` to stay clear of it.

## Hot reload

A running gateway applies config changes without a restart, three ways:
`kill -HUP <pid>` re-reads the config source; `--watch` polls the config
file's mtime (2 s) and reloads on change; embedders call `gw.Reload(cfg)`.
The upstream set and the policy engine swap atomically — unchanged upstreams
keep their live sessions, removed ones drain, clients get `list_changed` —
while the `auth`, `server`, `routing`, `audit`, `tracing`, and `discovery`
sections are fixed at startup: a reload that touches them fails loudly and
the running configuration keeps serving, so a bad push never takes the
gateway down. A rejected or invalid document is logged and ignored the same
way.

Restart-only changes (auth issuers, listen address, tracing endpoint) still
need a rollout; in Kubernetes the inline-config checksum annotation does
that automatically, and under systemd `ExecReload` covers the SIGHUP path
(see the unit below).

For upstreams that come and go without any operator involvement, see the
`discovery` section in the README — fold can poll a registry document and
swap discovered upstreams in and out on its own.

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
EMA replay protection, and task ownership live in memory. With multiple
replicas, set `REDIS_URL` (or `server.redisUrl`) so those behave fleet-wide
— otherwise each replica enforces its own rate-limit window, trips its own
breaker, an EMA ID-JAG could be redeemed once per replica, and **a task's
binding to the principal who minted it holds only on the replica that
served the mint**: elsewhere it falls through to the probe path and is
reachable by any caller. If you run more than one replica with task-using
upstreams, Redis is the difference between a guarantee and a coincidence.

Operationally forgiving by design: every Redis operation is bounded at
500 ms and fails open, so a Redis outage degrades to per-instance state
instead of taking the gateway down (replay protection and task ownership
fall back to each instance's local mirror rather than to nothing). A bad URL is a boot failure (validated
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
# `systemctl reload fold` hot-reloads the upstream set and policy.
ExecReload=/bin/kill -HUP $MAINPID
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
      and `/health` degradation (plus `fold_discovery_syncs_total`
      `rejected`/`error` outcomes when discovery is enabled)
