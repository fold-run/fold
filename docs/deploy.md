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
resolves secrets, so it needs no credentials). Be precise about what that
covers: the document's structure and cross-field rules. It does **not** check
that the environment variables `secretRef` fields name are set, that an EMA
`signingKeyRef` holds a parseable key, that audit sink paths open, that Redis
answers, or that `metricsAddr` binds — all of those surface at startup, which
is why the chart's validating init container can run without secrets
mounted. A staging rollout is the check for the rest.

## Docker

```bash
docker run --rm -p 8080:8080 \
  -e FOLD_CONFIG="$(cat fold.config.json)" \
  --env-file fold.env \
  ghcr.io/fold-run/fold:latest --host 0.0.0.0
```

- `FOLD_CONFIG` takes either a file path or the JSON document itself —
  inlining it avoids a volume mount entirely. The config document carries no
  secrets by design, so inlining it on the command line is fine.
- Secrets referenced by the config's `secretRef` fields are ordinary
  environment variables. Put them in an env file (`chmod 600 fold.env`,
  one `NAME=value` per line) rather than `-e NAME=value` — a literal `-e`
  value lands in your shell history and in the process listing of the
  `docker run` invocation. (Either way the values are visible to anyone
  with Docker API access via `docker inspect`; that boundary is Docker's,
  not the flag's.)
- `--host 0.0.0.0` is required in a container: the binary binds `127.0.0.1`
  by default, which is unreachable through published ports.
- The image runs as nonroot on distroless static; `--read-only` works.

Images are multi-arch (linux/amd64, linux/arm64), tagged `latest` and per
release (`v0.7.0`).

### Verifying what you deploy

Every release artifact carries sigstore provenance signed by the release
workflow — the images, the Helm chart, the binary archives, and
`checksums.txt`. Verification proves the artifact was built by
`fold-run/fold`'s release workflow, not by whoever held a registry token:

```bash
# An image (also works for ghcr.io/fold-run/fold-discovery and fold-stdio):
gh attestation verify oci://ghcr.io/fold-run/fold:v1.15.0 --owner fold-run

# The Helm chart — the tag is required, since the chart repo has no :latest:
gh attestation verify oci://ghcr.io/fold-run/charts/fold:<chart-version> --owner fold-run

# A downloaded binary archive:
gh attestation verify fold_*_linux_amd64.tar.gz --owner fold-run

# Without the gh toolchain: the checksum file carries a keyless cosign
# signature (checksums.txt.sig + .pem on the release), and the checksums
# then cover every archive:
cosign verify-blob checksums.txt \
  --signature checksums.txt.sig --certificate checksums.txt.pem \
  --certificate-identity-regexp '^https://github.com/fold-run/fold/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
sha256sum --check --ignore-missing checksums.txt
```

**Then check the deployment itself, not just the artifact.** `scripts/smoke.sh`
does what a real client does — `initialize`, `tools/list`, an optional
`tools/call`, then `/health` and `/metrics` — and exits non-zero on the first
answer a healthy gateway would not give:

```bash
scripts/smoke.sh https://gw.example.com --token "$TOKEN" --tool gh__list_repos
make compose-smoke                       # against the local compose stack
helm test fold -n fold                   # the same script, run from a pod in the cluster
```

`helm test` sends the probe Host header and, with the metrics listener on,
checks `/metrics` on that port. It is the check to run after every upgrade,
because a rollout that leaves pods Ready but answering an empty `tools/list`
is precisely the failure readiness does not catch.

Images additionally embed BuildKit provenance and an SBOM in the registry
(`docker buildx imagetools inspect ghcr.io/fold-run/fold:v1.11.0` shows
them), and cluster admission controllers that verify GitHub attestations
(Kyverno, sigstore policy-controller) can enforce this at deploy time. For
production, prefer the release tag — or the digest the attestation names —
over `latest`.

## docker compose

[`compose.yaml`](../compose.yaml) at the repo root runs the gateway with
`./fold.config.json` mounted, plus optional services under profiles. Its
image tags are pinned to the release the chart's `appVersion` names — a test
holds them equal, and the release flow bumps them together — so `make
compose-up` runs the gateway the repo you cloned documents, and the alert
rules the observability profile mounts describe the codes that gateway emits:

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

The fold service's compose healthcheck runs the binary's own self-probe:
`fold --healthcheck` GETs the local `/health` and exits by result, which is
what gives the distroless image (no shell, no curl) a native HEALTHCHECK.
It deliberately counts *any* HTTP response as healthy — it answers "is the
process serving", and `/health`'s 503-when-all-upstreams-are-down must not
restart the container (see "Probes" below). The same flag works for plain
`docker run --health-cmd "/fold --healthcheck"`.

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
- **Rollouts** — the chart's `strategy` is `maxUnavailable: 0, maxSurge: 1`,
  so a serving pod is never taken away before its replacement is Ready
  (Kubernetes' own 25%/25% would let a two-replica rollout run on one pod,
  which the PDB's `minAvailable: 1` permits). A `preStop` sleep
  (`lifecycle.preStopSleepSeconds`, 5 s, Kubernetes 1.30+) lets endpoint
  removal reach kube-proxy and the ingress before SIGTERM closes the
  listener — without it every rollout refuses a few seconds of connections
  per pod. `server.drainTimeout` is fold's `--drain-timeout`; the chart
  refuses to render a `terminationGracePeriodSeconds` that does not exceed
  the sleep plus the drain, because the kubelet would kill the pod mid-drain.
  `revisionHistoryLimit` keeps five ReplicaSets for `helm rollback`, and
  `priorityClassName` is there because a gateway fronting every MCP call
  should be evicted last.

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
  is reachable. **Read the consequence before accepting it.** When *every*
  probeable upstream is down, every pod fails readiness at once, the
  Service has no endpoints, and clients see the ingress's 502/503 instead of
  fold's own answer — `-31041` on named calls, an empty list with a
  `partialFailure` marker on fan-outs — which is the behaviour the product
  is built around. `probes.readiness.mode: tcp` keeps pods in rotation on
  process liveness alone and lets fold's error semantics reach clients; the
  trade is that a pod with a wedged upstream set stays in rotation too.
  Neither is wrong; the default keeps the pre-0.4.0 behaviour, and
  `probes.readiness.path` is there for a deployment that wants an HTTP probe
  on something other than `/health`.
- **Liveness**: plain TCP connect, deliberately *not* `/health` — liveness
  should detect a wedged process, not restart pods because upstreams are
  down, and shouldn't generate upstream traffic every few seconds.
- **Startup**: `httpGet /health` with a ~2-minute budget for first upstream
  connects and JWKS fetches.

**Discovery-only federations wait on their source.** A gateway with no static
upstreams and a `discovery` section answers `503` until the first document is
applied, and says why in a `discovery` object on the response. That is what
keeps a pod whose registry was unreachable at boot out of the Service — it
would otherwise be Ready with an empty federation, serving every client an
empty `tools/list` with no error. Two consequences for probes: readiness holds
the pod out until the source answers, which is the point; and the startup
probe's budget (`probes.startup.failureThreshold × periodSeconds`, about two
minutes by default) now also bounds how long the source may be down at boot
before the pod is restarted. Raise it for a discovery-only federation whose
source is slow to come up alongside it. A source that applies an *empty*
document is `200`: the registry said the federation is empty, and fold
believes it.

**Upstreams with caller-derived credentials are not probed at all.** A probe
runs on nobody's behalf, so an upstream using `passthrough` or
`token-exchange` has no credential to present: it reports `"unprobed": true`
and counts as neither healthy nor unhealthy. This is not cosmetic. Pinging it
anyway would fail on every poll, and that failure is not free — it consumes
the upstream's rate budget, records a circuit-breaker failure (five polls
open the circuit for the clients it serves perfectly well), and charges the
upstream and server budgets once a real session exists. A federation made
entirely of such upstreams answers `200`, so the pod becomes Ready; without
that, `healthy == 0` would hold it out of rotation forever. For the same
reason `healthCheck.intervalMs` is ignored on those upstreams, with a warning
at startup — an active probe loop would eject every endpoint on its first
round.

