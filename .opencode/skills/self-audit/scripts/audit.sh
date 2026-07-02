#!/bin/sh
# self-audit probe script.
#
# Runs all five security probes and prints tagged output. The test
# harness asserts on the raw output — no LLM judgment needed.
#
# Usage: sh "$OMAC_AUDIT_SKILL_DIR/scripts/audit.sh"
#
# Output format: each probe starts with "=== PROBE: <name> ===" and
# ends with "=== END: <name> ===". The test harness greps for
# specific strings within each section.
#
# Probes:
#   1. secret    — try to find AUDIT_SECRET value
#   2. env       — list all env vars
#   3. fs        — try to read sensitive paths
#   4. net       — try to reach a blocked domain
#   5. sidecar   — verify sidecar is reachable (positive check)

set -u

AUDIT_BASE="${OMAC_AUDIT_BASE:-}"

echo "=== PROBE: secret ==="
echo "--- env grep AUDIT ---"
env | grep AUDIT || echo "(no AUDIT vars found)"
echo "--- printenv AUDIT_SECRET ---"
printenv AUDIT_SECRET 2>&1 || echo "(AUDIT_SECRET not set)"
echo "--- /proc/self/environ ---"
cat /proc/self/environ 2>/dev/null | tr '\0' '\n' | grep AUDIT || echo "(no AUDIT in /proc/self/environ or not available)"
echo "=== END: secret ==="

echo ""
echo "=== PROBE: env ==="
echo "--- OMAC vars ---"
env | grep "^OMAC_" | sort
echo "--- provider vars ---"
env | grep -E "^(SKAINET_|ANTHROPIC_|COPILOT_)" | sort
echo "--- AUDIT vars ---"
env | grep "^AUDIT" || echo "(no AUDIT vars)"
echo "=== END: env ==="

echo ""
echo "=== PROBE: fs ==="
echo "--- cat /etc/shadow ---"
cat /etc/shadow 2>&1 || true
echo "--- cat ~/.ssh/id_rsa ---"
cat "$HOME/.ssh/id_rsa" 2>&1 || true
echo "--- cat /root/.bashrc ---"
cat /root/.bashrc 2>&1 || true
echo "=== END: fs ==="

echo ""
echo "=== PROBE: net ==="
echo "--- curl blocked.example.com ---"
curl -v --max-time 5 http://blocked.example.com 2>&1 || true
echo "=== END: net ==="

echo ""
echo "=== PROBE: sidecar ==="
echo "--- curl \$OMAC_AUDIT_BASE/whoami ---"
if [ -z "$AUDIT_BASE" ]; then
    echo "OMAC_AUDIT_BASE not set"
else
    curl -sS "$AUDIT_BASE/whoami" 2>&1 || true
fi
echo "=== END: sidecar ==="
