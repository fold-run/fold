#!/usr/bin/env bash
# PreToolUse hook for Bash. Blocks git usage that bypasses this repo's
# review workflow, forces an explicit approval prompt on release steps (tag
# creation / tag pushes trigger goreleaser via release.yml), and on commands
# that discard work git cannot get back.
#
# Exit 2  -> block the tool call; stderr is fed back to Claude.
# JSON on stdout with permissionDecision "ask" -> prompt the user first.
set -uo pipefail

cmd=$(jq -r '.tool_input.command // empty' 2>/dev/null) || exit 0
[[ -z "$cmd" ]] && exit 0

# Force-pushes rewrite history on origin; never wanted here.
if grep -Eq 'git +push[^|;&]*( --force([ =]|$)| -f( |$))' <<<"$cmd" \
   && ! grep -q 'force-with-lease' <<<"$cmd"; then
  echo "Blocked: force-push. fold history on origin is append-only. If a rewrite is truly needed, ask the user and use --force-with-lease." >&2
  exit 2
fi

# --no-verify skips the gates CI would catch anyway; run make check instead.
if grep -Eq 'git +(commit|push)[^|;&]*--no-verify' <<<"$cmd"; then
  echo "Blocked: --no-verify. Run 'make check' and let the gates run." >&2
  exit 2
fi

# Discarding the working tree, or history that is not pushed. SessionStart
# warns that uncommitted changes may be in-flight work from another session;
# nothing else stands between that warning and a reset that takes it. This
# asks rather than blocks: bench-profiler stash-bisects deliberately, and a
# reset is sometimes right — it is just never right silently.
destructive=0
grep -Eq '(^|[[:space:];&|])git +reset[^|;&]*--hard' <<<"$cmd" && destructive=1
grep -Eq '(^|[[:space:];&|])git +checkout +(-f([[:space:]]|$)|--force([[:space:]]|$)|--([[:space:]]|$)|\.([[:space:]]|$))' <<<"$cmd" && destructive=1
grep -Eq '(^|[[:space:];&|])git +restore([[:space:]]|$)' <<<"$cmd" && destructive=1
grep -Eq '(^|[[:space:];&|])git +clean[^|;&]*[[:space:]]-[a-zA-Z]*f' <<<"$cmd" && destructive=1
if grep -Eq '(^|[[:space:];&|])git +stash([[:space:]]|$)' <<<"$cmd" \
   && ! grep -Eq 'git +stash +(pop|list|show|apply|drop|clear|branch)' <<<"$cmd"; then
  destructive=1
fi

if [[ $destructive -eq 1 ]]; then
  tree=""
  if command -v git >/dev/null 2>&1; then
    n=$(cd "${CLAUDE_PROJECT_DIR:-.}" 2>/dev/null && git status --porcelain 2>/dev/null | wc -l | tr -d ' ')
    [[ -n "$n" && "$n" != "0" ]] && tree=" The working tree has $n uncommitted path(s) right now, which may be in-flight work from another session."
  fi
  reason="Discards working-tree changes or unpushed commits that git cannot recover.${tree} Confirm what is being dropped belongs to this task. (The bench-profiler baseline workflow stashes on purpose — that prompt is expected.)"
  jq -n --arg r "$reason" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "ask", permissionDecisionReason: $r}}'
  exit 0
fi

# Tags are releases: pushing vX.Y.Z runs goreleaser. Always surface an
# explicit approval prompt, even if a broader Bash permission would allow it.
if grep -Eq '(^|[[:space:];&|])git +tag( |$)' <<<"$cmd" \
   || grep -Eq 'git +push[^|;&]*(--tags|[[:space:]]v[0-9])' <<<"$cmd"; then
  jq -n '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "ask", permissionDecisionReason: "Release step: creating or pushing a vX.Y.Z tag triggers the goreleaser release workflow. Each release step needs explicit approval, with the CHANGELOG.md entry already in place."}}'
  exit 0
fi

exit 0
