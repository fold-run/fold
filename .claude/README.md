# Claude Code setup for fold

Project-level Claude Code configuration: skills, subagents, and hooks
tailored to this repo's invariants (see CLAUDE.md for the invariants
themselves). Checked in so every contributor's Claude session gets the
same guardrails.

## Skills (`skills/`) — invocable as slash commands

| Skill | Use |
| --- | --- |
| `/preflight` | Run the local merge gate in order (`make check`, plus bench / conformance / helm-check when the diff warrants) and interpret failures. |
| `/fold-release` | The release workflow: verify → per-step-approved commit/push → CI watch → tag (goreleaser) → CHANGELOG.md entry. |
| `/reloadable-state` | Checklist for adding config/state that must survive hot reload: snapshot placement, schema lockstep, reload/churn test matrix. |
| `/conformance` | Run, debug, or deliberately bump the pinned MCP conformance suite. |
| `/update-docs` | Map the working diff to the doc surfaces that track it and fix drift via the docs-sync agent. The Stop hook nudges toward this when code changes without doc changes. |
| `/perf` | Proxy-path latency work: measure via the bench-profiler agent, apply the move-to-snapshot-build fix pattern, verify with bench + race. |
| `/watch-ci` | Post-push CI watching: find the run, watch in background, triage failures back to local make targets. |

Claude also loads these automatically when a task matches the skill's
description.

## Subagents (`agents/`)

- **gateway-reviewer** (red) — read-only review of the working diff against
  the gateway's invariants (pipeline order, audit exit door, snapshot state,
  invisibility, error codes, hot-path allocations). Run after non-trivial
  changes to `gateway/`, `auth/`, `policy/`, `internal/`.
- **integration-test-author** (green) — writes integration tests with real
  MCP SDK peers (and miniredis), matching the existing suite's fixture style.
- **docs-sync** (blue) — audits README / docs/ / schema / example config for
  drift after a behavior change, before commit. Driven by `/update-docs`.
- **bench-profiler** (yellow) — read-only latency diagnosis: runs the
  added-latency gate, stash-bisects for a baseline, profiles with pprof,
  and localizes hot-path allocations. Driven by `/perf`.
- **security-auditor** (purple) — read-only review against
  `docs/security-model.md`: inbound chain order, deny-by-default pair,
  credential confinement, tenant isolation, SSRF/parser surface, audit
  completeness.

## Hooks (`hooks/`, wired in `settings.json`)

- `bash-guard.sh` (PreToolUse: Bash) — blocks force-pushes and
  `--no-verify`; forces an explicit approval prompt on `git tag` / tag
  pushes, since tags trigger the goreleaser release workflow.
- `post-edit.sh` (PostToolUse: Edit|Write) — runs `gofmt -w` on touched
  `.go` files; reminds about the conformance pin and the
  config.go ↔ schema lockstep when those files change.
- `session-start.sh` (SessionStart: startup|resume) — injects orientation
  into new sessions: branch/HEAD, uncommitted-work summary (with a
  leave-it-alone warning), and the latest CI conclusion via `gh`
  (best-effort, bounded).
- `stop-gate.sh` (Stop) — refuses to end a turn with gofmt-dirty files
  (fmt-check is the first CI gate), and nudges once per turn when non-test
  code changed in the working tree without any doc surface changing —
  pointing at `/update-docs`. Advisory: stating why docs are unaffected
  satisfies it.

Hooks need to stay executable (`chmod +x hooks/*.sh`) and depend on `jq`.

`settings.json` also pre-allows the read-only/gate commands (`make check`,
`go test`, `gh run watch`, …) so routine verification doesn't prompt.
Personal overrides go in `settings.local.json` (gitignored by Claude Code
convention).
