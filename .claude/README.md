# Claude Code setup for fold

Project-level Claude Code configuration: skills, subagents, and hooks
tailored to this repo's invariants (see CLAUDE.md for the invariants
themselves). Checked in so every contributor's Claude session gets the
same guardrails.

## Skills (`skills/`) — invocable as slash commands

| Skill | Use |
| --- | --- |
| `/preflight` | Run the local merge gate in order (`make check`, plus the conditional gates the diff warrants: bench, conformance, helm-check, vuln, console-check, image build) and interpret failures. |
| `/fold-release` | The release workflow: verify → per-step-approved commit/push → CI watch → tag (goreleaser) → CHANGELOG.md entry. |
| `/helm-chart` | Chart changes: the three `ci/*.yaml` render shapes, the probe Host-header trap, `Chart.yaml`'s two version lines, the docs that track values. |
| `/stdio-bridge` | `fold-stdio` and `internal/stdiobridge`: the protocol-blind pump, the argv-only command rule, the narrow-by-default flags. |
| `/discovery-controller` | `fold-discovery` and `internal/kubediscovery`: poll-to-poll with no client-go, and the allowlist flags that bound what a Service may propose. |
| `/deps` | govulncheck triage (including red with no commit behind it), dependabot review across three ecosystems, pin bumps. |
| `/roadmap` | Grooming README "Not implemented" and `docs/roadmap.md`: the four vetoes, converting a request into a non-goal, and the drift-canary pattern. |
| `/reloadable-state` | Checklist for adding config/state that must survive hot reload: snapshot placement, schema lockstep, reload/churn test matrix. |
| `/conformance` | Run, debug, or deliberately bump the pinned MCP conformance suite. |
| `/update-docs` | Map the working diff to the doc surfaces that track it and fix drift via the docs-sync agent. The Stop hook nudges toward this when code changes without doc changes. |
| `/perf` | Proxy-path latency work: measure via the bench-profiler agent, apply the move-to-snapshot-build fix pattern, verify with bench + race. |
| `/watch-ci` | Post-push CI watching: find the run, watch in background, triage failures back to local make targets. |
| `/mcp-spec` | Look up the normative spec text for a method, field, header, or error code before changing wire behavior — pinned to revision `2026-07-28`. |
| `/mcp-inspector` | Drive the official Inspector CLI against a running fold to see what a real client sees: the invisibility diff, namespacing round-trip, policy pair, `ui://` reads. |
| `/spec-drift` | Compare a new protocol revision against what fold implements and land each gap in code, README "Not implemented", or the roadmap. |
| `/console-sync` | The vendored `gateway/console` assets: reviewing a weekly bump PR as a supply-chain change, the `console-check` gate, the manifest allowlist, and failure triage. |

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
- **deploy-reviewer** (orange) — read-only review of the *rendered* chart
  across all three ci shapes, plus Kubernetes security posture, the three
  Dockerfiles, and the compose stack. Templates that parse can still render
  a Deployment that never becomes ready.
- **release-verifier** (pink) — verifies a published release from an
  operator's position: archives, checksums and their keyless cosign
  signature, SBOMs, the three ghcr images, the OCI chart, and every
  sigstore attestation — plus that the binary, chart `appVersion`, and
  CHANGELOG agree on the version.
- **flake-triage** (green) — reproduces an intermittent or race-detector
  failure, localizes the shared state behind it, and names the real fix.
  Read-only on purpose: the tempting fixes here (a sleep, a longer timeout,
  a skip) are the wrong ones, and an agent that could apply them would.
- **mcp-spec-auditor** (cyan) — read-only audit against the MCP
  specification itself rather than fold's own invariants: error-code
  allocation, required fields surviving the list merge, the caching and
  opacity rules that bind fold as a *shared intermediary*, and deprecations
  on the clock. Driven by `/spec-drift`; grounds every finding in fetched
  spec text.

## Hooks (`hooks/`, wired in `settings.json`)

- `bash-guard.sh` (PreToolUse: Bash) — blocks force-pushes and
  `--no-verify`; forces an explicit approval prompt on `git tag` / tag
  pushes, since tags trigger the goreleaser release workflow, and on
  commands that discard work git cannot recover (`reset --hard`,
  `checkout --`/`.`, `restore`, `clean -f`, `stash`). The last of these
  prompts rather than blocks — `bench-profiler` stash-bisects on purpose —
  and names the number of uncommitted paths at risk, which is the fact
  `session-start.sh` warns about and nothing else enforced.
- `pre-edit-guard.sh` (PreToolUse: Edit|Write) — **blocks** writes to
  `gateway/console/**` and `gateway/console_source.go`. Both are vendored
  from fold-run/fold-console; CI's `console` job re-vendors from the pin and
  fails on any difference, so an edit there cannot merge. Blocking at the
  write is cheaper than learning it from a red PR.
- `post-edit.sh` (PostToolUse: Edit|Write) — runs `gofmt -w` on touched
  `.go` files, and speaks up on the repo's lockstep surfaces:
  config.go ↔ schema, `gateway/metrics.go` ↔ the packaged dashboard and
  both alert files, `deploy/helm/fold/**` (chart `version` vs `appVersion`,
  `make helm-check`, the docs that track values), the conformance pin, and
  the console pin as a supply-chain change. It also compares the `-3104x`
  codes minted under `gateway/` against README's canonical Errors table —
  the one lockstep here with no test behind it — and stays silent unless
  they actually diverge.
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

`settings.json` also pre-allows the read-only and gate commands the skills
and subagents actually run — read-only git (`git diff`, `status`, `log`,
`show`), every `make` gate, `helm lint`/`template`, `go tool`, read-only
`gh`, and the Inspector CLI — so routine verification doesn't prompt. The
review agents work from the working diff, so `git diff` prompting was the
single largest source of friction in the set.

Personal overrides go in `settings.local.json` (gitignored by Claude Code
convention).
