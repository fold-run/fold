# fold-registry: federating from an MCP Registry

`fold-registry` is a second producer for the discovery document that
[`fold-discovery`](discovery-controller.md) already writes. It reads an [MCP
Registry](https://registry.modelcontextprotocol.io) — the official one, or any
deployment of the same open-source API — and serves
`{"upstreams": [...]}` for a gateway's `discovery.url` to poll. The gateway
cannot tell the two producers apart, and does not need to: the document format
is the contract, and everything the gateway does with it — whole-document
validation, the credential allowlists, the base-config merge — applies
unchanged.

It exists because a registry is where an organization's list of approved MCP
servers already lives. Copying a URL out of one into a config file works right
up until the vendor moves the endpoint, deprecates the entry, or publishes a
new version — after which nothing tells the gateway.

## The allowlist is the point

There is **no "federate everything" mode**, and the `--servers` flag is
required. Which servers an organization trusts is the decision this producer
exists to record, not one it should infer from a public registry that anyone
may publish to.

```jsonc
// servers.json
{
  "servers": [
    { "name": "io.github.acme/tools", "namespace": "acme" },
    { "name": "ac.tandem/docs-mcp",   "namespace": "tandem" }
  ]
}
```

| Field | Required | Meaning |
|---|---|---|
| `name` | yes | The registry's own reverse-DNS name, exactly as published. |
| `namespace` | no | The MCP namespace fold exposes the server under. Worth setting: the derived default is the whole registry name flattened (`io.github.acme/tools` → `io-github-acme-tools`), and the namespace prefixes every tool name a model reads. |
| `id` | no | The upstream id, for policy, audit, and metrics. Defaults to the same derived form. |

Unknown fields are rejected rather than ignored — a misspelled key in the file
whose whole job is to bound what gets federated should fail loudly rather than
widen it silently.

## What it will not federate

Each of these is skipped with a log line naming the server and the reason; one
unusable entry never takes the rest of the document down.

| Entry | Why |
|---|---|
| Status is not `active` | The registry has deleted or deprecated it. Tracking that is most of the reason to follow a registry rather than copy a URL once. |
| No `streamable-http` remote | A package-only entry (npm, PyPI, OCI) has no network endpoint. Those are [`fold-stdio`](stdio.md)'s job, and running someone else's process is a deliberate operator act rather than something a registry sync should conjure. |
| `sse`-only | fold speaks streamable HTTP. The HTTP+SSE transport is not implemented and is on the specification's removal clock. |
| More than one `streamable-http` remote | fold's multi-endpoint `urls` means "the same server, reachable several ways", with sessions pinned per endpoint. Two URLs listed side by side in a registry are not documented as interchangeable, and assuming it would be a guess with a correctness cost. |
| The remote requires a secret header | See below. |

**Credentials, never.** This producer emits no `auth` block of any kind. It
reads a document it does not control, and a producer that could name secrets
would hand the registry the ability to point gateway-held credentials at
endpoints of its choosing — the exfiltration path
`discovery.allowedAuthStrategies` and `allowedSecretRefs` exist to close. So a
registry entry declaring a header it cannot work without (GitHub's hosted
server declares `Authorization`) is **skipped by default**, because federating
it would add an upstream that fails every call. Such a server belongs in the
base config, where a human chose the secret and the destination together.
`--allow-secret-headers` federates them anyway, for a deployment that has
arranged the credential some other way.

Nothing here is a substitute for the gateway-side allowlists. Set
`discovery.allowedAuthStrategies` / `allowedSecretRefs` /
`allowedCredentialHosts` — and `allowedUpstreamHosts`, which bounds where an
*uncredentialed* entry may point — as you would for any producer: the gateway
enforcing its own bounds on a document is what makes the producer's promises
checkable, and a registry entry needs no credential to put a tool in front of
every model behind the gateway.

## Deploying

```bash
fold-registry --servers ./servers.json --port 8091
```

```jsonc
// In the gateway's config — no static upstream is required when discovery
// supplies the whole federation.
"discovery": { "url": "http://127.0.0.1:8091/", "intervalMs": 60000 }
```

Sidecar is the simplest shape, for the same reason it is for `fold-discovery`:
the gateway requires `https` for `discovery.url` with loopback exempt, so a
producer in the same pod needs no TLS at all. Standalone needs TLS in front of
it, plus `--bearer-env` on the producer and `bearerSecretRef` on the gateway.

The container image is `ghcr.io/fold-run/fold-registry`.

**A private registry is the more interesting deployment.** The registry API is
open source, and `--registry-url` points at any deployment of it — so an
enterprise running its own gets a curated internal catalog that the gateway
tracks automatically, with `--registry-bearer-env` for one that authenticates
its readers.

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `--servers` | — | **Required.** Path to the allowlist document. |
| `--registry-url` | `https://registry.modelcontextprotocol.io` | Base URL of the registry to read. |
| `--registry-bearer-env` | — | Env var holding a bearer token for a registry that authenticates readers. |
| `--port` / `--host` | `8091` / `0.0.0.0` | Where the document is served. |
| `--interval` | `5m` | Registry poll interval. Longer than the Kubernetes producer's: a public registry is a remote party, and entries change on the order of days. |
| `--bearer-env` | — | Env var whose value callers must present as a Bearer token. |
| `--reserved-ids` | — | Ids/namespaces registry entries may not claim — list the gateway's static upstream ids so a registry entry cannot publish a document-freezing collision. |
| `--allow-secret-headers` | off | Federate entries whose remote requires a secret header. They will fail every call unless the credential arrives some other way. |
| `--log-format`, `--log-level`, `--version` | `text`, `info` | As in `fold`. |

`GET /health` reports sync status (`503` before the first successful sync); any
other `GET` serves the document. On the gateway side the sync outcomes show up
in `fold_discovery_syncs_total` and the reload logs — see
[operations.md](operations.md).

## How a sync fails

The distinction the producer draws is between one entry being unavailable and
the registry being unavailable.

- **One fetch fails** — that server drops out of this round's document and the
  rest still publishes. A registry that 500s on one entry must not empty a
  federation.
- **Every fetch fails** — the last good document keeps serving and the failure
  is reported on `/health`. That is a registry outage, not an emptied
  allowlist, and publishing an empty document would tell the gateway to retire
  every discovered upstream.

The same reasoning gives the producer its `503` before the first successful
sync: an empty document is a statement, and the producer should not make it by
accident.

One request goes out per allowlisted name, against the registry's
`/v0.1/servers/{name}/versions/latest` endpoint, rather than paging the whole
registry — the allowlist is curated and small, and paging tens of thousands of
public entries to find four of them would make the sync cost scale with
someone else's registry rather than with this deployment.
