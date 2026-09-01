---
name: console-sync
description: Review, bump, or debug the vendored fold-console assets in gateway/console — the on-demand bump PR, the console-check CI gate, and the manifest allowlist. Use when the console-sync workflow is red, when a bump PR needs review, or when deliberately moving CONSOLE_COMMIT.
---

# The vendored console

`gateway/console` is **build output from another repo**, checked in.
`scripts/sync-console.sh` vendors it from `fold-run/fold-console` at a
pinned commit; `gateway/console_source.go` records that pin and is
generated, never hand-edited.

Assets are checked in rather than fetched because the Go module proxy is
fold's distribution channel: `go run github.com/fold-run/fold/cmd/fold@latest`
builds from the proxy zip alone, which runs no generators and carries no
submodule content. An unvendored tree is a `//go:embed` build failure for
every user installing that way.

**Review a bump as a supply-chain change.** These assets execute in an
operator's browser, same-origin with a page holding a live Bearer token in
memory. That is why nothing here auto-merges.

## Two different things are called console-sync

| | What it is | When it runs |
| --- | --- | --- |
| `make console-check` | CI gate proving `gateway/console` still equals its pin — re-vendors and fails on any diff. Its own job, because it needs network. | Every merge |
| `.github/workflows/console-sync.yml` | Resolves fold-console `main` and opens a **bump PR**. Never merges. | On demand (`workflow_dispatch`) — and as step 2 of `/fold-release` |

`console-check` going red means someone hand-edited vendored assets, or
the pin and the tree disagree. The fix is `make sync-console` and a review
of what moved — never editing `gateway/console` directly, which is the one
thing the gate exists to catch.

## Reviewing a bump PR

1. **Read the upstream diff, not this one.** The diff here is minified
   bundle output; reading it is not a review. Read `src/` on the upstream
   commit range. Upstream CI rebuilds and fails on drift between `src/` and
   `dist/`, so the bytes vendored here are entailed by that diff.
2. **Check the manifest.** `MANIFEST` in `scripts/sync-console.sh` is the
   exact allowlist of files that may enter the binary — `//go:embed` takes
   the whole directory, so without it anything upstream's build emitted
   alongside (a source map, a second chunk, a stats file) would ship to
   every operator and be served under `/console/`. A bump that adds a file
   to the embedded set is a reviewed change *here*, and is also the shape a
   supply-chain injection would take. `gateway/introspection_test.go`
   asserts the same set from the other side, against the embedded FS.
3. **`fonts/OFL.txt` is not decoration.** OFL 1.1 requires the licence
   accompany the Font Software, and these subsets are in every fold binary.
   It ships or the binary is out of compliance.
4. **An empty check list means "not run", never "passed".** GitHub creates
   no workflow runs for branches pushed with `GITHUB_TOKEN`, so `ci.yml`
   never fires on a bot-opened bump PR. The workflow runs the manifest and
   API-path tests inline before opening it; push any commit to the branch
   to trigger the full suite before merging.
5. Run the `security-auditor` agent over it before approving.

## Bumping by hand

```bash
CONSOLE_COMMIT=<40-hex> make sync-console   # try the target first
make console-check                          # then prove tree == pin
```

Edit the `CONSOLE_COMMIT` default in `scripts/sync-console.sh` and commit
it together with the re-vendored tree and the regenerated
`console_source.go` — the script's pin is what CI verifies against, so it
moves with the assets or the next `console-check` fails. Own commit, with
the reason. The pin is a commit SHA rather than a tag or release asset
because a SHA is already a content hash: immutable, and impossible to
re-point at different bytes.

## Failure triage

| Symptom | Cause |
| --- | --- |
| `has no dist/ directory` | Pin predates the SPA rename. Pre-v1.9 commits carry `console/` instead — bump forward. |
| `pinned commit is missing manifest files` | Upstream renamed or dropped a shipped file. Reconcile `MANIFEST` deliberately; do not just delete the entry. |
| `fatal: not a git repository` locally | The cached clone under `TMPDIR` was partially reaped by the OS. The script re-clones when `remote get-url origin` fails; if it still trips, delete the cache directory. CI never sees this — fresh `RUNNER_TEMP` each run. |
| `console-check` diff on a clean checkout | Vendored assets were hand-edited. Re-vendor; do not commit the hand edit. |
| Weekly job red with `! [rejected] … (stale info)` | **Fixed.** The job force-pushed a branch it had no remote-tracking ref for, because `actions/checkout` fetches only the default branch — `--force-with-lease` refuses rather than pushing. It recurred every week for as long as a bump PR stayed open at the same upstream commit. The job now exits 0 when the branch already exists. |

Because the branch name carries the upstream short SHA, an existing branch
is always the *same* bump awaiting review. To make the bot re-propose one
it already opened, **delete the branch** — closing the PR deliberately does
not, so a declined bump stays declined.
