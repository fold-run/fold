# Contributing to fold

Thanks for helping build the enterprise MCP gateway. This document covers
the mechanics; the [README](README.md) covers what fold is and how it is
put together.

## Setup

You need the Go toolchain version in [`go.mod`](go.mod) (newer Go versions
download it automatically) and, for the conformance suite only, Node 22+.

```bash
git clone https://github.com/fold-run/fold.git
cd fold
make check   # fmt-check + tidy-check + vet + build + race + lint
```

`make lint` needs [golangci-lint](https://golangci-lint.run/welcome/install/)
on your PATH; the config is [`.golangci.yml`](.golangci.yml).

## Before opening a PR

Run `make check`. CI gates every merge on those same targets plus:

| Gate | Command |
|---|---|
| Vulnerability scan | `make vuln` |
| Added-latency benchmark | `make bench` — added p50 < 5 ms through the proxy path |
| MCP conformance suite | `make conformance` — 40/40 checks, needs node/npx |

A single test runs with `go test ./gateway -run TestName -v`.

Commit messages follow `area: imperative summary` (e.g. `gateway: sweep idle
bridged sessions`, `docs: ...`), matching `git log`.

## Rules the repo follows

1. **Test with real peers.** Integration tests spin up real MCP servers from
   the official Go SDK behind the gateway — hand-rolled fixtures only for
   instrumentation. Redis paths are tested against miniredis.
2. **The gateway stays invisible.** Behavior through the gateway must match
   hitting the upstream directly; the conformance suite enforces this. Don't
   buffer or rewrite responses unless federation requires it (namespacing,
   list merging, policy filtering).
3. **Performance is a test.** The bench job gates merges on added latency;
   keep the proxy path allocation-light.
4. **Never hand-roll protocol framing.** The wire protocol is the official
   [MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk) on both the
   client-facing and upstream-facing sides.
5. **Known gaps are documented.** Deliberately unimplemented features live in
   README "Not implemented" — update it if you close or widen a gap.

## Pinned upstreams

CI pins the conformance suite to a commit and package version in
[`scripts/conformance.sh`](scripts/conformance.sh); bump both deliberately in
their own commit. A weekly [drift workflow](.github/workflows/drift.yml)
tests against the latest MCP SDK and conformance suite and opens an issue
when upstream moves — fixes for drift issues should also update the pins.

## Security issues

Do not open public issues for vulnerabilities — see [SECURITY.md](SECURITY.md).

## License

Contributions are accepted under [Apache-2.0](LICENSE).
