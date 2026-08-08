# Stdio servers: `fold-stdio`

fold federates streamable-HTTP MCP endpoints. Most MCP servers are not that —
they are local processes speaking stdio. `fold-stdio` is the shim that closes
the gap: it runs one stdio server and exposes it over streamable HTTP, so the
gateway federates it as an ordinary upstream.

Nothing in the gateway or the config document knows about stdio. A shimmed
server is an `http://` upstream, which means credential strategies, health
checks, load balancing, breakers, timeouts, policy, pagination, and audit all
apply with no special case. The reasoning behind the shape is in
[design-stdio.md](design-stdio.md).

## Quick start

```bash
# Run the server behind a shim.
fold-stdio --port 8091 -- npx -y @modelcontextprotocol/server-filesystem /data
```

Then point an upstream at it:

```json
{
  "upstreams": [
    { "id": "files", "url": "http://127.0.0.1:8091/mcp", "namespace": "files" }
  ]
}
```

Everything after `--` is the server command and its arguments.

## Flags

| Flag | Default | Purpose |
|---|---|---|
| `--port` | `8091` | Port to serve the MCP endpoint on |
| `--host` | `127.0.0.1` | Bind address — loopback by default; `0.0.0.0` is a deliberate act |
| `--max-sessions` | `64` | Concurrent sessions, **each of which is one child process** |
| `--max-body-bytes` | `1048576` | Request body cap, matching the gateway's default |
| `--env-passthrough` | *(none)* | Comma-separated variable names the server may see |
| `--dir` | *(this process's)* | Working directory for the server |
| `--bearer-env` | *(none)* | Variable holding a token callers must present |
| `--probe` | `false` | Start the server once to check it runs, then exit |
| `--log-format` | `text` | `text` \| `json` |
| `--log-level` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `--version` | | Print the version and exit |

## Endpoints

- `POST|GET|DELETE /mcp` — the endpoint the gateway federates.
- `GET /health` — `200` when the server is runnable, `503` when it is not, plus
  session counters. Matches the gateway's own health semantics, so chart probes
  and `healthCheck.intervalMs` behave identically against a shim. Probing costs
  a process, so the answer is memoized for one second and single-flighted — and
  a live session is itself proof of health, so the common case costs nothing.
- `GET /metrics` — Prometheus text: `fold_stdio_sessions`,
  `fold_stdio_max_sessions`, `fold_stdio_spawned_total`,
  `fold_stdio_spawn_errors_total`, `fold_stdio_rejected_total`,
  `fold_stdio_build_info`.

## One process per session

Each downstream session gets its own child process. This is not a tuning knob:
a stdio connection carries exactly one MCP session, so two sessions sharing a
process would share a JSON-RPC id space, and the replies would cross. Demuxing
them would mean rewriting ids and synthesizing handshakes — a protocol-aware
proxy, which is what the gateway in front of the shim already is.

The practical consequences:

- **`--max-sessions` is a process ceiling.** The default of 64 is deliberately
  low enough that a misbehaving client cannot fork-bomb the host. A slot is
  held until its child is actually reaped — not merely until the session is
  removed — so open-and-abandon cannot run past the bound while old processes
  are still dying. At the ceiling the shim answers `503` with `Retry-After`,
  which the gateway's breaker reads as a temporarily unhealthy endpoint rather
  than a hard failure.
- **Idle sessions are swept after five minutes**, matching the gateway's own
  bridged-session sweep. A client that opens sessions and walks away without a
  `DELETE` would otherwise pin a process each, permanently.
- **Children run in their own process group**, which is swept on teardown.
  Wrappers like `npx` fork the real server and do not always forward signals;
  without the group kill those grandchildren would survive and reparent to
  PID 1 — which, in the shim's own image, is the shim.
- **Session count tracks connected clients**, because the gateway opens one
  bridged session per downstream client to carry sampling, elicitation,
  logging, and progress. A federation with many simultaneous clients on one
  shimmed server wants headroom, and `fold_stdio_sessions` is the number to
  watch.
- **Memory is per process.** A server that loads a large index on start pays
  that cost per session; such servers are better run natively over HTTP.

## Security

**The command never comes from the network.** It is fixed at startup from
argv — not from a request, not from a config document, not from discovery. The
shim runs exactly one command, chosen by whoever started the process. This is
the whole security story, and it is why stdio is not a field in fold's config
document: a `command` field would hand whoever controls the discovery document
an `exec` on the gateway host, which would reduce `allowedSecretRefs` and
`allowedCredentialHosts` to formalities (see
[security-model.md](security-model.md)).

**The child inherits nothing.** Its environment is built from
`--env-passthrough` and contains only the named variables, so a shim holding
its own bearer token in the environment does not hand it to the server it
supervises.

```bash
fold-stdio --env-passthrough GITHUB_TOKEN,HOME \
  --bearer-env SHIM_TOKEN --host 0.0.0.0 \
  -- npx -y @modelcontextprotocol/server-github
```

**A non-loopback bind requires a token.** The default is `127.0.0.1`. Binding
wider without `--bearer-env` is refused at startup (exit 2), not merely
discouraged: the shim executes a process on demand and must never be an open
exec-over-HTTP surface on a shared network. The container entrypoint binds
`0.0.0.0`, so the published image requires a bearer by construction.

**Browser-driven attacks are refused.** Because the shim wires the SDK's
transport directly rather than its handler, it re-applies the protections that
handler would have: a loopback listener rejects a foreign `Host` (DNS
rebinding — the attack that matters most for a shim on a developer machine,
since a rebound page is same-origin and CORS never fires), cross-origin
requests are checked, and POSTs must be `application/json` so a no-preflight
"simple" request cannot reach the child.

## Containers

`ghcr.io/fold-run/fold-stdio` carries Node, because `npx`-delivered servers are
the common case. The shim's image cannot be distroless like the gateway's — its
job is to execute a server, and most arrive through a language runtime — so the
base is a build argument:

```bash
docker build -f deploy/docker/stdio.Dockerfile -t fold-stdio .                          # Node (default)
docker build -f deploy/docker/stdio.Dockerfile --build-arg RUNTIME_BASE=python:3.13-slim .
```

The gateway image stays distroless — that separation is the point of putting
the runtime here. On Kubernetes, run the shim as a sidecar or its own
Deployment and Service, and let [`fold-discovery`](discovery-controller.md)
register it like any other Service: label it `fold.run/upstream: "true"` and it
joins the federation.

**Cold starts are per session.** `npx` resolves and starts the package on every
spawn, which is seconds — and since each session spawns a child, that cost is
paid per session, not once at boot. Two consequences worth planning for: raise
`timeouts.connectMs` on the upstream above the server's cold-start time, and
give the container a warm package cache (a writable `HOME`, or a pre-installed
server so the command is a plain binary rather than `npx -y`). A server that is
slow to start is a poor fit for per-session spawning generally.

## Troubleshooting

**The gateway reports the upstream unavailable.** Check the shim directly:

```bash
fold-stdio --probe -- <command>      # start the server once, report, exit
curl -s localhost:8091/health
```

`--probe` distinguishes "the server cannot start" from "the shim cannot serve",
which is the split that matters when a federation goes red.

**The server logs nothing.** Stdio servers use stderr for logging, since stdout
carries the protocol. The shim relays that stderr to its own logger at `debug`,
so run with `--log-level debug`.

**One session per client feels like a lot of processes.** It is; see above. If
the server is expensive to start, prefer running it natively over HTTP — the
shim exists to reach servers that have no HTTP mode, not to make every server
worth reaching that way.
