#!/bin/bash
# run-matrix.sh — Ticket 01 loopback spike posture matrix.
#
# RUN THIS FROM A HOST TERMINAL (outside any omac sandbox). Inside a sandbox,
# sandbox_apply fails with EPERM ("sandbox-exec: sandbox_apply: Operation not
# permitted") — verified in-session; every posture would falsely read as
# fully blocked.
#
# What it does:
#   1. Starts a "pre-existing host listener" on $GUARDED_PORT (default 19211)
#      on BOTH 127.0.0.1 and ::1 (two python3 helpers) OUTSIDE the sandbox.
#   2. For each posture profile, substitutes __GUARDED_PORT__/__EXACT_PORT__/
#      __HOME__/__SCRATCH_LOG_DIR__ into a temp .sb file, then runs:
#         sandbox-exec -f <tmpprofile> java Probe.java server <guarded> <exact>
#      with JDK_JAVA_OPTIONS / JAVA_TOOL_OPTIONS / *_PROXY scrubbed from the
#      child env (the host shell here carries omac proxy injection which
#      would skew IPv4/IPv6 and egress results).
#   3. Captures rc + combined output to $LOG_DIR/<posture>.log and echoes the
#      rendered SBPL to $LOG_DIR/<posture>.sb so the report can quote the
#      exact effective policy per posture.
#   4. Prints a summary table by grepping RESULT lines.
#
# Idempotent: safe to re-run; temp profiles go to a fresh mktemp dir, logs are
# overwritten per posture, listeners are killed on exit via trap.
#
# Usage:
#   ./run-matrix.sh                 # all postures, GUARDED_PORT=19211
#   GUARDED_PORT=29211 ./run-matrix.sh
#   POSTURES="dynamic-port dynamic-port-deny" ./run-matrix.sh   # subset

set -u
set -o pipefail

FIXTURE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROFILE_DIR="$FIXTURE_DIR/profiles"
SCRATCH_ROOT="$HOME/.agents/artifacts/github.com--TNG--oh-my-agentic-coder/.scratch/jvm-build-executor/01-loopback"
LOG_DIR="$SCRATCH_ROOT/logs"
mkdir -p "$LOG_DIR"

GUARDED_PORT="${GUARDED_PORT:-19211}"
EXACT_PORT=19312   # fixed, predeclared "known good" loopback port for exact-port posture
CONNECT_TIMEOUT_GUARD=70   # generous per-posture timeout for the whole java probe

# Postures in matrix order. dynamic-port-deny BEFORE deny-before-allow so the
# order-sensitivity pair is adjacent in the logs.
POSTURES="${POSTURES:-blocked exact-port dynamic-port dynamic-port-deny deny-before-allow dynamic-port-deny-v4 dynamic-port-deny-v6 env-only open}"

JAVA_BIN="$(command -v java || true)"
PYTHON_BIN="$(command -v python3 || true)"
NC_BIN="$(command -v nc || true)"
if [[ -z "$JAVA_BIN" ]]; then
    echo "FATAL: no java on PATH. Do NOT use /usr/libexec/java_home (points at a" >&2
    echo "nonexistent Apple JDK on this machine); install Temurin or export PATH." >&2
    exit 1
