#!/usr/bin/env bash
# Runs as the "inner command" of `omac start ...` Stands in for the
# agent inside a real sandbox. Hits the echo-rest sidecar via two
# transports:
#
#   1. TCP loopback ($OMAC_ECHO_BASE = http://127.0.0.1:<port>/echo) —
#      this is the form that works under nono proxy mode (auto-activated
#      by tng-sandbox.json's custom_credentials block) thanks to
#      --open-port in the launcher profile. NB: the value has no trailing
#      slash; callers append "/<route>" themselves.
#
#   2. Unix socket ($OMAC_ECHO_SOCKET_BASE = http+unix://...) — the
#      lower-overhead form. Works in --no-sandbox runs and in nono on
#      Linux. On macOS under proxy mode, Seatbelt's `(deny network*)`
#      blocks AF_UNIX connect(2), so this path is expected to fail
#      there; we test it anyway so the failure is visible and explicit.
set -euo pipefail

echo "=============================================================="
echo " demo-client inside omac start"
echo "=============================================================="
echo "OMAC_HOST            = ${OMAC_HOST:-<unset>}"
echo "OMAC_PORT            = ${OMAC_PORT:-<unset>}"
echo "OMAC_BASE            = ${OMAC_BASE:-<unset>}"
echo "OMAC_SOCKET          = ${OMAC_SOCKET:-<unset>}"
echo "OMAC_SKILLS          = ${OMAC_SKILLS:-<unset>}"
echo "OMAC_ECHO_BASE       = ${OMAC_ECHO_BASE:-<unset>}"
echo "OMAC_ECHO_SOCKET_BASE= ${OMAC_ECHO_SOCKET_BASE:-<unset>}"
echo "--- the sandbox MUST NOT see the host secret: ---"
echo "ECHO_API_KEY in my env? $([[ -n "${ECHO_API_KEY:-}" ]] && echo LEAKED || echo absent-as-expected)"
echo "=============================================================="

if [[ -z "${OMAC_BASE:-}" ]]; then
  echo "FAIL: OMAC_BASE not set" >&2
  exit 1
fi

# Helper: print a section heading and run curl, but never abort the
# script if curl exits non-zero — we want to see all transports' results.
section() { echo; echo "--- $* ---"; }
try()     { ( "$@" ) || echo "  (command exited $?)"; }

# ──────────────────────────── TCP transport ────────────────────────────
echo
echo "############# TCP transport ($OMAC_BASE) #############"

section "GET /echo/status (TCP)"
try curl -sS "${OMAC_ECHO_BASE}/status"
echo

section "GET /echo/whoami (TCP, proves secret injection)"
try curl -sS "${OMAC_ECHO_BASE}/whoami"
echo

section "POST /echo/echo (TCP)"
try curl -sS \
  -H 'Content-Type: application/json' \
  -d '{"hello":"from sandbox","n":7}' \
  "${OMAC_ECHO_BASE}/echo"
echo

section "GET / (facade status, TCP)"
try curl -sS "${OMAC_BASE}"
echo

section "GET /echo/tick — SSE stream (TCP)"
{
  curl -sS -N --max-time 30 "${OMAC_ECHO_BASE}/tick?n=5&gap_ms=30" || true
} | awk '
    /^event:/ { ev=$2 }
    /^id:/    { id=$2 }
    /^data:/  { sub(/^data: /,""); printf "  [%s #%s] %s\n", ev, id, $0 }
  '

section "negative: unknown mount must 404 (TCP)"
curl -sS -o /tmp/omac-demo-404-body -w 'HTTP %{http_code}\n' \
  "${OMAC_BASE}nosuch/foo" || true
cat /tmp/omac-demo-404-body 2>/dev/null || true
echo

# ──────────────────────── Unix-socket transport ───────────────────────
if [[ -n "${OMAC_SOCKET:-}" ]]; then
  echo
  echo "############# Unix-socket transport ($OMAC_SOCKET) #############"
  echo "(expected to fail on macOS under nono proxy mode; works elsewhere)"

  section "GET /echo/status (unix)"
  try curl -sS --unix-socket "$OMAC_SOCKET" http://x/echo/status
  echo

  section "GET /echo/whoami (unix)"
  try curl -sS --unix-socket "$OMAC_SOCKET" http://x/echo/whoami
  echo
fi
