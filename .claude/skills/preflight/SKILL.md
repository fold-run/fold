---
name: preflight
description: Run fold's full local merge gate in the right order and interpret failures — make check, plus the conditional gates the diff warrants (bench, conformance, helm-check, vuln, console-check, image build). Use before declaring work done, before commits, or when the user asks "is this ready" / "run the checks".
---

# fold preflight

CI runs eight jobs: `test` (gofmt, `go mod tidy -diff`, vet, build,
`go test -race` + coverage), `lint`, `vuln`, `bench`, `helm`,
`conformance`, `console`, and `image`. `make check` covers `test` and
`lint`; everything else is conditional below. This skill runs the local
equivalent, cheapest-first, and stops early on failure.

## Always

```bash
make check        # fmt-check + tidy-check + vet + build + race + lint
```

Run failures to ground before moving on:
- **fmt-check** → `make fmt`, re-run.
- **tidy-check** → `go mod tidy`, then review the go.mod/go.sum diff —
  an unexpected dependency change is a finding, not a fix.
- **race failures** → these are real bugs in a gateway; never "fix" by
  removing `-race` or adding sleeps. Suspect snapshot access outside the
  atomic load, session-map access outside its lock, or test fixtures
  sharing state.
- **lint** → config is `.golangci.yml`; fix the code, don't add nolint
  without saying so.

## Conditional gates — run when the diff touches them

- **Proxy path** (`gateway/router.go`, `upstream.go`, `endpoints.go`,
  middleware, egress rewriting) → `make bench`. Gate: added p50 < 5 ms
  through the proxy path. A regression means a hot-path allocation or
  synchronous work that belongs at snapshot-build time.
- **Behavior through the gateway** (anything a client could observe) →
  `make conformance`. Needs node/npx; expects 40/40. A failed check
  usually means the gateway stopped being invisible — compare against
  hitting the upstream directly before touching the pinned commit.
- **`deploy/helm/`** → `make helm-check`. Lint, render against every
  `ci/*.yaml`, and the required-config guard.
- **`go.mod` / `go.sum` / a Go toolchain bump** → `make vuln`. This is its
  own CI job, not part of `make check`, so it is easy to skip and easy to
  be surprised by. A red govulncheck is often the *live* advisory database
  rather than the diff — check the Go patch version in `go.mod` against the
  advisory before hunting for a bug in the change.
- **`gateway/console/`, `scripts/sync-console.sh`, or a `CONSOLE_COMMIT`
  bump** → `make console-check` (needs network). Proves the vendored bytes
  are exactly the pinned fold-console commit. A hand edit under
  `gateway/console/` cannot merge at all — the PreToolUse guard blocks it,
  and CI's `console` job would fail it. See `/console-sync`.
- **`deploy/docker/*.Dockerfile`, `go.mod`'s Go version, or anything the
  images build from** → build the images locally:
  ```bash
  docker build -f deploy/docker/fold.Dockerfile --build-arg VERSION=ci .
  ```
  CI's `image` job covers this per-PR now, but it exists because a builder
  base pinned below the version `go.mod` requires sat on main through two
  green merges and failed after the tag was pushed — with the binaries and
  chart already published.
- **Untrusted-input parsers** (config parse, cursors, discovery docs) →
  `make fuzz` for a deeper pass; seed corpora already run in `make test`.

## Reporting

Report each gate run with pass/fail and real output for failures. If a
conditional gate was skipped, say why it didn't apply. End with a clear
verdict: ready to commit, or the ordered list of what's blocking.
