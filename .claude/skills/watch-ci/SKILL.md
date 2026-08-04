---
name: watch-ci
description: Watch fold's CI after a push or tag and triage failures back to local make targets. Use immediately after any git push, when the user asks about CI status, or when a run needs diagnosing.
---

# Watch fold CI

Every push gets watched — pushing and walking away is not done here.

## Workflow

1. **Find the run**: `gh run list -L 3` — match the head SHA to what was
   just pushed (`git rev-parse HEAD`). Tag pushes trigger `release.yml`
   (goreleaser) in addition to CI; watch both.
2. **Watch in the background**: `gh run watch <run-id> --exit-status` as a
   background Bash task, then continue or wait. Report the conclusion when
   it lands — pass or fail, never silently.
3. **On failure**, fetch the failing job's log
   (`gh run view <run-id> --log-failed`) and triage by job:

   | CI job | Local equivalent | Usual cause |
   | --- | --- | --- |
   | gofmt / tidy | `make fmt-check` / `make tidy-check` | forgot `make fmt` or `go mod tidy` |
   | vet / build / lint | `make vet` / `make build` / `make lint` | reproduces locally; fix and re-run |
   | race tests | `make race` | real concurrency bug — never sleeps or `-race` removal |
   | govulncheck | `make vuln` | new advisory; may be unrelated to the diff — check the CVE before touching deps |
   | bench gate | `make bench` | hot-path work; use `/perf`. Compare BENCH_RESULT vs recent green runs before calling flake |
   | conformance | `make conformance` | gateway stopped being invisible, or upstream drift; use `/conformance` |
   | helm | `make helm-check` | chart/values drift |

4. **Reproduce locally before pushing a fix** — CI is the verifier, not
   the debugger. Fix commits follow the same per-step approval as any
   other commit.

A scheduled drift workflow also runs conformance against unpinned
upstream (`main`/`@latest`); a red drift run signals upstream movement,
not a broken merge — handle via `/conformance` pin-bump, deliberately.
