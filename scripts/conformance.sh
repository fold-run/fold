#!/usr/bin/env bash
# Runs the official MCP conformance suite against fold-go fronting the
# conformance repo's everything-server (passthrough mode). Used locally and
# as the CI conformance gate.
#
# Requires: go, node/npm/npx, git.
set -euo pipefail

# Pinned for reproducible CI runs; bump deliberately. The scheduled drift
# workflow overrides COMMIT=main and PKG=@latest to catch upstream movement.
CONFORMANCE_REPO=https://github.com/modelcontextprotocol/conformance.git
CONFORMANCE_COMMIT="${CONFORMANCE_COMMIT:-81eb1c3edaed87d7fd585d7b80186da7a2960660}"
CONFORMANCE_PKG="${CONFORMANCE_PKG:-@modelcontextprotocol/conformance@0.1.16}"

UPSTREAM_PORT="${UPSTREAM_PORT:-3901}"
GATEWAY_PORT="${GATEWAY_PORT:-3902}"

root="$(cd "$(dirname "$0")/.." && pwd)"
work="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/fold-conformance"
mkdir -p "$work"

cleanup() {
  [[ -n "${UPSTREAM_PID:-}" ]] && kill "$UPSTREAM_PID" 2>/dev/null || true
  [[ -n "${GATEWAY_PID:-}" ]] && kill "$GATEWAY_PID" 2>/dev/null || true
}
trap cleanup EXIT

wait_for() { # url, what — any HTTP response counts as up
  for _ in $(seq 1 60); do
    curl -s -o /dev/null --max-time 2 "$1" && return 0
    sleep 1
  done
  echo "timed out waiting for $2 at $1" >&2
  return 1
}

echo "--- fetching conformance repo @ ${CONFORMANCE_COMMIT:0:12}"
if [[ ! -d "$work/conformance" ]]; then
  git clone --quiet "$CONFORMANCE_REPO" "$work/conformance"
fi
git -C "$work/conformance" fetch --quiet origin "$CONFORMANCE_COMMIT"
git -C "$work/conformance" checkout --quiet "$CONFORMANCE_COMMIT"

echo "--- installing everything-server deps"
(cd "$work/conformance/examples/servers" && npm install --silent --no-audit --no-fund)

echo "--- starting everything-server on :$UPSTREAM_PORT"
(cd "$work/conformance/examples/servers" && PORT="$UPSTREAM_PORT" npx tsx typescript/everything-server.ts > "$work/everything.log" 2>&1) &
UPSTREAM_PID=$!
wait_for "http://127.0.0.1:$UPSTREAM_PORT/mcp" everything-server

echo "--- building and starting fold gateway on :$GATEWAY_PORT"
(cd "$root" && go build -o "$work/fold" ./cmd/fold)
cat > "$work/fold.conformance.json" <<EOF
{ "upstreams": [ { "id": "everything", "url": "http://127.0.0.1:$UPSTREAM_PORT/mcp" } ] }
EOF
"$work/fold" --config "$work/fold.conformance.json" --port "$GATEWAY_PORT" > "$work/fold.log" 2>&1 &
GATEWAY_PID=$!
wait_for "http://127.0.0.1:$GATEWAY_PORT/healthz" fold-gateway

echo "--- running conformance suite through the gateway"
npx -y "$CONFORMANCE_PKG" server --url "http://127.0.0.1:$GATEWAY_PORT/mcp"