The same split applies outside the chart — nomad checks, load-balancer
target groups, uptime monitors, hand-written manifests. **Never use
`/health` as a liveness/restart signal**: it answers `503` when every
upstream is down, so an orchestrator restarting on it kills perfectly
healthy gateways at the exact moment the federation is degraded — the worst
possible time. Use a TCP check for "is the process alive", `/health` for
"should this instance receive traffic", and give it a timeout above the 5 s
fan-out budget.

Upgrading from v1.8 or earlier: `/healthz` was the path through v1.4 and a
deprecated alias thereafter. **It was removed in v1.9 and now 404s.** Point
probes, load-balancer target checks, and uptime monitors at `/health` before
upgrading — a liveness probe left on the old path will fail the pod.

Shutdown: on SIGTERM the gateway drains for up to `--drain-timeout` (default
10 s), then severs whatever is still open and exits **0** — a stream cut at
the bound is the expected end of a routine stop on any gateway with clients
attached, not a failure, and it is logged as `drain deadline passed`. Keep
the drain under the orchestrator's grace period: the chart sets
`terminationGracePeriodSeconds: 30` to stay clear of the default.

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

## Rotating credentials

Hot reload covers the config *document*; it does **not** re-read the
environment. Every secret enters the process as an env var named by a
`secretRef`, and a container's environment is fixed at start — updating the
Kubernetes Secret changes nothing in running pods. Rotation is therefore
always: update the Secret (or EnvironmentFile), then restart the process
(`kubectl rollout restart deploy/<fold>`; `systemctl restart fold`). A
SIGHUP is not enough, however tempting the hot-reload section makes it look.

