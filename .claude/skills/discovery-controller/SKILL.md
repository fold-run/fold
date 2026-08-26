---
name: discovery-controller
description: Work on fold-discovery and internal/kubediscovery — the controller that turns labeled Kubernetes Services into fold's discovery document, and the trust boundary its allowlist flags defend. Use when changing cmd/fold-discovery, internal/kubediscovery, gateway/discovery.go, or docs/discovery-controller.md.
---

# The Kubernetes discovery controller

`fold-discovery` lists Services matching a label selector on an interval,
maps them to upstream entries via annotations, validates them with fold's
own `config` package, and serves `{"upstreams": [...]}` for the gateway's
`discovery.url` to poll.

Linux-only (goreleaser builds it for linux alone) — it runs in a cluster,
listing Services with the pod's service account.

## The constraint that shapes the whole package

**Poll-to-poll, no client-go, no informers.** fold polls the producer; the
producer polls the Kubernetes list API with the pod's service-account token
and plain HTTP. That is the entire Kubernetes client.

This keeps fold's module free of the Kubernetes dependency tree, and it is
load-bearing rather than incidental: `go.mod` has 14 direct dependencies
and pulling in client-go would multiply that, in a binary operators run at
the edge of their network. **Do not add client-go, informers, or a
controller-runtime dependency** to make something more elegant. If a change
seems to need watch semantics, it needs a shorter interval.

## The trust boundary is the reason for every allowlist flag

A Service annotation is a **lower-trust input than the config document**.
Anyone who can create a Service in a watched namespace can propose an
upstream. Every flag below exists to bound what that person can propose,
and each defaults to the safe end:

| Flag | Default | What it stops |
| --- | --- | --- |
| `--allow-auth-strategies` | none | A Service choosing how the gateway authenticates to it |
| `--allow-secret-refs` | none | A Service naming an env var whose value it wants sent |
| `--allow-credential-hosts` | unrestricted | A credentialed Service pointing at an attacker's host |
| `--reserved-ids` | empty | A Service claiming the id or namespace of a static upstream |
| `--allow-unprefixed-ids` | off (prefixing on) | Cross-namespace id collisions; **single-tenant clusters only** |
| `--min-health-interval-ms` | 1000 | A Service asking for a probe loop that hammers itself |
| `--bearer-env` | none | An unauthenticated reader of the discovery document |

When adding an annotation that reaches a new config field, the question is
not "does it validate" but **"what can someone who can create a Service do
with it, and which flag bounds that."** If the answer is "nothing bounds
it", the change needs a flag before it needs a test.

`fold.run/config` reaches every field of a fold upstream; the named
annotations are conveniences for the common case. So a new *config* field
is automatically reachable from a Service — which means
`/reloadable-state` and this skill overlap by design. Check both.

## It is designed sidecar-first, and that is a security decision

fold requires **`https` for `discovery.url`** — the discovery document is a
trust anchor, since it proposes upstreams — with loopback exempt. That one
rule shapes both deployment modes:

- **Sidecar in the gateway pod** (recommended): `--host 127.0.0.1`
  satisfies the https requirement with no TLS at all, adds no network hop,
  and gives every gateway replica a consistent view because the source is
  consistent. The pod's service account carries the Services permission.
- **Standalone Deployment + Service**: needs real TLS in front of the
  producer — a mesh, an in-cluster certificate, or an ingress — plus
  `--bearer-env` on the producer and `bearerSecretRef` on the gateway.

A change that makes the producer easier to reach over plain HTTP is
weakening a trust anchor, not improving ergonomics. Check which mode a
change assumes; the loopback exemption is the only reason mode one works.

## The gateway side

`gateway/discovery.go` polls `discovery.url`. What matters there is the
merge, and it is symmetric:

- Base config + discovery-sourced upstreams are merged, and **each side
  preserves the other's contribution** — a base reload does not drop
  discovered upstreams, a discovery sync does not drop configured ones.
- The **merged document is validated whole before any swap**. A bad
  discovery payload rejects the sync and leaves the old snapshot serving.
- `fold_discovery_syncs_total` counts outcomes — see `/observability` if
  you add a failure mode.

That is snapshot discipline, so a change here goes through
`/reloadable-state`'s test matrix, not just this skill's.

## Tests

- `internal/kubediscovery/kubediscovery_test.go` — annotation mapping,
  each allowlist rejection, validation.
- `internal/kubediscovery/fullloop_test.go` — the loop end to end.
- `gateway/discovery_test.go` — the gateway polling a producer, including
  the both-directions merge preservation.

## Docs and deploy

- `docs/discovery-controller.md` — labeling, the annotation table, the flag
  reference. A new annotation or flag lands here or it does not exist.
- `deploy/fold-discovery.yaml` — the standalone manifest, with the minimal
  RBAC (ClusterRole: `get`/`list` on `services`; a Role plus `--namespace`
  scopes it to one). If a change needs a new verb or resource, that is a
  privilege increase and belongs in the PR body, not in the manifest alone.
- `deploy/docker/discovery.Dockerfile` — CI's `image` job builds it per PR.
