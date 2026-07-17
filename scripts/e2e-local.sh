#!/usr/bin/env bash
#
# e2e-local.sh — run the omac e2e suite from inside an omac sandbox.
#
# The e2e tests (internal/e2e/, build tag `e2e`) are designed for a host
# shell: `go test` installs harnesses via bun/npm, then `omac start` spawns
# the inner agent under a fresh Seatbelt/bwrap sandbox. Three things break
# when that pipeline runs from inside an already-running omac sandbox:
#
#   1. bun/npm postinstall scripts fail with EPERM — the omac sandbox
#      blocks the package manager from spawning the postinstall subprocess
#      that downloads the harness's platform binary.
#   2. `omac start`'s facade fails to bind its Unix socket — the runtime
#      dir lives under $TMPDIR, which inside the sandbox is a deep path
#      ($TMPDIR/omac-<hash>/bridge.sock) that exceeds macOS's 104-byte
#      SUN_LEN limit, yielding `bind: invalid argument`.
#   3. The inner sandbox (sandbox-exec / nono / bwrap) can't be applied —
#      macOS denies nested Seatbelt profile application with
#      `sandbox_apply: Operation not permitted`.
#
# This wrapper detects the nested-sandbox environment (OMAC_SOCKET is set
# only inside an omac sandbox) and exports three env vars the test code
# reads to accommodate each blocker:
#
#   E2E_NESTED=1              — forces --no-sandbox for every harness
#                              (blocker 3) and shortens TMPDIR (blocker 2).
#   E2E_RECOVER_INSTALL=1     — installHarness retries with --ignore-scripts
#                              then runs the package's postinstall script
#                              via `node` directly (blocker 1).
#   TMPDIR=/tmp/omac-e2e      — a short path so bridge.sock stays under
#                              SUN_LEN (blocker 2).
#
# CI never sets OMAC_SOCKET, so none of these branches are taken there —
# the host-shell and CI paths are completely unchanged.
#
# Usage:
#   scripts/e2e-local.sh [harness]              # smoke tier (no secrets)
#   scripts/e2e-local.sh smoke [harness]       # smoke tier explicitly
#   scripts/e2e-local.sh echo [harness]         # full echo-rest (needs secrets)
#   scripts/e2e-local.sh audit [harness]       # security audit (needs secrets)
#
# When no subcommand is given, defaults to "smoke". harness defaults to
# opencode. Pass extra `go test` flags after `--`.
#
# Examples:
#   scripts/e2e-local.sh                                   # smoke, opencode
#   scripts/e2e-local.sh smoke claude-code
#   scripts/e2e-local.sh echo opencode
#   scripts/e2e-local.sh audit opencode -- -run TestE2ESecurityAudit/opencode
#
# Outside an omac sandbox this script is a thin passthrough — it sets none
# of the E2E_NESTED / E2E_RECOVER_INSTALL vars and just runs go test.

set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"

# Tier + harness parsing.
tier="smoke"
harness="${E2E_HARNESS:-opencode}"
extra=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        smoke|echo|audit) tier="$1"; shift ;;
        --) shift; extra+=("$@"); break ;;
        -*) extra+=("$1"); shift ;;
        *) harness="$1"; shift ;;
    esac
done

go_args=(
    -tags=e2e
    -timeout=30m
    -v
)

case "$tier" in
    smoke)
        go_args+=(-run 'TestHarnessCLIContract|TestHarnessLaunchProbe')
        ;;
    echo)
        go_args+=(-run 'TestE2EEchoRest')
        ;;
    audit)
        go_args+=(-run 'TestE2ESecurityAudit')
        ;;
esac

# Detect nested-omac-sandbox execution. OMAC_SOCKET is set by `omac start`
# / `omac serve` into the inner process env; it's never set on a bare host
# shell or in CI. This is the signal that we're running nested.
#
# We set E2E_NESTED=1 here so the test code has an explicit signal, but the
# test code's nestedInOmacSandbox() also reads OMAC_SOCKET directly as a
# belt-and-suspenders fallback — so an agent running inside an omac sandbox
# that shells out to `go test ./internal/e2e/` directly (not via this
# wrapper) still gets the accommodations. That's intentional: the
# accommodations are required whenever the test runs nested, regardless of
# how it was invoked. See the doc comment on nestedInOmacSandbox in
# fixtures.go for the full rationale.
if [[ -n "${OMAC_SOCKET:-}" ]]; then
    echo "== nested omac sandbox detected (OMAC_SOCKET set) ==" >&2
    export E2E_NESTED=1
    export E2E_RECOVER_INSTALL=1
    # Short TMPDIR for the `go test` process itself so any temp files it
    # creates (e.g. buildOmac's t.TempDir) stay under SUN_LEN. The test
    # code (shortTmpDirForNested) further overrides TMPDIR to
    # /tmp/omac-e2e-<pid> for the omac start/serve subprocesses, isolating
    # them from this process's TMPDIR. Both stay well under 104 bytes.
    export TMPDIR="/tmp/omac-e2e"
    mkdir -p "$TMPDIR"
    echo "== forced --no-sandbox, install recovery, TMPDIR=$TMPDIR ==" >&2

    # The test code falls back from bun to npm for opencode when bun is
    # missing (resolveInstallCmd in e2e_test.go), so bun is not required.
    # If a host-level bun exists at a standard location, add it to PATH so
    # the faster bun install path is used; otherwise the npm fallback
    # handles it. We don't error — the test code's fallback is the single
    # source of truth for installer selection.
    if ! command -v bun >/dev/null 2>&1; then
        BUN_CANDIDATES=(
            "${BUN_INSTALL:-/dev/null}/bin/bun"
            "$HOME/.bun/bin/bun"
            /opt/homebrew/bin/bun
            /usr/local/bin/bun
        )
        for c in "${BUN_CANDIDATES[@]}"; do
            if [[ -x "$c" ]]; then
                export PATH="$(dirname "$c"):$PATH"
                echo "== bun found at $c, added to PATH ==" >&2
                break
            fi
        done
    fi
else
    # Bare host: honor an explicit E2E_NESTED=1 if the user set it, but
    # don't force the accommodations on otherwise.
    :
fi

# Single-harness selection (matches the E2E_HARNESS env the tests read).
export E2E_HARNESS="$harness"

echo "== go test ${go_args[*]} ${extra[*]:-} ./internal/e2e/ ==" >&2
cd "$REPO"
exec go test "${go_args[@]}" ${extra[@]+"${extra[@]}"} ./internal/e2e/