Per credential:

- **Upstream API keys / OAuth client secrets** (`auth.secretRef`,
  `clientAuth.secretRef`): overlap-friendly — provision the new credential
  at the upstream, update the Secret, rolling-restart, then revoke the old
  one. Zero downtime with `replicaCount` ≥ 2, the PDB, and — since chart
  0.4.0 — a rollout that never drops below the serving count
  (`maxUnavailable: 0`) with a `preStop` sleep ahead of SIGTERM, all of which
  the chart applies by default.
- **The EMA signing key** (`auth.ema.signingKeyRef`): read once at startup,
  and there is no dual-key overlap — after the restart the gateway trusts
  only the new key, so fold-minted tokens signed by the old key (up to
  `tokenTtlSec`, default 10 minutes old) fail verification and clients
  transparently re-exchange their ID-JAGs. Rotate off-peak if that brief
  re-exchange wave matters.
- **The Redis password** (`redis.existingSecret` / `REDIS_URL`): update and
  restart; during any window where an instance holds the old URL, state
  operations fail open per the Redis outage semantics below — degraded
  enforcement, not an outage.
- **The discovery bearer token** (`discovery.bearerSecretRef`) and **audit
  webhook headers**: same update-then-restart; a stale discovery token
  fails safe (last good upstream set keeps serving), a stale webhook header
  shows up as `fold_audit_events_total` retries/drops.
- **IdP signing keys (JWKS)**: the one rotation that needs nothing from
  you. The verifier refetches the JWKS on an unknown `kid`, so normal IdP
  key rollover is absorbed live — no restart, no config change.

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

### Private CAs

fold's outbound TLS — to upstreams, to the IdP's JWKS and token endpoints,
to Redis over `rediss://`, to an audit webhook or OTLP collector — trusts the
system root store, and the distroless image ships Mozilla's. An internal PKI
is not in it, and the image has no `update-ca-certificates` to add one. Go
reads two environment variables for exactly this: `SSL_CERT_FILE` (one PEM
bundle) and `SSL_CERT_DIR` (a directory of them), both honoured on Linux and
both **replacing** the system store rather than extending it — so a bundle
that must trust public CAs as well includes them.

With the chart, mount the bundle and point the variable at it:

```yaml
extraVolumes:
  - name: private-ca
    configMap:
      name: corp-ca-bundle          # key ca.pem
extraVolumeMounts:
  - name: private-ca
    mountPath: /etc/ssl/private
    readOnly: true
env:
  - name: SSL_CERT_FILE
    value: /etc/ssl/private/ca.pem
```

With compose, bind-mount the file and set the same variable in
`environment:`. The gateway has no `caFile` config field on purpose: the
variable is what Go already reads, a field would duplicate it inside the
frozen config surface, and `fold-discovery`'s `--ca-file` exists only because
the Kubernetes API's CA is a different trust decision from the gateway's.

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

**Redis down is degraded, never down — at boot too.** Every Redis operation
is bounded at 500 ms and fails open to the instance's local mirror, so an
outage turns the fleet into N gateways enforcing N copies of each limit
rather than into an outage of its own; `fold_state_degraded_total` counts
every such decision and `FoldStateDegraded` alerts on it. The same holds
when Redis is unreachable as a replica *starts*: it comes up, logs `redis
unreachable at startup` once, counts `fold_state_degraded_total{kind="startup"}`,
and reconnects on the next operation that reaches Redis. It used to refuse to
start, which made a Redis blip during a rolling restart fail every new pod
while the old ones would have carried on — the one moment fail-closed was
strictly worse than fail-open. A `redis://` URL that does not parse is still
fatal: that is a typo, and typos should not start.

