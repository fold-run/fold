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
| Vendored console matches its pin | `make console-check` — needs network |

A single test runs with `go test ./gateway -run TestName -v`.

## Titles

A PR title is a **prose sentence naming the problem**, with no `area:`
prefix — *A retry answering an upstream's question arrived looking like a
first try*, *Every federated list was announcing a cache scope that does not
exist*. It says what was wrong, not what the diff does; the branch name
(`fix/forward-mrtr-continuation`) is where the change gets classified.
Merges are squashed, so the title becomes the commit subject verbatim.

Commits that go straight to `main` — releases, conformance-receipt
repoints — keep the `area: imperative summary` form (`release: v1.15.0`,
`docs: repoint the conformance receipt at the v1.14.0 run`). They are
housekeeping, with no problem to name.

## Running fold while you work on it

```bash
make dev-up      # the reference everything-server on :8098, fold on :8099
make dev-status  # health, upstream by upstream
make dev-down
```

`scripts/dev-stack.sh` builds the gateway from your working tree and puts a
real MCP upstream behind it, federated under the namespace `demo` so tools
arrive as `demo__echo`. Needs Go and node; no Docker. The repo's `.mcp.json`
points at that address, so an MCP client configured from this directory
talks to the gateway you are editing.

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

## The console is not maintained here

**`gateway/console/` is vendored output. Send UI changes to
[fold-run/fold-console](https://github.com/fold-run/fold-console).** CI's
`console` job re-vendors from the pinned commit and fails the build on any
difference, so an edit made directly here cannot merge. `make sync-console` is how the
bytes get here; `gateway/console_source.go` records which upstream commit they
came from.

## Pinned upstreams

CI pins the conformance suite to a commit and package version in
[`scripts/conformance.sh`](scripts/conformance.sh), and the console assets to a
commit in [`scripts/sync-console.sh`](scripts/sync-console.sh). Bump each
deliberately, in its own commit. Both pins are commit SHAs rather than tags: a
SHA is a content hash, so it cannot be re-pointed at different bytes.

Two weekly workflows watch for movement. [drift](.github/workflows/drift.yml)
tests against the latest MCP SDK and conformance suite and opens an *issue*,
because the fix is a judgement call. [console-sync](.github/workflows/console-sync.yml)
opens a *PR*, because the fix is the diff — but it never auto-merges: the
console executes in an operator's browser alongside a live token, so a pin bump
is reviewed as a supply-chain change.

## Security issues

Do not open public issues for vulnerabilities — see [SECURITY.md](SECURITY.md).

## License

Contributions are accepted under [Apache-2.0](LICENSE).
