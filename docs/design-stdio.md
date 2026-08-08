# Design: stdio upstreams

Status: **implemented** (`cmd/fold-stdio`, `internal/stdiobridge`). This doc
recorded the design before implementation and now serves as the decision
record; the operator guide is [stdio.md](stdio.md). It settles the decision the
[roadmap](roadmap.md) deferred to it: fold gains stdio reach through a sidecar
shim, not through subprocess supervision inside the gateway.

Two things in the original proposal did not survive implementation — the
`--shared-process` option turned out to be unsound rather than merely slower,
and the bridge ended up protocol-blind rather than protocol-aware. Both are
recorded under [What implementation changed](#what-implementation-changed).

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
side and `mcp.NewStreamableHTTPHandler` on the network side. (The network side
changed during implementation; see below.)

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
- `GET /metrics` — session count and ceiling, spawns, spawn failures,
  refusals at the ceiling.

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

### One process per session

One subprocess per incoming MCP session, bounded by `--max-sessions` — the
bound being the thing the gateway could not have provided — with processes
reaped on session close.

The original proposal offered `--shared-process` as an opt-in for servers
documented as multi-client-safe, and called the default a question that
deserved measurement. Implementation answered it more decisively than
measurement would have: see below.

## Config and docs

Nothing in the fold config document changes. A shimmed server is an `http://`
upstream like any other, which is the strongest evidence the shape is right —
this feature ships without touching the frozen config surface, the schema, or
the v1 contract.

## What implementation changed

Three findings, recorded because each contradicts something written above.

**`--shared-process` was unsound, not merely slower.** The proposal framed
sharing as a performance/isolation trade to be settled by measurement. It is
not a trade. A stdio connection carries exactly one MCP session, so two
downstream sessions on one process share a JSON-RPC id space: two pump
goroutines reading one connection steal each other's replies, and two clients
can both pick id `1`. The first implementation of the flag deadlocked the
second client immediately. Making it work would mean rewriting ids,
synthesizing per-client handshakes, and fanning out notifications — a
protocol-aware proxy, which is precisely what the gateway in front of the shim
already is. The flag was removed rather than fixed, and
`TestConcurrentSessionsDoNotCrossTalk` pins the invariant.

**The bridge is protocol-blind.** The proposal assumed the SDK's
`NewStreamableHTTPHandler` fronting an `mcp.Server` that forwards to the child.
That path cannot be transparent: server dispatch consults a per-server method
table *before* middleware runs, so the shim would have to enumerate every
method and would answer method-not-found for anything the SDK learned later.
Instead the bridge connects `mcp.StreamableServerTransport` and
`mcp.CommandTransport` directly and pumps `jsonrpc.Message` values between
them verbatim. Framing still belongs entirely to the SDK on both sides; the
bridge owns only session bookkeeping and the pump. Sampling and elicitation
work without the bridge knowing they exist, which is the property that matters.

The one exception, and it is deliberate: the bridge reads the JSON-RPC envelope
of a session-less POST to see whether it is an `initialize`. Only an initialize
earns a durable session; anything else — notably the protocol-negotiation probe
the SDK client sends first — is served on a session torn down with the request.
Without that, every client connect stranded a process for the life of the
shim. It is a routing decision about session durability, not inspection: no
message is altered.

**The shim is not linux-only.** `fold-discovery` is, because it runs in a
cluster. The shim runs wherever the stdio servers run, which includes developer
machines pointing a local gateway at a local server, so it releases for darwin
too. Its image also cannot be distroless: executing servers delivered by `npx`
or `uvx` needs a runtime, so `deploy/docker/stdio.Dockerfile` takes the base as a build
argument and defaults to Node. The gateway image stays distroless — keeping
that split is exactly why the runtime lives here and not there.

## What shipped

- **The bridge** — `internal/stdiobridge`: session-to-process mapping, the
  bidirectional pump, `--max-sessions` with a `503` + `Retry-After` refusal the
  breaker can read, reaping on session close and on shutdown, the env
  allowlist, and a body cap matching the gateway's own default.
- **The shim** — `cmd/fold-stdio`: `/mcp`, `/health`, `/metrics`, `--probe`,
  `--bearer-env` with a constant-time compare, and `fold-discovery`'s flag
  shape. `/health` is memoized for one second and single-flighted, and a live
  session short-circuits it entirely — a probe costs a process, so an
  unmemoized health endpoint would have been a fork bomb driven by whatever
  polls it. That is the same load amplification the gateway closed for its own
  `/health` in v1.5.0, and it would have been easy to reintroduce here.
- **Crash behaviour** — a child that dies ends its session cleanly rather than
  being restarted. The proposal called for restart with backoff; that is wrong
  for a per-session process, because the MCP handshake died with it and a
  silently restarted child would serve a client that believes it is initialized.
  The client reconnects, which is the honest signal. Child stderr relays to the
  shim's logger at `debug`, since stdio servers log there.
- **Tests**, per repo rule 1 — `gateway/stdio_test.go` runs a real SDK stdio
  server behind the bridge behind the gateway, asserting namespacing, the
  policy enforcement pair, and audit. `internal/stdiobridge/bridge_test.go`
  covers the process contract: transparency of the child's identity,
  server-initiated sampling across the bridge, env non-leakage, per-session
  isolation, cross-talk, the session ceiling, spawn failure, crash, and
  no orphans after `Close`.
- **Packaging** — `deploy/docker/stdio.Dockerfile` (runtime base as a build argument,
  defaulting to Node) and goreleaser entries for linux and darwin.

**The security review found the cost of being protocol-blind.** Connecting the
SDK's transport directly skips `StreamableHTTPHandler`, and that handler is
where the SDK keeps its *inbound* protections — loopback DNS-rebinding
rejection, cross-origin checks, the `application/json` requirement, and the
request-body cap. Bypassing it silently dropped all four, which on a loopback
shim meant any web page able to rebind DNS could drive the local server. They
are now re-applied in `Bridge.ServeHTTP`. The lesson generalizes: taking a
lower-level SDK seam means inheriting the responsibilities of the layer
skipped, and those were not visible from the design.

The same review closed a set of resource-bounding defects, each now covered by
a test: the session ceiling bounded map entries rather than processes (a slot
was released before its child died, so open-and-abandon ran far past the
bound); the body cap covered only the handshake; a body that was not JSON-RPC
at all still spawned a process, making an empty POST a fork/exec amplifier;
there was no idle sweeper, so an abandoned session pinned a process forever;
an unauthenticated `/health` could be poisoned by an aborted request into
reporting the shim unhealthy to every prober; and children were not put in
their own process group, so an `npx` wrapper's grandchildren survived teardown
and reparented to PID 1 — the shim itself. The container entrypoint's
`0.0.0.0` bind is now backed by a startup refusal when no bearer is configured,
rather than by documentation.

Not yet done: a compose example.

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
