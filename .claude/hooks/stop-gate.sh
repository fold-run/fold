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

unformatted=$(gofmt -l . 2>/dev/null)
if [[ -n "$unformatted" ]]; then
  echo "gofmt needed on: ${unformatted//$'\n'/, } — run 'make fmt' before finishing (CI gates on fmt-check)." >&2
  exit 2
fi

exit 0
