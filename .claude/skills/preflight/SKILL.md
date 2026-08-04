---
name: preflight
description: Run fold's full local merge gate in the right order and interpret failures — make check, plus bench/conformance/helm-check when the change warrants them. Use before declaring work done, before commits, or when the user asks "is this ready" / "run the checks".
---

# fold preflight

CI gates every merge on: gofmt, `go mod tidy -diff`, vet, build,
`go test -race`, golangci-lint, govulncheck, the added-latency benchmark,
and the MCP conformance suite. This skill runs the local equivalent,
cheapest-first, and stops early on failure.

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
- **`deploy/helm/`** → `make helm-check`.
- **Untrusted-input parsers** (config parse, cursors, discovery docs) →
  `make fuzz` for a deeper pass; seed corpora already run in `make test`.

## Reporting

Report each gate run with pass/fail and real output for failures. If a
conditional gate was skipped, say why it didn't apply. End with a clear
verdict: ready to commit, or the ordered list of what's blocking.