Operationally forgiving by design: every Redis operation is bounded at
500 ms and fails open, so a Redis outage degrades to per-instance
enforcement instead of taking the gateway down. Every primitive keeps a
local mirror: replay protection, task ownership, budgets, and — since
v1.11 — rate limits and circuit breakers, whose mirrors are fed on every
healthy decision so they are warm when an outage starts. Degraded decisions
are counted (`fold_budget_degraded_total`, `fold_state_degraded_total`) and
the packaged alerts fire on them. A bad URL is a boot failure (validated
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
  stdout collector at your SIEM. The sink writes from a worker behind a
  bounded buffer, so a stalled log driver costs counted drops
  (`fold_audit_events_total{sink="stdout",outcome="dropped"}`), never request
  latency.
- **stderr** — operational logs (`log/slog`; `--log-format json`).

For direct SIEM delivery use the `webhook` sink instead (asynchronous,
batched; buffered events are dropped on overflow rather than adding request
latency — pair it with `deadLetterPath` so what a receiver outage refuses is
kept). If no `audit` section is configured, nothing is emitted.

Whether stdout is durable is a property of your collector, not of fold, which
is why `audit.requireDurable` does not count it: set that flag and fold
refuses to start unless a sink it can vouch for — a `file` sink, or a
`webhook`/`otlp-logs` sink with a `deadLetterPath` — is running. Use it where a
best-effort trail is not acceptable and you would rather learn at rollout than
during an incident review.

`GET /metrics` serves Prometheus metrics; the chart has an optional
ServiceMonitor (`metrics.serviceMonitor.enabled`).

## Production checklist

- [ ] `server.allowedHosts` pinned to your public hostname(s), not `["*"]` —
      and if it must be `["*"]` behind a proxy, `server.allowedOrigins` set,
      because the wildcard switches the Origin check off with it
- [ ] `auth.mode: "required"` with your IdP's issuer (anonymous gateways
      have no per-principal policy or rate limits; fold warns at startup when
      auth is off and the listener is bound beyond loopback)
- [ ] With EMA: `auth.ema.allowedAssertionTypes` (and `allowedClientIds`
      where your IdP stamps a `client_id`) — without them any IdP-signed JWT
      addressed to the resource is exchangeable, and fold warns at startup
- [ ] Every credentialed upstream (`static`, `passthrough`, `token-exchange`,
      `client-credentials`) reaches its endpoint over `https`, or over a hop
      your mesh encrypts — fold warns at startup for each one that does not
- [ ] `policy.defaultDecision: "deny"` with explicit allow rules (an absent
      `policy` block is an allow-all engine)
- [ ] `policy.serverInitiatedDecision: "deny"` with explicit grants for any
      upstream that legitimately needs sampling or elicitation. This one
      defaults to `"allow"` for upgrade compatibility, so it is the check a
      hardened deployment has to opt into: without it, an upstream can spend
      the caller's model budget or put a prompt in front of the caller's
      human, and deny-by-default governs only the other direction
- [ ] `pinDefinitions: "warn"` on upstreams you do not operate yourself, with
      `FoldDefinitionDrift` routed somewhere a human reads — otherwise a tool
      can acquire new instructions after you approved it and nothing says so
- [ ] If `hook` is configured: `onError` matches what your organization
      actually wants during a hook outage (traffic stops, or the gateway keeps
      serving uninspected), `timeoutMs` is smaller than your client timeouts,
      and `FoldHookErrors` is routed somewhere a human reads — under
      `onError: "allow"` that alert is the only signal that calls are going
      uninspected
- [ ] Discovery: `allowedAuthStrategies`, `allowedSecretRefs`,
      `allowedCredentialHosts`, and `allowedUpstreamHosts` set whenever the
      registry is not operated by the gateway's own operators (the gateway
      warns at startup when the credential three are all absent, and again
      when `allowedUpstreamHosts` is); avoid `"server": "*"` policy rules
      combined with discovery
- [ ] Multi-tenant: `server.introspection.groups` set — any valid principal
      may otherwise read the federation topology from `/api/federation`
- [ ] `rediss://` for Redis and HTTPS upstream URLs even inside the mesh
- [ ] TLS terminated in front; SSE timeouts/buffering configured
- [ ] Redis configured when running more than one replica
- [ ] An `audit` sink configured and shipped somewhere durable — with
      `requireDurable` set if a best-effort trail is not acceptable, which
      makes the gateway prove it at startup instead of at audit time
- [ ] `fold --validate` gating config changes in CI/CD — and a staging
      rollout for what it cannot see (secret env vars present, signing key
      parseable, sink paths writable, Redis reachable)
- [ ] Kubernetes: PodDisruptionBudget on (the chart's default when
      `replicaCount` ≥ 2), resource limits sized, probe Host header matches
      the allowlist, `networkPolicy.enabled` scoping the metrics port and —
      with `egress.enabled` — where upstream credentials may travel;
      `terminationGracePeriodSeconds` above preStop + drain (the chart
      checks), and a decision recorded on `probes.readiness.mode` for the
      all-upstreams-down case
- [ ] Private PKI: the CA bundle mounted and `SSL_CERT_FILE` pointing at it
      (see "Private CAs") — the image trusts only public roots otherwise
- [ ] Release artifacts verified at deploy time (`gh attestation verify`,
      or a cosign/Kyverno admission policy — see "Verifying what you deploy")
- [ ] A credential-rotation procedure written down (see "Rotating
      credentials"): secrets enter as env vars, and hot reload does *not*
      re-read them
- [ ] Alerts on `fold_upstream_breaker_state`, `fold_http_rejections_total`,
      `fold_panics_total`, and `/health` degradation (plus
      `fold_discovery_syncs_total` `rejected`/`error` outcomes when
      discovery is enabled)
