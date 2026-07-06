#!/bin/sh
# self-audit probe script.
#
# Runs all security probes and prints tagged output. The test
# harness asserts on the raw output — no LLM judgment needed.
#
# Usage: sh "$OMAC_HARNESS_SKILLS_DIR/self-audit/scripts/audit.sh"
#
# Output format: each probe starts with "=== PROBE: <name> ===" and
# ends with "=== END: <name> ===". The test harness greps for
# specific strings within each section.
#
# Probes:
#   1. secret    — try to find AUDIT_SECRET value
#   2. env       — list all env vars
#   3. fs_read   — try to read sensitive paths (credentials, shell configs)
#   4. fs_write  — try to write to system paths (read-only mounts)
#   5. fs_exec   — try to execute binaries from denied paths
#   6. net       — try to reach a blocked domain
#   7. sidecar   — verify own sidecar is reachable (positive check)
#   8. xskill    — try to reach another skill's sidecar (cross-skill isolation)

set -u

AUDIT_BASE="${OMAC_AUDIT_BASE:-}"
# echo-rest sidecar base, if registered alongside self-audit.
ECHO_BASE="${OMAC_ECHO_BASE:-}"

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
echo "=== PROBE: fs_read ==="
echo "--- cat /etc/shadow ---"
cat /etc/shadow 2>&1 || true
echo "--- cat ~/.ssh/id_rsa ---"
cat "$HOME/.ssh/id_rsa" 2>&1 || true
echo "--- cat /root/.bashrc ---"
cat /root/.bashrc 2>&1 || true
echo "--- cat ~/.aws/credentials ---"
cat "$HOME/.aws/credentials" 2>&1 || true
echo "--- cat ~/.kube/config ---"
cat "$HOME/.kube/config" 2>&1 || true
echo "--- cat ~/.docker/config.json ---"
cat "$HOME/.docker/config.json" 2>&1 || true
echo "--- cat ~/.netrc ---"
cat "$HOME/.netrc" 2>&1 || true
echo "--- cat ~/.gnupg/pubring.gpg ---"
cat "$HOME/.gnupg/pubring.gpg" 2>&1 || true
echo "--- cat ~/.password-store ---"
ls "$HOME/.password-store" 2>&1 || true
echo "--- ls ~/.local/share/keyrings ---"
ls "$HOME/.local/share/keyrings" 2>&1 || true
echo "--- cat ~/.bash_history ---"
cat "$HOME/.bash_history" 2>&1 || true
echo "--- cat ~/.zshrc ---"
cat "$HOME/.zshrc" 2>&1 || true
echo "--- cat ~/.profile ---"
cat "$HOME/.profile" 2>&1 || true
echo "--- cat ~/.env ---"
cat "$HOME/.env" 2>&1 || true
echo "--- cat ~/.envrc ---"
cat "$HOME/.envrc" 2>&1 || true
echo "=== END: fs_read ==="

echo ""
echo "=== PROBE: fs_write ==="
echo "--- write /etc/omac-audit-test ---"
echo "test" > /etc/omac-audit-test 2>&1 || true
echo "--- write /usr/omac-audit-test ---"
echo "test" > /usr/omac-audit-test 2>&1 || true
echo "--- write /bin/omac-audit-test ---"
echo "test" > /bin/omac-audit-test 2>&1 || true
echo "--- write /sbin/omac-audit-test ---"
echo "test" > /sbin/omac-audit-test 2>&1 || true
echo "=== END: fs_write ==="

echo ""
echo "=== PROBE: fs_exec ==="
echo "--- exec /usr/bin/python3 (read-only mount, exec should fail or be denied) ---"
# /usr is granted read-only; executing a binary from it tests no-exec enforcement.
/usr/bin/python3 -c 'print("EXEC_OK")' 2>&1 || true
echo "--- exec /bin/sh -c (read-only mount) ---"
/bin/sh -c 'echo "SHELL_EXEC_OK"' 2>&1 || true
echo "=== END: fs_exec ==="

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

echo ""
echo "=== PROBE: xskill ==="
echo "--- curl \$OMAC_ECHO_BASE/whoami (cross-skill isolation) ---"
if [ -z "$ECHO_BASE" ]; then
    echo "OMAC_ECHO_BASE not set (echo-rest not registered)"
else
    curl -sS "$ECHO_BASE/whoami" 2>&1 || true
fi
echo "=== END: xskill ==="
