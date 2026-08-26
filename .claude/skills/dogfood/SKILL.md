---
name: dogfood
description: Run fold locally and talk to it as a real MCP client — the dev stack, the repo's .mcp.json, and what to look at once it is up. Use to see a change from a client's side, to reproduce a client-reported bug, or when a test is green and the behavior still looks wrong.
---

# Running fold and using it

fold's central claim is about what a client experiences, and the fastest way
to check a claim about experience is to have the experience.

```bash
make dev-up       # everything-server on :8098, fold on :8099
make dev-status   # the health body, upstream by upstream
make dev-down
```

`scripts/dev-stack.sh` builds fold from the working tree, so it serves the
code you are editing. The upstream is the reference everything-server — the
same peer `make conformance` fronts, so what you see here and what CI gates
on are the same server. It is federated under the namespace `demo` rather
than passthrough, deliberately: namespacing is the behavior most worth
seeing from the outside (`demo__echo`, not `echo`).

Ports are overridable and are **not** the gateway's own default 8080, which
is a popular squat — Docker Desktop takes it. The script refuses to start on
a busy port and names the process holding it rather than failing obscurely.

## Two clients

**The Inspector** — one method, scriptable, no session state to reason
about. `/mcp-inspector` covers it, including the invisibility diff: the same
method against fold and against the upstream directly, compared.

**This Claude Code session** — `.mcp.json` at the repo root points at
`http://127.0.0.1:8099/mcp`, so with the stack up and the server approved,
the session's own tool list contains fold's federated tools. That is a
genuine end-to-end client, with all the awkwardness a real one has.

The gateway must already be running: fold serves streamable HTTP and has no
downstream stdio mode, so nothing here is self-starting. If the server shows
as failed, the stack is down — `make dev-up`, then reconnect.

## What to look at once it is up

- **Namespacing round-trip.** Tools list as `demo__*`; a call with the
  namespaced name reaches the upstream under its bare name. A call with the
  bare name answers `-31043`.
- **The console** at `http://127.0.0.1:8099/console/` — the federation view
  an operator gets, served from the vendored assets (`/console-sync`).
- **Health**, which answers **503 with a body** when an upstream is down.
  Stop the upstream and watch the breaker move; "listening" and "ready" are
  different claims and this is where the difference is visible.
- **The audit trail** on stdout in `$TMPDIR/fold-dev-stack/fold.log` — one
  event per terminal response, denials included.

## Adding upstreams

Edit the config the script generates (`$TMPDIR/fold-dev-stack/fold.dev.json`)
and `SIGHUP` the gateway — hot reload is a feature worth exercising rather
than restarting past. A second upstream makes list merging and cross-upstream
namespacing visible, which one upstream cannot show.

To federate a **stdio** server, put `fold-stdio` in front of it
(`/stdio-bridge`). One caveat learned the hard way: `npx -y <pkg>` as the
shim's command re-resolves the package on **every session**, because each
session spawns its own child process — slow enough to blow the gateway's
connect timeout and present as `initialize: request terminated without
response`. Install the package first and point the shim at a direct
`node`/binary invocation.
