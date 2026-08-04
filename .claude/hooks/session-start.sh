#!/usr/bin/env bash
# SessionStart hook: orient the session — working-tree state, branch/HEAD,
# and latest CI conclusion. Stdout is injected into context. Fail silent
# and fast: a broken orientation must never block a session.
set -uo pipefail

cd "${CLAUDE_PROJECT_DIR:-.}" 2>/dev/null || exit 0
command -v git >/dev/null 2>&1 || exit 0

branch=$(git branch --show-current 2>/dev/null)
head=$(git log -1 --format='%h %s' 2>/dev/null)
echo "fold session start: branch ${branch:-?} @ ${head:-?}"

dirty=$(git status --porcelain 2>/dev/null | head -25)
if [[ -n "$dirty" ]]; then
  echo "Working tree has uncommitted changes (may be in-flight work from another session — leave it alone unless it is the task):"
  echo "$dirty"
else
  echo "Working tree clean."
fi

# Latest CI conclusion, best-effort. gh can hang on network; bound it.
if command -v gh >/dev/null 2>&1; then
  runner=(gh run list -L 1 --json displayTitle,status,conclusion,headBranch
          -q '.[0] | "Latest CI run (\(.headBranch)): \(.status) \(.conclusion // "") — \(.displayTitle)"')
  if command -v timeout >/dev/null 2>&1; then
    timeout 5 "${runner[@]}" 2>/dev/null || true
  elif command -v gtimeout >/dev/null 2>&1; then
    gtimeout 5 "${runner[@]}" 2>/dev/null || true
  else
    "${runner[@]}" 2>/dev/null || true
  fi
fi

exit 0
