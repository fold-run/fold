# Design: stdio upstreams

Status: **proposed**. This doc records the design before implementation, and
the decision the [roadmap](roadmap.md) deferred to it: fold gains stdio reach
through a sidecar shim, not through subprocess supervision inside the gateway.

## Motivation

fold cannot front an MCP server that runs as a local process. Upstream
validation accepts only absolute `http(s)` URLs (`config/config.go:494`), and
the only client transport the gateway constructs is
`mcp.StreamableClientTransport` (`gateway/upstream.go:364`).

That excludes most of the ecosystem. The reference servers, the vendor
integrations people actually want governed, and nearly everything installed
through `npx` or `uvx` ship as stdio processes. An operator who wants fold's
policy, credential brokering, and audit in front of those has no path today —
which makes this the largest single gap in fold's reach, and it comes from
the ecosystem rather than from any competitive read.

The protocol side is not the problem. The SDK already ships
`mcp.CommandTransport` (`mcp/cmd.go:20`), which runs a command and speaks
newline-delimited JSON over its stdin/stdout, with stdin-close-then-SIGTERM
teardown. Swapping it in at `connectTo` is a dozen lines. The problem is
everything that swap drags into the gateway.

## The decision

Two shapes were considered, plus the option of shipping nothing.

### Why not in-gateway subprocess supervision

**Stdio is one process per session, and fold opens many sessions per
upstream.** This is the load-bearing argument. Every upstream holds one
shared `rootSession` for lists, reads, and subscriptions, plus a
`bridgedSession` per downstream client, keyed by downstream session id
(`gateway/upstream.go:429`, `:505`) — that is what carries sampling,
elicitation, logging, and progress back to the originating client. Against
streamable HTTP those are N concurrent HTTP sessions against one endpoint,
which costs almost nothing. Against `CommandTransport` each one is a separate
OS process.

So an in-gateway stdio upstream means the gateway spawns and supervises
`1 + N` processes per upstream, N scaling with connected clients, with the
five-minute bridged-session sweeper (`gateway/upstream.go:38`) doubling as a
process reaper. Every instance in a fleet spawns its own set; shared state
does not help, because a process is not state. A component whose design is "a
single static binary with no local state" acquires a process table.

**The deployment image cannot run the servers anyway.** fold ships from
`gcr.io/distroless/static-debian12:nonroot` — no shell, no Node, no Python.
Running `npx`-delivered servers means putting a language runtime into the
gateway image, which trades the ~22 MB distroless story and its attack
surface for a capability most deployments will not use.

**The surrounding machinery assumes network endpoints.** Multi-endpoint
`urls` with round-robin and connect failover (`gateway/endpoints.go`), the
`healthCheck` probe loop, and the connect/request/stream-idle timeouts are
all network concepts. Under a subprocess they either become meaningless or
need a parallel implementation. Credentials are worse than meaningless: all
five upstream strategies attach HTTP headers, and there is no header to
attach to a pipe.

**It puts process lifecycle behind the latency gate.** Restart policy, crash
backoff, zombie reaping, and fd accounting would live in the same binary that
gates merges on added p50 and on allocation discipline in the proxy path.

**And the security consequence is severe.** If a stdio upstream is a config
field, then whoever controls the discovery document controls an `exec` on the
gateway host. Discovery is explicitly a partially-trusted authorization
boundary — `allowedAuthStrategies`, `allowedSecretRefs`, and
`allowedCredentialHosts` exist precisely because the registry may not be the
operator ([security-model.md](security-model.md)). A `command` field turns
every one of those bounds into a formality. There is no allowlist design that
makes remote-supplied argv safe enough to be worth it.

### Why not "document an existing bridge and ship nothing"

Third-party stdio-to-HTTP bridges exist and work. Pointing at one costs
nothing to build, and it is a legitimate answer.

It is rejected for three reasons: it puts an unpinned third-party process
inside the trust boundary of a gateway whose whole pitch is a governed path;
its `/health` semantics will not match what fold's probes and the chart
expect; and it leaves the most-asked-for integration as a "wire up something
else" footnote rather than a supported path. Shipping a small binary is
cheaper than supporting an ecosystem of them.

### The shim

**`fold-stdio`** — a second binary that runs one stdio MCP server and exposes
it over streamable HTTP, using the SDK's `mcp.CommandTransport` on the process
side and `mcp.NewStreamableHTTPHandler` on the network side.

