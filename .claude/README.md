# Claude Code setup for fold

Project-level Claude Code configuration: skills, subagents, and hooks
tailored to this repo's invariants (see CLAUDE.md for the invariants
themselves). Checked in so every contributor's Claude session gets the
same guardrails.

## Skills (`skills/`) — invocable as slash commands

| Skill | Use |
| --- | --- |
| `/preflight` | Run the local merge gate in order (`make check`, plus bench / conformance / helm-check when the diff warrants) and interpret failures. |
| `/fold-release` | The release workflow: verify → per-step-approved commit/push → CI watch → tag (goreleaser) → README changelog entry. |
| `/reloadable-state` | Checklist for adding config/state that must survive hot reload: snapshot placement, schema lockstep, reload/churn test matrix. |
| `/conformance` | Run, debug, or deliberately bump the pinned MCP conformance suite. |

Claude also loads these automatically when a task matches the skill's
description.

## Subagents (`agents/`)

- **gateway-reviewer** — read-only review of the working diff against the
  gateway's invariants (pipeline order, audit exit door, snapshot state,
  invisibility, error codes, hot-path allocations). Run after non-trivial
  changes to `gateway/`, `auth/`, `policy/`, `internal/`.
- **integration-test-author** — writes integration tests with real MCP SDK
  peers (and miniredis), matching the existing suite's fixture style.
- **docs-sync** — audits README / docs/ / schema / example config for
  drift after a behavior change, before commit.

## Hooks (`hooks/`, wired in `settings.json`)

- `bash-guard.sh` (PreToolUse: Bash) — blocks force-pushes and
  `--no-verify`; forces an explicit approval prompt on `git tag` / tag
  pushes, since tags trigger the goreleaser release workflow.
- `post-edit.sh` (PostToolUse: Edit|Write) — runs `gofmt -w` on touched
  `.go` files; reminds about the conformance pin and the
  config.go ↔ schema lockstep when those files change.
- `stop-gate.sh` (Stop) — refuses to end a turn with gofmt-dirty files
  (fmt-check is the first CI gate).

Hooks need to stay executable (`chmod +x hooks/*.sh`) and depend on `jq`.

`settings.json` also pre-allows the read-only/gate commands (`make check`,
`go test`, `gh run watch`, …) so routine verification doesn't prompt.
Personal overrides go in `settings.local.json` (gitignored by Claude Code
convention).
