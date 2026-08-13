#!/usr/bin/env bash
# PreToolUse hook for Bash. Blocks git usage that bypasses this repo's
# review workflow, and forces an explicit approval prompt on release steps
# (tag creation / tag pushes trigger goreleaser via release.yml).
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

# Tags are releases: pushing vX.Y.Z runs goreleaser. Always surface an
# explicit approval prompt, even if a broader Bash permission would allow it.
if grep -Eq '(^|[[:space:];&|])git +tag( |$)' <<<"$cmd" \
   || grep -Eq 'git +push[^|;&]*(--tags|[[:space:]]v[0-9])' <<<"$cmd"; then
  jq -n '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "ask", permissionDecisionReason: "Release step: creating or pushing a vX.Y.Z tag triggers the goreleaser release workflow. Each release step needs explicit approval, with the CHANGELOG.md entry already in place."}}'
  exit 0
fi

exit 0
