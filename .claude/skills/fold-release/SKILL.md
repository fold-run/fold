---
name: fold-release
description: Cut a fold release — verify gates, write the changelog and bump the Helm chart's appVersion, then with explicit per-step approval commit/push, watch CI, tag to trigger goreleaser, and refresh the conformance receipt. Use when the user asks to release, ship, tag, or publish a version of fold.
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

4. **Bump the version-bearing surfaces.** Source carries no version (it is
   stamped by ldflags), but two files name one and drift silently:

   - **`deploy/helm/fold/Chart.yaml` → `appVersion`.** Set it to the version
     being released, quoted and `v`-prefixed (`appVersion: "v1.8.0"`). This
     is not cosmetic: `values.yaml` ships `image.tag: ""` and the deployment
     renders `{{ .Values.image.tag | default .Chart.AppVersion }}`, so a
     stale `appVersion` is what a default `helm install` actually deploys —
     and it is also the `app.kubernetes.io/version` label on every object.
     It went unbumped from v0.4.0 through v1.7.0, which meant chart users
     were installing a pre-1.0 gateway.
   - **`deploy/helm/fold/Chart.yaml` → `version`** (the chart's own
     version), bumped on any change under `deploy/helm/` — including the
     `appVersion` line above, since it changes rendered output. Patch-level
     unless the chart's interface changed. This is the OCI tag the chart
     publishes under, and a published tag is immutable: reusing one means two
     different charts wearing the same name.

   The release workflow's `chart` job enforces the first of these — it refuses
   to publish when `appVersion` does not name the tag — so forgetting the bump
   now fails the release rather than shipping a chart that installs the wrong
   gateway. Do not treat that gate as the reminder: it fires after the tag is
   pushed, which is the expensive place to find out.

   Then re-render and confirm what a user would get:

   ```bash
   helm template fold deploy/helm/fold -f deploy/helm/fold/ci/default-values.yaml \
     | grep -E "image:|app.kubernetes.io/version" | sort -u
   make helm-check
   ```

   Nothing else needs a bump. Compose files, `deploy/fold-discovery.yaml`,
   and the docs pin `:latest` deliberately (with "pin a version in
   production" where it matters) — leave them alone rather than
   "fixing" them into stale pins.

5. **Commit and push — only on explicit say-so.** Conventional style
   matching recent history (`release: ...`). The changelog entry and the
   chart bump belong in this commit, so the tag names a tree where the
   documented version and the chart agree.

6. **Watch CI.** `gh run list` to find the run, then `gh run watch <id>`
   in the background. Report the conclusion. Do not tag until CI is green.

7. **Tag and push the tag — only on explicit say-so.**
   `git tag -a vX.Y.Z -m "..."` then `git push origin vX.Y.Z`. (A
   PreToolUse hook will surface an approval prompt on tag commands — that
   is expected, not an error.)

8. **Watch the release workflow** the same way (`gh run list` /
   `gh run watch`) and report the result of all three jobs — `binaries`
   (goreleaser: archives, SBOMs, checksums, provenance), `image` (the three
   ghcr images), and `chart` (`oci://ghcr.io/fold-run/charts/fold` at the
   chart's own version) — including the artifacts published.

   A chart-only publish for an already-released tag does not need a new
   release: run the workflow via `workflow_dispatch` with `chart_tag`, which
   checks out that tag and publishes the chart exactly as it shipped.

9. **Refresh the pinned conformance receipt.** The README's "Conformant,
   provably" paragraph links a specific green `conformance` job by run and
   job id — the receipt a skeptic clicks. Repoint it at this release's run:
   `gh run view <ci-run-id> --json jobs --jq '.jobs[] | select(.name=="conformance") | .url'`,
   then update the link and its version label in the same pass. A receipt
   naming an old version still verifies, but reads as neglect on the day
   someone challenges the 40/40 claim.

## Failure handling

- CI red after push: diagnose and report; fix commits also need per-step
  approval. Never force-push over a pushed release commit.
- Release workflow red after tag: report first. Deleting/re-pushing a tag
  is a user decision, never yours.
- **Stopping between the push and the tag is the failure mode that hides.**
  v1.7.0 sat on `main` for two days with a changelog entry claiming it had
  shipped, no tag, and therefore no release run at all — the workflow only
  fires on `v*`, so nothing was red anywhere. If a release is interrupted,
  say plainly which steps did and did not happen. To check any version:
  `git ls-remote --tags origin | grep vX.Y.Z` and
  `gh run list --workflow release.yml`; an entry in the changelog with no
  run is the signature.
- A release tagged without the chart bump leaves the chart pointing at the
  previous version. Fix forward in a follow-up commit — never retag a
  pushed release to pick up a missed edit.
