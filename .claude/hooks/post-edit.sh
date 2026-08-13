#!/usr/bin/env bash
# PostToolUse hook for Edit|Write. Keeps touched Go files gofmt-clean
# (fmt-check is a CI gate) and flags edits to files with lockstep or
# pinning invariants. Exit 2 feeds stderr back to Claude (the edit has
# already happened; this is guidance, not a block).
set -uo pipefail

file=$(jq -r '.tool_input.file_path // empty' 2>/dev/null) || exit 0
[[ -z "$file" ]] && exit 0

# Order matters: specific files first, then the generic *.go case.
case "$file" in
  */config/config.go)
    command -v gofmt >/dev/null 2>&1 && [[ -f "$file" ]] && gofmt -w "$file"
    echo "config surface touched: config/config.go and config/fold.config.schema.json must stay in lockstep (the schema drift test enforces it), and new fields fall under the v1 compatibility contract (README 'API stability'). Update both sides plus docs/configuration.md." >&2
    exit 2
    ;;
  */scripts/conformance.sh)
    echo "conformance.sh touched: CONFORMANCE_COMMIT/CONFORMANCE_PKG are pinned deliberately and CI gates on 40/40 checks. If you changed the pin, run 'make conformance' locally and note the bump in the commit message." >&2
    exit 2
    ;;
  */config/fold.config.schema.json)
    echo "config surface touched: config/config.go and config/fold.config.schema.json must stay in lockstep (the schema drift test enforces it), and new fields fall under the v1 compatibility contract (README 'API stability'). Update both sides plus docs/configuration.md." >&2
    exit 2
    ;;
  *.go)
    if command -v gofmt >/dev/null 2>&1 && [[ -f "$file" ]]; then
      gofmt -w "$file"
    fi
    ;;
esac

exit 0