fi
# env -i later strips PATH down to system dirs + the java dir. If `java` is a
# jenv SHIM (a bash script that shells out to jenv-* helpers on PATH), the shim
# breaks under env -i AND under deny-default sandboxing (jenv reads /dev/fd
# process substitution, which Seatbelt denies). Resolve through the shim chain
# to the REAL JVM binary instead: shim -> jenv-exec -> <jenv-version>/bin/java.
resolve_java_bin() {
    local j="$1" target
    for _ in 1 2 3 4 5; do
        if [[ -L "$j" ]]; then
            target="$(readlink "$j")"
            [[ "$target" != /* ]] && target="$(cd "$(dirname "$j")" && cd "$(dirname "$target")" && pwd)/$(basename "$target")"
            j="$target"
            continue
        fi
        if head -1 "$j" 2>/dev/null | grep -qE '^#!'; then
            # script (jenv shim): the real JVM lives in the active jenv version
            local ver
            ver="$(cat "$HOME/.jenv/version" 2>/dev/null || true)"
            if [[ -n "$ver" && -x "$HOME/.jenv/versions/$ver/bin/java" ]]; then
                echo "$HOME/.jenv/versions/$ver/bin/java"
                return 0
            fi
        fi
        break
    done
    echo "$j"
}
REAL_JAVA_BIN="$(resolve_java_bin "$JAVA_BIN")"
if [[ ! -x "$REAL_JAVA_BIN" ]]; then
    echo "FATAL: resolved java binary not executable: $REAL_JAVA_BIN (from $JAVA_BIN)" >&2
    exit 1
fi
if [[ "$REAL_JAVA_BIN" != "$JAVA_BIN" ]]; then
    echo "[run-matrix] resolved jenv shim $JAVA_BIN -> $REAL_JAVA_BIN"
fi
JAVA_BIN="$REAL_JAVA_BIN"
echo "[run-matrix] java: $JAVA_BIN"
"$JAVA_BIN" -version 2>&1 | sed 's/^/[run-matrix]   /'

if [[ -z "$PYTHON_BIN" && -z "$NC_BIN" ]]; then
    echo "FATAL: need python3 or nc for the host listener." >&2
    exit 1
fi

# --- guarded host listeners --------------------------------------------------
LISTENER_PIDS=()
start_listener() {
    local addr="$1"
    if [[ -n "$PYTHON_BIN" ]]; then
        "$PYTHON_BIN" - "$addr" "$GUARDED_PORT" <<'PYEOF' &
import socket, sys, threading
addr, port = sys.argv[1], int(sys.argv[2])
family = socket.AF_INET6 if ":" in addr else socket.AF_INET
s = socket.socket(family, socket.SOCK_STREAM)
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind((addr, port))
s.listen(16)
print(f"LISTENER-READY {addr}:{port}", flush=True)
def serve(c, peer):
    try:
        c.sendall(b"guarded-listener-ok\n")
    except OSError:
        pass
    finally:
        c.close()
while True:
    try:
        c, peer = s.accept()
        threading.Thread(target=serve, args=(c, peer), daemon=True).start()
    except OSError:
        break
PYEOF
        LISTENER_PIDS+=($!)
    else
        # nc fallback: loop forever accepting and replying.
        (
            while true; do
                printf 'guarded-listener-ok\n' | "$NC_BIN" -l "$addr" "$GUARDED_PORT" >/dev/null 2>&1
            done
        ) &
        LISTENER_PIDS+=($!)
    fi
}

echo "[run-matrix] starting guarded host listeners on 127.0.0.1:$GUARDED_PORT and [::1]:$GUARDED_PORT (UNSANDBOXED)"
# python's LISTENER-READY lines are parented to a file so we can confirm BOTH
# families bound — a silently dead ::1 listener would make guarded-v6 read
# ECONNREFUSED and fake an IPv6 "deny hole" that is really a rig failure.
LISTENER_READY_FILE="$(mktemp /tmp/loopback-spike-ready.XXXXXX)"
exec 9>>"$LISTENER_READY_FILE"
start_listener "127.0.0.1" 1>&9 2>&9
start_listener "::1" 1>&9 2>&9
exec 9>&-
sleep 1
for fam in 127.0.0.1 ::1; do
    if ! grep -q "LISTENER-READY $fam:$GUARDED_PORT" "$LISTENER_READY_FILE" 2>/dev/null; then
        echo "[run-matrix] FATAL: guarded listener on $fam:$GUARDED_PORT did not signal ready." >&2
        echo "[run-matrix] Output so far:" >&2; sed 's/^/[run-matrix]   /' "$LISTENER_READY_FILE" >&2
        echo "[run-matrix] Port in use, or IPv6 unavailable on this host? Pick another port:" >&2
        echo "[run-matrix]   GUARDED_PORT=<port> ./run-matrix.sh" >&2
        rm -f "$LISTENER_READY_FILE"
        kill "${LISTENER_PIDS[@]:-}" 2>/dev/null
        exit 1
    fi
done
rm -f "$LISTENER_READY_FILE"
echo "[run-matrix] both guarded listeners confirmed up"

# Belt-and-braces: verify the v4 listener is actually reachable (python may have
# signaled ready then died). Cannot do the same for ::1 via bash /dev/tcp.
for fam in 127.0.0.1; do
    if ! (exec 3<>"/dev/tcp/$fam/$GUARDED_PORT") 2>/dev/null; then
        if true; then
            echo "[run-matrix] FATAL: guarded listener on $fam:$GUARDED_PORT not reachable before any sandboxing." >&2
            echo "[run-matrix] PORT IN USE? pick another: GUARDED_PORT=<port> ./run-matrix.sh" >&2
            kill "${LISTENER_PIDS[@]}" 2>/dev/null
            exit 1
        fi
    else
        exec 3>&- 3<&-
    fi
done

cleanup() {
    echo "[run-matrix] stopping guarded listeners"
    kill "${LISTENER_PIDS[@]}" 2>/dev/null
    wait "${LISTENER_PIDS[@]}" 2>/dev/null
    [[ -n "${TMPDIR_SPIKE:-}" && -d "${TMPDIR_SPIKE:-}" ]] && rm -rf "$TMPDIR_SPIKE"
}
trap cleanup EXIT

TMPDIR_SPIKE="$(mktemp -d /tmp/loopback-spike.XXXXXX)"

# --- posture runs ------------------------------------------------------------
RENDERED_SBS=()
run_posture() {
    local posture="$1"
    local src="$PROFILE_DIR/$posture.sb"
    local rendered="$TMPDIR_SPIKE/$posture.sb"
    local log="$LOG_DIR/$posture.log"

    if [[ ! -f "$src" ]]; then
        echo "[run-matrix] SKIP $posture — no profile at $src" | tee "$log"
        return 0
    fi

    # Substitute placeholders into a temp profile (runner-generated file).
    sed -e "s/__GUARDED_PORT__/$GUARDED_PORT/g" \
        -e "s/__EXACT_PORT__/$EXACT_PORT/g" \
        -e "s|__HOME__|$HOME|g" \
        -e "s|__SCRATCH_LOG_DIR__|$SCRATCH_ROOT|g" \
        "$src" > "$rendered"
    cp "$rendered" "$LOG_DIR/$posture.sb"   # exact effective policy for the report
    RENDERED_SBS+=("$posture")

    {
        echo "=== posture: $posture ==="
        echo "=== date: $(date -u +%Y-%m-%dT%H:%M:%SZ) ==="
        echo "=== profile (rendered): $rendered ==="
        echo "=== guarded listener: 127.0.0.1:$GUARDED_PORT + [::1]:$GUARDED_PORT ==="
    } > "$log"

    # Clean env for the sandboxed JVM: Belt-and-braces removal of the omac
    # proxy injection and JVM picker flags present in the host shell, so the
    # kernel posture is the only variable. PROBE_SOURCE lets Probe re-launch
    # itself in child/grandchild JVMs.
    env -i \
        PATH="/usr/bin:/bin:/usr/sbin:/sbin:$(dirname "$JAVA_BIN")" \
        HOME="$HOME" \
        TMPDIR="${TMPDIR:-/tmp}" \
        LANG="${LANG:-en_US.UTF-8}" \
        PROBE_SOURCE="$FIXTURE_DIR/Probe.java" \
        /usr/bin/sandbox-exec -f "$rendered" \
        "$JAVA_BIN" "$FIXTURE_DIR/Probe.java" server "$GUARDED_PORT" "$EXACT_PORT" \
        >> "$log" 2>&1 &
    local probe_pid=$!

    # Enforce a whole-posture timeout without GNU timeout(1) (absent on macOS).
    local waited=0
    while kill -0 "$probe_pid" 2>/dev/null && (( waited < CONNECT_TIMEOUT_GUARD )); do
        sleep 1
        ((waited++))
    done
    if kill -0 "$probe_pid" 2>/dev/null; then
        echo "=== TIMEOUT: posture run exceeded ${CONNECT_TIMEOUT_GUARD}s; killed ===" >> "$log"
        kill -9 "$probe_pid" 2>/dev/null
        wait "$probe_pid" 2>/dev/null
        echo "=== rc: TIMEOUT ===" >> "$log"
    else
        wait "$probe_pid"
        local rc=$?
        echo "=== rc: $rc ===" >> "$log"
    fi
}

echo "[run-matrix] postures: $POSTURES"
for p in $POSTURES; do
    echo "[run-matrix] running posture: $p"
    run_posture "$p"
done

# --- summary ----------------------------------------------------------------
echo
echo "================ POSTURE MATRIX SUMMARY (GUARDED_PORT=$GUARDED_PORT) ================"
printf '%-24s | %-16s | %-16s | %-19s | %-19s | %-14s\n' \
    "posture" "child+v4-loopback" "grandchild" "guarded v4" "guarded v6" "ext egress"
printf -- '-------------------------+------------------+------------------+---------------------+---------------------+----------------\n'
for p in $POSTURES; do
    log="$LOG_DIR/$p.log"
    [[ -f "$log" ]] || { printf '%-24s | %s\n' "$p" "no log"; continue; }
    cell() { # cell <probe-key> <log>
        local key="$1" f="$2"
        local line
        line="$(grep -E "^RESULT $key " "$f" | tail -1)"
        if [[ -z "$line" ]]; then
            if grep -q "Operation not permitted" "$f" && ! grep -q "^RESULT " "$f"; then
                printf '%s' "SB-APPLY-EPERM?"
            else
                printf '%s' "no-result"
            fi
        elif echo "$line" | grep -q " OK "; then
            printf '%s' "OK(reachable)"
        else
            printf '%s' "FAIL(denied)"
        fi
    }
    child="$(cell child-loopback "$log")"
    grand="$(cell grandchild-loopback "$log")"
    gv4="$(cell guarded-v4 "$log")"
    gv6="$(cell guarded-v6 "$log")"
    ext="$(cell external-egress "$log")"
    printf '%-24s | %-16s | %-16s | %-19s | %-19s | %-14s\n' \
        "$p" "$child" "$grand" "$gv4" "$gv6" "$ext"
done
echo
echo "[run-matrix] raw logs:    $LOG_DIR/<posture>.log"
echo "[run-matrix] rendered SBPL: $LOG_DIR/<posture>.sb"
echo
echo "NEXT: inspect per-posture logs, then run the Worker API reproducer:"
echo "  $SCRATCH_ROOT/gradle-worker-api/run-worker.sh dynamic-port-deny"
