#!/usr/bin/env bash
# PostToolUse hook for Edit|Write. Keeps touched Go files gofmt-clean
# (fmt-check is a CI gate) and flags edits to files with lockstep or
# pinning invariants. Exit 2 feeds stderr back to Claude (the edit has
# already happened; this is guidance, not a block).
set -uo pipefail

file=$(jq -r '.tool_input.file_path // empty' 2>/dev/null) || exit 0
[[ -z "$file" ]] && exit 0

fmt() { command -v gofmt >/dev/null 2>&1 && [[ -f "$file" ]] && gofmt -w "$file"; return 0; }

schema_msg="config surface touched: config/config.go and config/fold.config.schema.json must stay in lockstep (the schema drift test enforces it), and new fields fall under the v1 compatibility contract (README 'API stability'). Update both sides plus docs/configuration.md."

# Order matters: specific files first, then the generic *.go case.
case "$file" in
  */config/config.go)
    fmt
    echo "$schema_msg" >&2
    exit 2
    ;;
  */config/fold.config.schema.json)
    echo "$schema_msg" >&2
    exit 2
    ;;
  */gateway/metrics.go)
    fmt
    echo "observability pack lockstep: gateway/observability_pack_test.go checks metric names in BOTH directions against deploy/helm/fold/dashboards/fold-overview.json, deploy/helm/fold/templates/prometheusrule.yaml, and deploy/observability/alerts.yml. A new fold_* metric must land in the pack (or earn a line in metricNamesUnexercised); a renamed one silently empties every panel and disarms every alert that names it. Metric names are frozen by the v1 contract (README 'API stability'). Update docs/operations.md, then: go test ./gateway -run 'TestPackReferencesOnlyDeclaredMetrics|TestEveryMetricAppearsInThePack'" >&2
    exit 2
    ;;
  */deploy/helm/fold/dashboards/*|*/deploy/helm/fold/templates/prometheusrule.yaml|*/deploy/observability/alerts.yml)
    echo "observability pack touched: every fold_* name here must be declared in gateway/metrics.go — observability_pack_test.go enforces it both ways, and the two rule files (prometheusrule.yaml for the prometheus-operator, deploy/observability/alerts.yml for the compose stack) must carry the same alerts. Run: go test ./gateway -run 'TestPackReferencesOnlyDeclaredMetrics|TestEveryMetricAppearsInThePack|TestDashboardIsWellFormed|TestBothRuleFilesCarryTheSameAlerts'" >&2
    exit 2
    ;;
  */deploy/helm/fold/Chart.yaml)
    echo "chart version surface: two lines, two jobs. 'version' is the chart's own, published as an immutable OCI tag — bump it on ANY change under deploy/helm/, since reusing a tag means two different charts wearing one name. 'appVersion' is the gateway a default install actually deploys (values.yaml ships image.tag: \"\" and the deployment falls back to .Chart.AppVersion) and it labels every rendered object; the release workflow's chart job refuses to publish when it does not name the tag, and gateway/release_pins_test.go holds compose.yaml and deploy/fold-discovery.yaml's image tags equal to it. Then: make helm-check" >&2
    exit 2
    ;;
  */deploy/helm/fold/*)
    echo "Helm chart touched: run 'make helm-check' (lint + render against every deploy/helm/fold/ci/*.yaml + the required-config guard). Bump 'version' in Chart.yaml — a published OCI chart tag is immutable. A new or changed value also needs deploy/helm/fold/README.md and docs/deploy.md." >&2
    exit 2
    ;;
  */scripts/conformance.sh)
    echo "conformance.sh touched: CONFORMANCE_COMMIT/CONFORMANCE_PKG are pinned deliberately and CI gates on 40/40 checks. If you changed the pin, run 'make conformance' locally and note the bump in the commit message." >&2
    exit 2
    ;;
  */scripts/sync-console.sh)
    echo "console pin touched: CONSOLE_COMMIT is a supply-chain decision, not a version bump — the console runs same-origin with a live Bearer token in the operator's browser. Bumping it means re-vendoring ('make sync-console'), a security-auditor pass over the vendored diff, and 'make console-check'. CI separately asserts CONSOLE_REPO still points at fold-run/fold-console, so repointing it is a reviewed change too. See /console-sync." >&2
    exit 2
    ;;
  *.go)
    fmt
    # A minted error code is a public contract: README's table is canonical
    # and nothing in the test suite enforces it. Only speak up on real drift,
    # so this stays silent on the many edits that merely touch the file.
    case "$file" in
      */gateway/*)
        if grep -q -- '-3104' "$file" 2>/dev/null; then
          root="${CLAUDE_PROJECT_DIR:-.}"
          # Plain globs, not grep --include/--exclude: those are GNU-only
          # (ugrep treats them as filenames) and would recurse into the
          # vendored console tree for nothing. [[ ]] rather than a nested
          # case, whose ')' bash mis-parses inside command substitution.
          in_code=$(for f in "$root"/gateway/*.go; do
                      [[ "$f" == *_test.go ]] && continue
                      grep -ohE -- '-3104[0-9]' "$f" 2>/dev/null
                    done | sort -u)
          in_docs=$(grep -ohE -- '-3104[0-9]' "$root/README.md" 2>/dev/null | sort -u)
          if [[ -n "$in_code" && "$in_code" != "$in_docs" ]]; then
            echo "error-code registry drift: the codes minted in gateway/ ($(tr '\n' ' ' <<<"$in_code")) do not match the codes README's 'Errors' table documents ($(tr '\n' ' ' <<<"$in_docs")). That table is canonical and no test enforces it — CLAUDE.md says the list moves with it. Update README, and check docs/configuration.md and the roadmap if the new code names a new refusal." >&2
            exit 2
          fi
        fi
        ;;
    esac
    ;;
esac

exit 0
