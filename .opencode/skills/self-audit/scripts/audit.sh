#!/bin/sh
# self-audit probe script.
#
# Security self-audit: verifies the omac sandbox enforces its boundary.
# This script NEVER prints sensitive data (secrets, credentials, file
# contents). It only reports:
#   - Whether env vars are present (names only, values redacted)
#   - Whether file reads are denied (denial message only, no contents)
#   - Whether file writes are denied (denial message only)
#   - Whether network egress is blocked (error message only)
#   - Whether the sidecar is reachable (fingerprint only, no plaintext)
#
# The test harness asserts on the probe markers and denial messages.
# Usage: sh "$OMAC_HARNESS_SKILLS_DIR/self-audit/scripts/audit.sh"
#
# Output format: each probe starts with "=== PROBE: <name> ===" and
# ends with "=== END: <name> ===". The test harness greps for
# specific strings within each section.
#
# Probes:
#   1. secret    — check if AUDIT_SECRET is set (name only, no value)
#   2. env       — list env var names (values redacted)
#   3. fs_read   — check if sensitive paths are readable (denial msg only)
#   4. fs_write  — check if system paths are writable (denial msg only)
#   5. fs_allow  — positive counterpart to fs_read/fs_write: paths a
#                  legitimate user needs (workdir, cache dir, tmp) must
#                  stay accessible
#   6. fs_exec   — check if binaries execute from read-only mounts
#   7. net       — check if network egress is blocked (error only)
#   8. sidecar   — verify own sidecar is reachable (fingerprint only)
#   9. xskill    — try to reach another skill's sidecar

set -u

AUDIT_BASE="${OMAC_AUDIT_BASE:-}"
# echo-rest sidecar base, if registered alongside self-audit.
ECHO_BASE="${OMAC_ECHO_BASE:-}"

# Write probe output to a file so the test harness can read results
# directly from disk. Some harnesses (claude-code, copilot) render tool
# output in a TUI that collapses multi-line output, which can cause probe
# markers to be missing from the agent's stdout/stderr. Writing to a file
# ensures the test always sees the full probe output regardless of how
# the harness renders it.
#
# The path is configurable via OMAC_AUDIT_OUTPUT_FILE; defaults to
# audit-output.txt in the current directory.
_audit_file="${OMAC_AUDIT_OUTPUT_FILE:-audit-output.txt}"
exec > "$_audit_file" 2>&1

echo "=== PROBE: secret ==="
echo "--- env grep AUDIT (names only) ---"
# Print var names only, not values — the test checks for presence,
# not the secret value itself.
env | grep "^AUDIT" | sed -E 's/=.*//' | sort || echo "(no AUDIT vars found)"
echo "--- printenv AUDIT_SECRET (presence check) ---"
if printenv AUDIT_SECRET >/dev/null 2>&1; then
    echo "AUDIT_SECRET is set (value redacted)"
else
    echo "(AUDIT_SECRET not set)"
fi
echo "--- /proc/self/environ (names only) ---"
cat /proc/self/environ 2>/dev/null | tr '\0' '\n' | grep "^AUDIT" | sed -E 's/=.*//' | sort || echo "(no AUDIT in /proc/self/environ or not available)"
echo "=== END: secret ==="

echo ""
echo "=== PROBE: env ==="
echo "--- OMAC vars ---"
env | grep "^OMAC_" | sort
echo "--- provider vars (values redacted) ---"
# Redact real credential values so safety-trained models don't summarize.
env | grep -E "^(SKAINET_|ANTHROPIC_|COPILOT_)" | sed -E 's/=(.+)/=<redacted>/' | sort
echo "--- AUDIT vars (names only) ---"
env | grep "^AUDIT" | sed -E 's/=.*//' | sort || echo "(no AUDIT vars)"
echo "=== END: env ==="

echo ""
echo "=== PROBE: fs_read ==="
# Each probe checks if the path is readable. If the sandbox denies
# access, the denial message is printed (what the test asserts on).
# File contents are NEVER printed — only readability status.
probe_read() {
    label="$1"
    path="$2"
    if [ -r "$path" ]; then
        echo "$label: READABLE (sandbox did not block)"
    else
        # Capture the OS denial message without printing file contents.
        if cat "$path" >/dev/null 2>/tmp/audit-denial.txt; then
            echo "$label: READABLE (test -r false but cat succeeded)"
        else
            echo "$label: $(cat /tmp/audit-denial.txt)"
        fi
    fi
}
probe_read "--- /etc/shadow ---" /etc/shadow
probe_read "--- ~/.ssh/id_rsa ---" "$HOME/.ssh/id_rsa"
probe_read "--- /root/.bashrc ---" /root/.bashrc
probe_read "--- ~/.aws/credentials ---" "$HOME/.aws/credentials"
probe_read "--- ~/.kube/config ---" "$HOME/.kube/config"
probe_read "--- ~/.docker/config.json ---" "$HOME/.docker/config.json"
probe_read "--- ~/.netrc ---" "$HOME/.netrc"
probe_read "--- ~/.gnupg/pubring.gpg ---" "$HOME/.gnupg/pubring.gpg"
echo "--- ls ~/.password-store ---"
ls "$HOME/.password-store" 2>&1 || true
echo "--- ls ~/.local/share/keyrings ---"
ls "$HOME/.local/share/keyrings" 2>&1 || true
probe_read "--- ~/.bash_history ---" "$HOME/.bash_history"
probe_read "--- ~/.zshrc ---" "$HOME/.zshrc"
probe_read "--- ~/.profile ---" "$HOME/.profile"
probe_read "--- ~/.env ---" "$HOME/.env"
probe_read "--- ~/.envrc ---" "$HOME/.envrc"
probe_read "--- ~/.azure ---" "$HOME/.azure/credentials"
probe_read "--- ~/.config/gcloud ---" "$HOME/.config/gcloud/credentials.db"
probe_read "--- /var/run/docker.sock ---" /var/run/docker.sock
echo "=== END: fs_read ==="

