#!/usr/bin/env bash
# Stop hook: don't end a turn leaving unformatted Go files behind —
# gofmt (fmt-check) is the first CI gate. Cheap on purpose; the full
# gate is 'make check', run deliberately, not on every stop.
set -uo pipefail

input=$(cat)

# If we already blocked this stop once, let it through (no loops).
if jq -e '.stop_hook_active == true' <<<"$input" >/dev/null 2>&1; then
  exit 0
fi

cd "${CLAUDE_PROJECT_DIR:-.}" 2>/dev/null || exit 0
command -v gofmt >/dev/null 2>&1 || exit 0

msgs=()

unformatted=$(gofmt -l . 2>/dev/null)
if [[ -n "$unformatted" ]]; then
  msgs+=("gofmt needed on: ${unformatted//$'\n'/, } — run 'make fmt' before finishing (CI gates on fmt-check).")
fi

# Docs-drift nudge: behavior-relevant code changed in the working tree but
# no doc surface did. Advisory — either run /update-docs (docs-sync agent)
# or state why docs are unaffected, then stop again.
if command -v git >/dev/null 2>&1; then
  changed=$( (git diff --name-only HEAD; git ls-files --others --exclude-standard) 2>/dev/null | sort -u)
  code_changed=$(grep -E '^(gateway|config|auth|policy|audit|cmd|internal)/.*\.go$' <<<"$changed" | grep -v '_test\.go$' || true)
  docs_changed=$(grep -E '(\.md$|^docs/|fold\.config\.schema\.json$|fold\.config\.example\.json$)' <<<"$changed" | grep -v '^\.claude/' || true)
  if [[ -n "$code_changed" ]] && [[ -z "$docs_changed" ]]; then
    msgs+=("Docs-drift check: non-test code changed (${code_changed//$'\n'/, }) but no doc surface did. If behavior changed, run /update-docs (docs-sync agent); if docs are genuinely unaffected, say so and finish.")
  fi
fi

if [[ ${#msgs[@]} -gt 0 ]]; then
  printf '%s\n' "${msgs[@]}" >&2
  exit 2
fi

exit 0
