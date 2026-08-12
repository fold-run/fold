# Embedding fold in a Go service

The `fold` binary is a thin CLI over a public Go API: build a `Gateway`
from the config document, mount its `http.Handler`, close it on shutdown.
Everything below compiles in CI — the code mirrors the
[package examples](https://pkg.go.dev/github.com/fold-run/fold/gateway),
which are part of the test suite.

```go
package main

import (
    "log"
    "log/slog"
    "net/http"

    "github.com/fold-run/fold/config"
    "github.com/fold-run/fold/gateway"
)

func main() {
    cfg, err := config.Parse([]byte(`{
        "upstreams": [
            {"id": "github", "url": "https://mcp.example.com/mcp", "namespace": "github"}
        ]
    }`))
    if err != nil {
        log.Fatal(err)
    }

    gw, err := gateway.New(cfg, gateway.WithLogger(slog.Default()))
    if err != nil {
        log.Fatal(err)
    }
    defer gw.Close()

    log.Fatal(http.ListenAndServe("127.0.0.1:8080", gw.Handler()))
}
```

Notes:

- **Config** comes from `config.Parse` (bytes), `config.Load` (file path),
  or a `config.Config` you construct; `gateway.New` validates either way.
  `config.Schema()` returns the JSON Schema if you want to lint documents
  in your own tooling.
- **`Handler()`** serves the MCP endpoint plus the operational endpoints
  (`/health`, `/metrics`, and the OAuth endpoints when auth/EMA are
  configured) — mount it at the root of a listener, not under a prefix.
  fold does not terminate TLS; that is your server's job. So are the
  listener's connection bounds: give your `http.Server` a
  `ReadHeaderTimeout` and an `IdleTimeout` (the CLI uses 10 s and 120 s),
  but **no `WriteTimeout`** — MCP responses ride long-lived SSE streams,
  and a write timeout severs them mid-response. `IdleTimeout` is safe:
  it applies only between requests on a keep-alive connection.
- **`WithLogger`** supplies a `*slog.Logger` for operational events; without
  it the gateway is silent. Per-request accounting is in `/metrics` and the
  audit sinks, not the log stream.
- **`Close()`** drains upstream sessions, stops background loops (sweeper,
  discovery, health probes), and flushes buffered trace spans. It is safe
  to call more than once.

## Hot reload

`Reload` applies a new document to a running gateway — the embedder
equivalent of the CLI's SIGHUP/`--watch`:

```go
next, err := config.Parse(newDocument)
if err != nil {
    // reject the push; the running config is untouched
}
if err := gw.Reload(next); err != nil {
    // also fine: validation failures and changes to construction-wired
    // sections (auth, server, routing, audit, tracing, discovery) are
    // rejected loudly while the old configuration keeps serving
}
```

The upstream set and policy engine swap atomically; upstreams whose config
is unchanged keep their live sessions; removed ones drain; connected
clients receive `list_changed`. Discovery-sourced upstreams (if the
`discovery` section is configured) survive a base reload unchanged.

## What is API

The stable embedding surface is defined by the README's
[API stability](../README.md#api-stability) section: the `gateway` package,
the `config` document structs and functions, and the contract types
(`auth.Principal` with its context helpers, `audit.Event`/`Outcome`). The
other exported constructors in `auth`, `policy`, and `audit` are wiring the
gateway threads through its packages — not an extension surface.

One practical use of the contract types: the verified caller rides the
request context, so middleware you wrap *inside* fold's handler chain — or
tool-side code in the same process — can read it with
`auth.PrincipalFromContext(ctx)`.