The gateway does not change at all. A shimmed server is an ordinary
`http://` upstream, so credential strategies, health checks, load balancing,
breakers, timeouts, policy, pagination, and audit all apply without a special
case — and the invisibility rule and the latency gate are untouched by
construction rather than by measurement.

The process fan-out does not vanish; it moves. That is the point. It moves
into a component whose job is process supervision, where it can be bounded,
restarted, and observed without any of it touching the gateway's hot path,
its deployment image, or its trust boundary. And the language runtime moves
with it: the shim's image carries Node or Python, or the operator supplies
their own base.

This follows the precedent `fold-discovery` already set — a second small
binary with its own image and manifest, rather than widening the first.

## What it is

```
fold-stdio --port 8091 -- npx -y @modelcontextprotocol/server-filesystem /data
```

Everything after `--` is the command and its argv, fixed at startup.

- `GET|POST /mcp` — the streamable HTTP endpoint the gateway points at.
- `GET /health` — matching fold's own `/health` semantics, so chart probes
  and `healthCheck.intervalMs` behave identically.
- `GET /metrics` — process state, session count, restarts, spawn failures.

Flags follow `fold-discovery`'s shape: `--port`, `--host`, `--log-format`,
`--log-level`, `--version`, plus `--bearer-env` for the same reason discovery
has it — the shim must not be an open exec-over-HTTP surface on a shared
network.

### The command never comes from the network

Fixed at startup from argv. Not from a request, not from a config document,
not from discovery. The shim runs exactly one command, chosen by whoever
started the process. This is the whole security story and it is not
negotiable: it is what keeps the discovery-boundary argument above from
reappearing one layer down.

Environment reaches the child through an explicit `--env-passthrough`
allowlist of variable names rather than by inheriting the shim's environment,
so a shim holding its own bearer secret does not hand it to the server it
supervises.

### One process per session, by default

The one real design question. Stdio servers are written for a single client,
and many keep per-client state; multiplexing several gateway sessions onto
one process would let one downstream client observe another's state, which is
a correctness and isolation bug that would be invisible until it bit.

So the default is one subprocess per incoming MCP session, bounded by
`--max-sessions` (the bound is the thing the gateway could not have), with
idle processes reaped on session close. `--shared-process` is available as an
opt-in for servers documented as multi-client-safe, and for the common
single-agent case where the fan-out is one anyway.

This deserves measurement before the default is frozen — if typical
federations run few enough concurrent clients that per-session spawning is
free, the default is easy; if not, the flag matters more than the default.

## Config and docs

Nothing in the fold config document changes. A shimmed server is an `http://`
upstream like any other, which is the strongest evidence the shape is right —
this feature ships without touching the frozen config surface, the schema, or
the v1 contract.

## Implementation phases

1. **The shim** — `cmd/fold-stdio` plus `internal/stdiobridge`:
   `CommandTransport` on one side, `NewStreamableHTTPHandler` on the other,
   session-to-process mapping, `--max-sessions`, reaping, `/health`,
   `/metrics`, and the env allowlist.
2. **Lifecycle hardening** — crash restart with backoff, spawn-failure
   surfacing through `/health` so the gateway's breaker sees it as an
   unhealthy endpoint rather than as a hang; child stderr relayed to the
   shim's logger at `debug`.
3. **Tests**, per repo rule 1 — a real SDK stdio server behind the shim
   behind the gateway, asserting the federated path end to end: namespacing,
   policy denial, audit, and the bridged surfaces (sampling and elicitation
   through two hops is the interesting case). Plus process-level tests: crash
   restart, `--max-sessions` refusal, no env leakage, and no orphans after
   shutdown.
4. **Packaging** — `Dockerfile.stdio` with a runtime base, a goreleaser entry
   (linux only, like `fold-discovery`), and a compose example.
5. **Review** — security-auditor, since this is a process-spawning network
   service; gateway-reviewer is largely a no-op because the gateway does not
   change.
6. **Docs** — `docs/stdio.md` as the operator guide, a README section, and a
   roadmap update moving the item from Horizon 2 to shipped.

## Explicitly out of scope

- **Installing or resolving servers.** The shim runs a command that already
  works on the box. No package fetching, no version resolution, no registry
  lookup — fold governs endpoints that exist, and this does not change that.
- **Supervising many servers from one shim.** One shim, one command. A
  federation of ten stdio servers runs ten shims, which is what makes each
  one's blast radius, image, and credentials independent.
- **Taking the command from discovery.** See above; this is the line the
  design exists to hold.
- **stdio on the client-facing side.** fold's downstream surface is
  streamable HTTP; a local client that wants a stdio endpoint is asking for a
  different program.
