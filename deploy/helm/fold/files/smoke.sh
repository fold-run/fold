#!/usr/bin/env sh
# Post-deploy smoke test for a running fold: the handshake a real client makes,
# then the two operational endpoints. Exits non-zero on the first thing that
# is not as a healthy gateway would answer it. POSIX sh and curl only, so it
# runs unchanged from a laptop, a CI job, a compose service, or the chart's
# `helm test` pod.
#
#   scripts/smoke.sh URL [--host HOST] [--token TOKEN] [--tool NAME] [--metrics URL]
#
#   URL        base URL of the gateway (http://localhost:8080)
#   --host     Host header to send (must be in server.allowedHosts); default: URL's host
#   --token    bearer token for an auth-required gateway
#   --tool     a tool to call with empty arguments after tools/list (optional)
#   --metrics  where /metrics lives when metricsAddr moved it (default: URL)
#
# Environment: SMOKE_TIMEOUT (seconds per request, default 10); SMOKE_TOKEN (same as --token).
set -eu

usage() { sed -n '2,15p' "$0" >&2; exit 2; }
[ $# -ge 1 ] || usage
BASE=${1%/}; shift
HOST=""; TOKEN=${SMOKE_TOKEN:-}; TOOL=""; METRICS=""
while [ $# -gt 0 ]; do
  case "$1" in
    --host) HOST=$2; shift 2 ;;
    --token) TOKEN=$2; shift 2 ;;
    --tool) TOOL=$2; shift 2 ;;
    --metrics) METRICS=$2; shift 2 ;;
    *) usage ;;
  esac
done
METRICS=${METRICS:-$BASE}
T=${SMOKE_TIMEOUT:-10}
MCP="$BASE/mcp"

fail() { echo "smoke: FAIL: $*" >&2; exit 1; }
step() { echo "smoke: $*"; }

HDRS=$(mktemp); trap 'rm -f "$HDRS"' EXIT
# mcp_post BODY [curl args...]: POST to /mcp with the common headers; prints
# the body and stores the response headers in $HDRS.
mcp_post() {
  body=$1; shift
  set -- "$@" -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream'
  [ -n "$HOST" ] && set -- "$@" -H "Host: $HOST"
  [ -n "$TOKEN" ] && set -- "$@" -H "Authorization: Bearer $TOKEN"
  curl -sS --max-time "$T" -o - -D "$HDRS" "$@" -X POST --data "$body" "$MCP"
}
# get URL: GET with the Host header when one is set; prints "<code> <body>".
get() {
  if [ -n "$HOST" ]; then
    curl -sS --max-time "$T" -w '\n%{http_code}' -H "Host: $HOST" "$1"
  else
    curl -sS --max-time "$T" -w '\n%{http_code}' "$1"
  fi
}

# 1. initialize
step "initialize $MCP"
INIT=$(mcp_post '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"fold-smoke","version":"1"}}}') \
  || fail "initialize request failed"
case "$INIT" in
  *'"serverInfo"'*|*'"protocolVersion"'*) ;;
  *) fail "initialize did not return a server: $INIT" ;;
esac
SID=$(awk 'tolower($1)=="mcp-session-id:"{print $2}' "$HDRS" | tr -d '\r')
[ -n "$SID" ] || fail "no Mcp-Session-Id header on initialize"
step "session $SID"

# 2. initialized notification (a well-behaved client sends it)
mcp_post '{"jsonrpc":"2.0","method":"notifications/initialized"}' -H "Mcp-Session-Id: $SID" >/dev/null || fail "initialized notification refused"

# 3. tools/list
step "tools/list"
LIST=$(mcp_post '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' -H "Mcp-Session-Id: $SID") || fail "tools/list request failed"
case "$LIST" in
  *'"tools"'*) ;;
  *'"error"'*) fail "tools/list answered an error: $LIST" ;;
  *) fail "tools/list returned no tools array: $LIST" ;;
esac
COUNT=$(printf '%s' "$LIST" | grep -o '"name"' | wc -l | tr -d ' ')
step "tools/list ok ($COUNT tool names)"

# 4. optional tools/call
if [ -n "$TOOL" ]; then
  step "tools/call $TOOL"
  CALL=$(mcp_post "{\"jsonrpc\":\"2.0\",\"id\":3,\"method\":\"tools/call\",\"params\":{\"name\":\"$TOOL\",\"arguments\":{}}}" -H "Mcp-Session-Id: $SID") || fail "tools/call request failed"
  case "$CALL" in
    *'"error"'*) fail "tools/call $TOOL answered an error: $CALL" ;;
    *'"content"'*|*'"structuredContent"'*) step "tools/call ok" ;;
    *) fail "tools/call $TOOL returned no content: $CALL" ;;
  esac
fi

# 5. end the session
if [ -n "$HOST" ]; then
  curl -sS --max-time "$T" -o /dev/null -H "Host: $HOST" ${TOKEN:+-H "Authorization: Bearer $TOKEN"} -H "Mcp-Session-Id: $SID" -X DELETE "$MCP" || true
else
  curl -sS --max-time "$T" -o /dev/null ${TOKEN:+-H "Authorization: Bearer $TOKEN"} -H "Mcp-Session-Id: $SID" -X DELETE "$MCP" || true
fi

# 6. /health — 200 is the pass; a 503 still means the process is serving but
#    is reported as degraded and fails the smoke.
step "GET $BASE/health"
HOUT=$(get "$BASE/health") || fail "/health unreachable"
HCODE=$(printf '%s' "$HOUT" | tail -n 1)
[ "$HCODE" = "200" ] || fail "/health answered $HCODE (degraded: no probeable upstream reachable, or discovery has not applied): $(printf '%s' "$HOUT" | head -n 1 | cut -c1-300)"

# 7. /metrics
step "GET $METRICS/metrics"
METRICS_BODY=$(get "$METRICS/metrics") || fail "/metrics unreachable"
case "$METRICS_BODY" in
  *fold_build_info*) ;;
  *) fail "/metrics did not expose fold_build_info" ;;
esac

echo "smoke: PASS"
