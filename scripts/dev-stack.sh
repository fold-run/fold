#!/usr/bin/env bash
# Runs fold locally with a real MCP upstream behind it, so a client — your
# editor, the Inspector, a Claude Code session via .mcp.json — can talk to
# the gateway you are working on.
#
#   scripts/dev-stack.sh up     # start, wait for health, print the URL
#   scripts/dev-stack.sh down   # stop
#   scripts/dev-stack.sh status
#
# Requires: go, node/npx. Ports are overridable:
#   GATEWAY_PORT=9099 UPSTREAM_PORT=9098 scripts/dev-stack.sh up
#
# The upstream is the reference everything-server in streamableHttp mode —
# the same server the conformance suite fronts, so what you see here and what
# CI gates on are the same peer. It is federated under the namespace `demo`
# rather than passthrough, deliberately: namespacing is the behavior most
# worth seeing from a client's side (`demo__echo`, not `echo`).
set -euo pipefail

UPSTREAM_PORT="${UPSTREAM_PORT:-8098}"
GATEWAY_PORT="${GATEWAY_PORT:-8099}"
UPSTREAM_PKG="${UPSTREAM_PKG:-@modelcontextprotocol/server-everything}"

root="$(cd "$(dirname "$0")/.." && pwd)"
work="${TMPDIR:-/tmp}/fold-dev-stack"
mkdir -p "$work"

wait_for() { # url, what — any HTTP response counts as listening
  for _ in $(seq 1 60); do
    curl -s -o /dev/null --max-time 2 "$1" && return 0
    sleep 1
  done
  echo "timed out waiting for $2 at $1" >&2
  return 1
}

port_busy() { lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; }

stop_one() { # pidfile, what
  [[ -f "$1" ]] || return 0
  local pid; pid=$(cat "$1")
  if kill "$pid" 2>/dev/null; then echo "stopped $2 (pid $pid)"; fi
  rm -f "$1"
}

case "${1:-up}" in
  down)
    stop_one "$work/gateway.pid" fold
    stop_one "$work/upstream.pid" everything-server
    exit 0
    ;;

  status)
    if curl -s --max-time 2 "http://127.0.0.1:$GATEWAY_PORT/health"; then
      echo
    else
      echo "no gateway answering on 127.0.0.1:$GATEWAY_PORT"
    fi
    exit 0
    ;;

  up) ;;
  *) echo "usage: $0 [up|down|status]" >&2; exit 2 ;;
esac

# Ports first: 8080 is a popular squat (Docker Desktop takes it by default),
# which is why this stack does not use the gateway's own default port.
for p in "$UPSTREAM_PORT" "$GATEWAY_PORT"; do
  if port_busy "$p"; then
    echo "port $p is already in use — set UPSTREAM_PORT/GATEWAY_PORT to something free" >&2
    lsof -nP -iTCP:"$p" -sTCP:LISTEN 2>/dev/null | tail -n +2 >&2
    exit 1
  fi
done

echo "--- starting everything-server on :$UPSTREAM_PORT"
PORT="$UPSTREAM_PORT" nohup npx -y "$UPSTREAM_PKG" streamableHttp > "$work/upstream.log" 2>&1 &
echo $! > "$work/upstream.pid"
wait_for "http://127.0.0.1:$UPSTREAM_PORT/mcp" everything-server

echo "--- building fold"
(cd "$root" && go build -o "$work/fold" ./cmd/fold)

# Generated rather than checked in, like the conformance harness's config: it
# names ports that are overridable, so a committed copy would be a second
# place for them to disagree.
cat > "$work/fold.dev.json" <<EOF
{
  "upstreams": [
    { "id": "everything", "url": "http://127.0.0.1:$UPSTREAM_PORT/mcp", "namespace": "demo" }
  ],
  "server": {
    "allowedHosts": ["localhost", "127.0.0.1"],
    "introspection": { "enabled": true },
    "console": { "enabled": true }
  },
  "audit": { "sinks": [ { "type": "stdout" } ] }
}
EOF

echo "--- starting fold on :$GATEWAY_PORT"
nohup "$work/fold" --config "$work/fold.dev.json" --port "$GATEWAY_PORT" > "$work/fold.log" 2>&1 &
echo $! > "$work/gateway.pid"
wait_for "http://127.0.0.1:$GATEWAY_PORT/health" fold

# /health answers 503 with a body when an upstream is down, so read the body
# rather than trusting the status: "listening" and "ready" are not the same
# claim, and this stack exists to show the difference.
status=$(curl -s "http://127.0.0.1:$GATEWAY_PORT/health")
echo "$status"
case "$status" in
  *'"status":"ok"'*) ;;
  *) echo "gateway is up but degraded — see $work/fold.log and $work/upstream.log" >&2 ;;
esac

cat <<EOF

fold is serving at  http://127.0.0.1:$GATEWAY_PORT/mcp
  console           http://127.0.0.1:$GATEWAY_PORT/console/
  health            http://127.0.0.1:$GATEWAY_PORT/health
  logs              $work/fold.log , $work/upstream.log

Tools arrive namespaced (demo__echo, not echo). See what a real client sees:
  npx -y @modelcontextprotocol/inspector --cli http://127.0.0.1:$GATEWAY_PORT/mcp \\
    --transport http --method tools/list

Stop with: scripts/dev-stack.sh down
EOF
