---
name: fold-release
description: Cut a fold release — verify gates, get explicit per-step approval, commit/push, watch CI, tag to trigger goreleaser, then add the README changelog entry. Use when the user asks to release, ship, tag, or publish a version of fold.
---

# fold release workflow

Releases are annotated-tag-driven: pushing `vX.Y.Z` triggers
`.github/workflows/release.yml` (goreleaser). The version string is stamped
via `-ldflags "-X github.com/fold-run/fold/gateway.version=..."` — goreleaser
handles this; never hardcode a version in source.

**Cardinal rule: every git-touching step needs the user's explicit,
per-step approval.** "Release it" authorizes the *process*, not each push.
Finish and report each step, then wait for the go-ahead before the next.
Never batch commit+push+tag on a single approval.

## Steps

1. **Verify before anything touches git.** Run the full gate:
   - `make check` (fmt-check, tidy-check, vet, build, race, lint)
   - `make bench` if the proxy path changed (gate: added p50 < 5 ms)
   - `make conformance` if behavior through the gateway changed (40/40)
   - `make helm-check` if `deploy/helm/` changed
   Report results with real output. Stop here if anything fails.

2. **Pick the version.** The repo is under the v1 compatibility contract
   (README "API stability"): patch for fixes, minor for
   backwards-compatible features, major only for a deliberate contract
   break (which needs its own conversation). Confirm the number with the
   user.

3. **Write the changelog entry** at the top of README `## Changelog`:
   `### vX.Y.Z — YYYY-MM-DD`, leading with a one-line theme, then feature
   bullets. Match the register of existing entries. This lands in the
   release commit, before the tag.

4. **Commit and push — only on explicit say-so.** Conventional style
   matching recent history (`release: ...`).

5. **Watch CI.** `gh run list` to find the run, then `gh run watch <id>`
   in the background. Report the conclusion. Do not tag until CI is green.

6. **Tag and push the tag — only on explicit say-so.**
   `git tag -a vX.Y.Z -m "..."` then `git push origin vX.Y.Z`. (A
   PreToolUse hook will surface an approval prompt on tag commands — that
   is expected, not an error.)

7. **Watch the release workflow** the same way (`gh run list` /
   `gh run watch`) and report the goreleaser result, including the
   artifacts published.

## Failure handling

- CI red after push: diagnose and report; fix commits also need per-step
  approval. Never force-push over a pushed release commit.
- Release workflow red after tag: report first. Deleting/re-pushing a tag
  is a user decision, never yours.