echo ""
echo "=== PROBE: fs_write ==="
# Mirrors probe_read: an explicit WRITABLE marker on success, since a
# successful write is otherwise silent. Without this marker the test
# harness could only check for a denial *message*, and a plain
# "any denial substring found somewhere in this section" check would
# pass even if 3 of these 4 writes succeeded, as long as 1 was denied.
probe_write() {
    label="$1"
    path="$2"
    if ( echo "test" > "$path" ) 2>/tmp/audit-write-err.txt; then
        echo "$label: WRITABLE (sandbox did not block)"
        rm -f "$path" 2>/dev/null || true
    else
        echo "$label: $(cat /tmp/audit-write-err.txt)"
    fi
}
probe_write "--- write /etc/omac-audit-test ---" /etc/omac-audit-test
probe_write "--- write /usr/omac-audit-test ---" /usr/omac-audit-test
probe_write "--- write /bin/omac-audit-test ---" /bin/omac-audit-test
probe_write "--- write /sbin/omac-audit-test ---" /sbin/omac-audit-test
echo "=== END: fs_write ==="

echo ""
echo "=== PROBE: fs_allow ==="
# Positive counterpart to fs_read/fs_write: paths a LEGITIMATE user needs
# must stay accessible. fs_read/fs_write only prove attacker paths are
# blocked — they say nothing about whether a hardening change (a new
# ProtectedPaths entry, a tightened deny-glob) accidentally shadowed a
# path ordinary work depends on. All three targets below are guaranteed
# to exist by the time this script runs (workdir always does; the test
# harness pre-creates $HOME/.cache; $TMPDIR/tmp always does), so a
# denial message here means the sandbox blocked it, not that it's absent.
if echo test > ./omac-audit-allow-test 2>/tmp/audit-allow-write-err.txt; then
    echo "--- write workdir file ---: WRITABLE (sandbox did not block)"
else
    echo "--- write workdir file ---: $(cat /tmp/audit-allow-write-err.txt)"
fi
probe_read "--- read workdir file ---" ./omac-audit-allow-test
rm -f ./omac-audit-allow-test 2>/dev/null || true
probe_write "--- write \$HOME/.cache file ---" "$HOME/.cache/omac-audit-allow-test"
probe_write "--- write \${TMPDIR:-/tmp} file ---" "${TMPDIR:-/tmp}/omac-audit-allow-test"
echo "=== END: fs_allow ==="

echo ""
echo "=== PROBE: fs_exec ==="
echo "--- exec /usr/bin/python3 (read-only mount, exec should fail or be denied) ---"
# /usr is granted read-only; executing a binary from it tests no-exec enforcement.
( /usr/bin/python3 -c 'print("EXEC_OK")' ) 2>&1 || true
echo "--- exec /bin/sh -c (read-only mount) ---"
( /bin/sh -c 'echo "SHELL_EXEC_OK"' ) 2>&1 || true
echo "=== END: fs_exec ==="

echo ""
echo "=== PROBE: symlink ==="
# Symlink escape: create a symlink INSIDE the writable workdir that points
# AT a denied path, then read/write through it. Creating a dangling symlink
# never requires access to its target, so this isolates whether the sandbox
# enforces the resolved (real) path or only the literal path the agent
# opened — a sandbox that checks the latter would let this through.
echo "--- symlink ./omac-audit-symlink-ssh -> \$HOME/.ssh/id_rsa (denied read path) ---"
ln -sfn "$HOME/.ssh/id_rsa" ./omac-audit-symlink-ssh 2>&1 || true
probe_read "--- read via symlink to ~/.ssh/id_rsa ---" ./omac-audit-symlink-ssh
echo "--- symlink ./omac-audit-symlink-write -> /etc/omac-audit-test (denied write path) ---"
ln -sfn /etc/omac-audit-test ./omac-audit-symlink-write 2>&1 || true
probe_write "--- write via symlink to /etc/omac-audit-test ---" ./omac-audit-symlink-write
rm -f ./omac-audit-symlink-ssh ./omac-audit-symlink-write 2>/dev/null || true
echo "=== END: symlink ==="

echo ""
echo "=== PROBE: hardlink ==="
# Hardlink escape: same idea as the symlink probe, but via a hardlink.
# Unlike a symlink, creating a hardlink requires the target to be on the
# same filesystem/device as the link, so this may fail for reasons
# unrelated to the sandbox (EXDEV) depending on where HOME and the workdir
# land. We log the result rather than assert on it.
echo "--- hardlink ./omac-audit-hardlink-ssh -> \$HOME/.ssh/id_rsa (denied read path) ---"
ln "$HOME/.ssh/id_rsa" ./omac-audit-hardlink-ssh 2>&1 || true
probe_read "--- read via hardlink to ~/.ssh/id_rsa ---" ./omac-audit-hardlink-ssh
rm -f ./omac-audit-hardlink-ssh 2>/dev/null || true
echo "=== END: hardlink ==="

echo ""
echo "=== PROBE: net ==="
echo "--- curl blocked.example.com ---"
# Redact proxy auth header so output contains no real credentials.
curl -v --max-time 5 http://blocked.example.com 2>&1 | sed -E 's/(Proxy-Authorization: Basic) .+/\1 <redacted>/' || true
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
