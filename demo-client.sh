#!/usr/bin/env bash
# Runs as the "inner command" of `omac start --no-sandbox`. Stands in for
# the agent inside a real sandbox. Hits the echo-rest sidecar via the
# bridge Unix socket.
set -euo pipefail

echo "=============================================================="
echo " demo-client inside omac start"
echo "=============================================================="
echo "OMAC_SOCKET    = ${OMAC_SOCKET:-<unset>}"
echo "OMAC_SKILLS    = ${OMAC_SKILLS:-<unset>}"
echo "OMAC_ECHO_BASE = ${OMAC_ECHO_BASE:-<unset>}"
echo "--- the sandbox MUST NOT see the host secret: ---"
echo "ECHO_API_KEY in my env? $([[ -n "${ECHO_API_KEY:-}" ]] && echo LEAKED || echo absent-as-expected)"
echo "=============================================================="

if [[ -z "${OMAC_SOCKET:-}" ]]; then
  echo "FAIL: OMAC_SOCKET not set" >&2
  exit 1
fi

echo
echo "--- GET /echo/status (the facade routes this to the sidecar) ---"
curl -sS --unix-socket "$OMAC_SOCKET" http://x/echo/status
echo

echo
echo "--- GET /echo/whoami (proves secret injection via env) ---"
curl -sS --unix-socket "$OMAC_SOCKET" http://x/echo/whoami
echo

echo
echo "--- POST /echo/echo  {\"hello\":\"from sandbox\"} ---"
curl -sS --unix-socket "$OMAC_SOCKET" \
  -H 'Content-Type: application/json' \
  -d '{"hello":"from sandbox","n":7}' \
  http://x/echo/echo
echo

echo
echo "--- GET /  (facade status) ---"
curl -sS --unix-socket "$OMAC_SOCKET" http://x/
echo

echo
echo "--- GET /echo/tick  (Server-Sent Events stream; five frames with a gap) ---"
# -N disables curl's output buffering, so the frames print as they arrive,
# just like they would inside a sandboxed agent reading an LLM stream.
# The sidecar closes the connection after sending 'done', so curl exits
# 0 on normal SSE termination. The `|| true` guards against environments
# where the server's connection close is interpreted as a transient error
# (curl can exit 18 or 28 depending on platform); we don't want a
# successful demo to fail the script just because of a final-frame race.
{
  curl -sS -N --max-time 30 \
    --unix-socket "$OMAC_SOCKET" \
    'http://x/echo/tick?n=5&gap_ms=30' \
    || true
} | awk '
    /^event:/ { ev=$2 }
    /^id:/    { id=$2 }
    /^data:/  { sub(/^data: /,""); printf "  [%s #%s] %s\n", ev, id, $0 }
  '

echo
echo "--- negative: unknown mount must 404 ---"
curl -sS -o /tmp/omac-demo-404-body -w 'HTTP %{http_code}\n' \
  --unix-socket "$OMAC_SOCKET" http://x/nosuch/foo
cat /tmp/omac-demo-404-body
echo
