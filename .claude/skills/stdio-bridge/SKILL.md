---
name: stdio-bridge
description: Work on fold-stdio and internal/stdiobridge — the shim that exposes a stdio MCP server over streamable HTTP so the gateway can federate servers with no network endpoint. Use when changing cmd/fold-stdio, internal/stdiobridge, the stdio compose profile, or docs/stdio.md.
---

# The stdio shim

`fold-stdio` runs one stdio MCP server and serves it over streamable HTTP,
so the gateway can federate a server that has no network endpoint. It ships
as its own binary (`cmd/fold-stdio`), its own image
(`deploy/docker/stdio.Dockerfile`), and a compose profile.

```
fold-stdio --port 8091 -- npx -y @modelcontextprotocol/server-filesystem /data
```

The gateway then treats it as an ordinary HTTP upstream. That is the whole
point: a shimmed server should be indistinguishable from a native one.

## The invariant: the bridge is protocol-blind

`internal/stdiobridge` pumps JSON-RPC messages between an HTTP session and
a child process **verbatim**. No method table, no typed parameters, no
rewriting. Framing stays in the SDK on both sides —
`mcp.CommandTransport` owns stdio, `mcp.StreamableServerTransport` owns
HTTP — and the bridge owns only session bookkeeping and the pump.

This is what lets methods the SDK has not learned yet, and server-initiated
traffic like sampling and elicitation, cross without the bridge
understanding either. **Any change that teaches the bridge what a message
means is a regression**, even when it makes something work. If a fix seems
to need message inspection, the fix is in the wrong layer.

It is the same rule as the gateway's invisibility rule, one level down.

## The security invariant: the command comes from argv

**The command is fixed at startup from argv and is never taken from a
request, a config document, or discovery.** `docs/design-stdio.md` records
why. A shim that could be told what to execute is a remote-code-execution
endpoint wearing an MCP costume — and discovery-sourced upstreams make that
reachable by anyone who can create a Service.

Nothing in a change should widen this. If a feature seems to want
per-request command selection, it wants a second shim process instead.

The surrounding defaults follow the same posture, and each is a deliberate
narrow default rather than an oversight:

- `--host` binds **loopback**; `0.0.0.0` is "a deliberate act" in its own
  flag help.
- `--env-passthrough` defaults to **none** — the child inherits nothing.
- `--bearer-env` names an env var whose value callers must present. In
  compose this is `SHIM_TOKEN`, and both sides must agree on it: the
  Makefile writes it to `.env` once rather than generating per invocation,
  because a fresh value on every `up` leaves the running shim rejecting the
  gateway until both restart.
- `--max-sessions` bounds concurrency, and **each session is one child
  process** — this is a process-count bound, not just a memory one.
- `--dir` sets the child's working directory.
- `--probe` starts the server once to check it runs, then exits. Use it in
  a container healthcheck or to debug "the upstream is down".

## Tests

- `internal/stdiobridge/bridge_test.go` — the pump, session lifecycle,
  child-process teardown, body caps.
- `gateway/stdio_test.go` — the gateway federating a shimmed server, which
  is the test that matters: it proves the shim is invisible from the other
  side.

Real peers, per the repo's first rule. A change to the pump that keeps
`bridge_test.go` green and breaks `stdio_test.go` has broken the product.

## Docs

- `docs/stdio.md` — the operator's guide.
- `docs/design-stdio.md` — the design record, including the argv rule.
- `compose.yaml` — the `stdio` profile.
- The archive goreleaser builds ships `docs/stdio.md` alongside the binary.

Unlike `fold-discovery`, this one is **not** Linux-only: goreleaser builds
linux and darwin, because it runs wherever the stdio servers run, including
a developer's machine pointing a local gateway at a local server.
