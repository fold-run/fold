# fold-discovery: Kubernetes upstream discovery

`fold-discovery` completes the self-serve federation story on Kubernetes: a
team ships an MCP server, labels its Service, and the tools appear behind
the gateway — no fold config change, no operator involvement.

It is the producer half of fold's [discovery](configuration.md#discovery)
mechanism: it lists Services matching a label selector on an interval
(default 15 s), maps them to upstream entries via annotations, validates
each entry with fold's own `config` package, and serves the
`{"upstreams": [...]}` document a gateway's `discovery.url` polls. It is
deliberately poll-to-poll — plain HTTP against the Kubernetes list API with
the pod's service account, no informers, no client-go dependency.

## Labeling a Service

```yaml
apiVersion: v1
kind: Service
metadata:
  name: search
  namespace: prod
  labels:
    fold.run/upstream: "true"        # the selector (configurable via --selector)
  annotations:
    fold.run/namespace: "search"     # optional overrides, see below
spec:
  ports:
    - name: mcp
      port: 8080
```

This Service becomes the upstream
`{"id": "search", "url": "http://search.prod.svc.cluster.local:8080/mcp", "namespace": "search"}`.

### Annotations

| Annotation | Default | Meaning |
|---|---|---|
| `fold.run/id` | Service name | Upstream id (fold's `[a-z0-9-]` rules apply). |
| `fold.run/namespace` | the id | MCP namespace (`{namespace}__{tool}`). |
| `fold.run/port` | port named `mcp`, else first port | Service port for the URL. |
| `fold.run/path` | `/mcp` | MCP path. |
| `fold.run/scheme` | `http` | URL scheme (in-cluster default). |
| `fold.run/url` | — | Full URL override; replaces the derived cluster-DNS URL (external endpoints, non-standard addressing). |
| `fold.run/config` | — | A JSON object decoded as the upstream entry itself — anything fold's schema allows (`auth`, `rateLimit`, `circuitBreaker`, `timeouts`, …). Derived defaults fill only fields it leaves unset. `secretRef` values name env vars **on the gateway**, which must hold them. |

**Labeling rights are registration rights — bound them.** The producer is
default-deny about credentials: a Service carrying any auth strategy or
`secretRef` in `fold.run/config` is skipped unless `--allow-auth-strategies`
and `--allow-secret-refs` grant it, because an ungated reference would let
any Service author point a gateway-held secret at a URL of their choosing.
The gateway enforces the same bounds independently via
`discovery.allowedAuthStrategies` / `allowedSecretRefs` / `allowedCredentialHosts` / `allowedUpstreamHosts` (see the README and
[security-model.md](security-model.md)) — set both when registrants and
gateway operators are different people. Use `--reserved-ids` for the
gateway's static upstream ids; namespace prefixing is already on by default.

Fail-safe mapping: a Service that produces an invalid entry (bad id,
malformed `fold.run/config`, no usable port), carries disallowed
credentials, sends them to a host outside `--allow-credential-hosts`, or
claims a reserved id is skipped. A contested id or namespace drops **every**
claimant rather than first-wins, so API list order cannot hand an identity to
whoever sorts earlier; the affected Services are skipped with a log line — one bad Service never takes the
rest of the document down. The document is sorted by id so
the gateway's change detection only fires on real changes, and the producer
serves `503` until its first successful list so a restart can never feed
the gateway an accidentally empty document.

## Deploying

**Recommended: as a sidecar in the gateway pod.** fold requires `https` for
`discovery.url` (it is a trust anchor), with loopback exempt — a sidecar
satisfies that with no TLS at all, adds no network hop, and gives every
gateway replica a consistent view because the source (the Kubernetes API)
is consistent:

```yaml
# In the gateway pod spec (serviceAccountName needs list/get on services):
containers:
  - name: fold
    # ... as in the chart ...
  - name: fold-discovery
    image: ghcr.io/fold-run/fold-discovery:latest   # pin a version in production
    args: ["--host", "127.0.0.1", "--log-format", "json"]
```

with the gateway config:

```jsonc
"discovery": { "url": "http://127.0.0.1:8090/upstreams.json", "intervalMs": 30000 }
```

**Standalone**: [`deploy/fold-discovery.yaml`](../deploy/fold-discovery.yaml)
(also shipped in the release archive) runs it as its own Deployment +
Service with the minimal RBAC (ClusterRole: `get`/`list` on `services`;
scope to a Role + `--namespace` for one namespace). Standalone requires TLS
in front of the producer's Service to satisfy the gateway's `https`
requirement — a mesh, an in-cluster certificate, or an ingress. Set
`--bearer-env` on the producer and `bearerSecretRef` on the gateway to
authenticate the poll.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--port` / `--host` | `8090` / `0.0.0.0` | Where the document is served. |
| `--namespace` | all namespaces | Scope the Service list. |
| `--selector` | `fold.run/upstream=true` | Label selector. |
| `--interval` | `15s` | Kubernetes list interval. |
| `--kube-api`, `--token-file`, `--ca-file` | in-cluster | API access overrides (e.g. `--kube-api http://127.0.0.1:8001` via `kubectl proxy` for local runs). |
| `--bearer-env` | — | Env var whose value callers must present as a Bearer token. |
| `--allow-auth-strategies` | none | Credential strategies Services may carry in `fold.run/config`. **Default-deny**: without this flag, a Service naming any credentialed strategy is skipped. |
| `--allow-secret-refs` | none | Env var names Services may reference in `secretRef` fields. Default-deny. |
| `--reserved-ids` | — | Ids/namespaces Services may not claim — list the gateway's static upstream ids so a registration cannot publish a document-freezing collision. |
| `--allow-unprefixed-ids` | off (prefixing **on**) | Namespace prefixing is the default: both the id and the MCP namespace must carry the registering namespace's prefix (hyphens are escaped so the prefix is unambiguous). Disable only in single-tenant clusters. |
| `--allow-credential-hosts` | none | Hosts (`*.suffix` wildcards, subdomains only) a credentialed Service may send secrets to — its endpoints **and** its `tokenEndpoint`. Required in practice whenever credentials are allowed. |
| `--min-health-interval-ms` | `1000` | Floor for a Service's `healthCheck.intervalMs`, so a registration cannot turn the gateway into a probe flood. |
| `--log-format`, `--log-level`, `--version` | `text`, `info` | As in `fold`. |

`GET /health` reports sync status (`503` before the first successful
list); any other `GET` serves the document. `/healthz` — the spelling
through v1.4 — is a deprecated alias of `/health`, kept because without it
the path would fall through to the document handler and an existing probe
would quietly start scraping the upstreams document instead. On the gateway side, the sync
outcomes show up in `fold_discovery_syncs_total` and the reload logs — see
[operations.md](operations.md).
