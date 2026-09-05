#!/usr/bin/env sh
# Post-deploy smoke test against a running fold. The script itself lives in
# the chart (deploy/helm/fold/files/smoke.sh) so `helm test` can ship the
# same bytes; this is the repo-root entry point for running it by hand:
#
#   scripts/smoke.sh http://localhost:8080 [--host HOST] [--token TOKEN] [--tool NAME]
exec "$(dirname "$0")/../deploy/helm/fold/files/smoke.sh" "$@"
