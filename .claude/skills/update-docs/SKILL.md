---
name: update-docs
description: Bring fold's docs in line with the working diff — map changed code to the README sections, docs/ guides, schema, and example config that track it, then fix the drift via the docs-sync agent. Use when the user asks to update docs, when the Stop hook flags code changes without doc changes, or before committing a behavior change.
---

# Update fold's docs from the working diff

Documentation drift is a merge-blocker in spirit here: README sections
track code directly, and the schema drift test makes part of it literal.
This skill turns "the diff changed behavior" into "the right doc surfaces
changed with it".

## 1. Scope the drift

```bash
git diff --name-only HEAD    # plus: git ls-files --others --exclude-standard
```

Map changed code to the surfaces that track it:

| Changed | Doc surface that must keep up |
| --- | --- |
| `gateway/router.go` middleware order | README "Request pipeline" |
| `config/config.go` fields | `config/fold.config.schema.json` (drift test), README "Configuration", `docs/defaults.md`, `fold.config.example.json` |
| Minted error codes | README "Error codes" |
| New/changed endpoints, metrics | README "Operational endpoints" / "Observability", `docs/operations.md` |
| Auth, policy, EMA | `docs/security-model.md`, README auth/policy sections |
| Embedding API (`gateway.New`/`Reload`/`Handler`) | `docs/embedding.md`, README "API stability" |
| Deploy artifacts (`Dockerfile`, `deploy/`, compose) | `docs/deploy.md`, README "Deploying" |
| Closing/widening a deliberate gap | README "Not implemented" |
| Architectural invariants (snapshot, sessions, pipeline) | `CLAUDE.md` |

Not doc-relevant: `*_test.go`-only changes, `.claude/` changes, pure
refactors with no observable behavior change — say so and stop.

## 2. Delegate to the docs-sync agent

For anything beyond a one-line fix, launch the **docs-sync** subagent with:
the list of changed files, a one-paragraph summary of the behavior change,
and the specific surfaces from the table above it should audit. It
verifies claims against code and edits the docs; relay its report.

Trivial single-surface fixes (one README line) may be done inline.

## 3. Verify

- Schema or example config touched → `go test ./config`.
- README "Changelog" is release-time only — never add entries here for
  unreleased work (that happens in `/fold-release` step 3).
- Report: surfaces updated, surfaces checked-and-already-accurate, and
  anything deliberately deferred (e.g. changelog).
