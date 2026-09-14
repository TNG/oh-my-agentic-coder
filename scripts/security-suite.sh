#!/usr/bin/env bash
#
# security-suite.sh — run omac's security regression suite.
#
# Every test in the suite asserts a security property omac is supposed to
# hold. A RED test therefore means the property is broken right now and the
# matching weakness is live in the tree. The suite is expected to fail until
# the fixes land, so the set of currently-failing tests is pinned in
# security-suite-expected-failures.txt and this script compares against it.
#
#   test newly PASSES  -> a fix landed. Delete its line from the pinned file
#                         in the same commit; that is the proof the fix works.
#   test newly FAILS   -> a fix regressed, or a new property was added
#                         without pinning it.
#
# Both directions exit non-zero, so the suite stays a meaningful gate while
# most of it is still red.
#
# Usage:
#   scripts/security-suite.sh          # run and compare against the pinned set
#   scripts/security-suite.sh list     # print the failing set, pin-file format
#   scripts/security-suite.sh raw      # plain verbose go test output, no compare
#
# The suite never skips: in a set where red means "vulnerable", a skipped test
# reads as a passing one. A test that cannot run fails instead, so the
# environment has to supply what the suite needs. Those preconditions are
# checked up front (see preflight) rather than being discovered one red test at
# a time:
#
#   - the ability to bind a loopback port (nine tests drive real servers)
#   - bash, curl and jq            (the harness bridge hooks)
#   - bubblewrap with working user namespaces, on Linux (datagram confinement)
#
# Entries tagged "# linux-only" in the pin file are dropped from the comparison
# on other platforms, where the test does not exist and would otherwise be
# reported as fixed.
#
# Exit code 0 = failing set matches the pinned set exactly.
# Exit code 2 = the environment cannot run the suite; nothing was measured.

set -euo pipefail

REPO="$(cd "$(dirname "$0")/.." && pwd)"
PINNED="$REPO/scripts/security-suite-expected-failures.txt"
TAG="vuln"
SELECT='^TestSecurity'

# run_suite emits one "<package> <TestName>" line per failing top-level test.
# Parsing `go test -json` rather than the human output keeps the package
# attribution correct when packages run in parallel, and a build or setup
# failure (a "fail" record with no test attached) is reported as such instead
# of silently shrinking the failing set.
run_suite() {
    local json
    # stderr is kept: a toolchain or module failure emits nothing on the JSON
    # stream, which would empty the failing set and report every pinned
    # property as fixed.
    json="$(cd "$REPO" && go test -tags="$TAG" -run "$SELECT" -json ./... 2>>/dev/stderr || true)"
    printf '%s\n' "$json" | python3 -c '
import json, sys

MODULE = "github.com/TNG/oh-my-agentic-coder/"
failed, broken = set(), []
for line in sys.stdin:
    line = line.strip()
    if not line.startswith("{"):
        continue
    try:
        ev = json.loads(line)
    except json.JSONDecodeError:
        continue
    if ev.get("Action") != "fail":
        continue
    pkg = ev.get("Package", "").removeprefix(MODULE)
    test = ev.get("Test", "")
    if not test:
        # Package-level failure with no failing test of ours: a build error
        # or a TestMain problem. Never a security signal.
        if not any(f.startswith(pkg + " ") for f in failed):
            broken.append(pkg)
        continue
    if "/" in test:
        continue  # subtest; the suite is pinned at top-level granularity
    failed.add(f"{pkg} {test}")

if broken:
    print("suite did not build or run in: " + ", ".join(sorted(set(broken))), file=sys.stderr)
    sys.exit(2)
for entry in sorted(failed):
    print(entry)
'
}

# pinned_set strips comments and blanks so the pin file can be annotated, and
# drops platform-gated entries whose test does not exist here. LC_ALL=C matches
# the byte order run_suite emits; a collating locale would order the two sides
# differently and comm would produce nonsense.
pinned_set() {
    local entries
    entries="$(grep -vE '^[[:space:]]*(#|$)' "$PINNED")"
    if [[ "$(uname -s)" != "Linux" ]]; then
        entries="$(printf '%s\n' "$entries" | grep -v '# linux-only' || true)"
    fi
    printf '%s\n' "$entries" | sed 's/[[:space:]]*#.*$//' | grep -v '^$' | LC_ALL=C sort
}

# preflight refuses to run when the environment cannot exercise the suite.
#
# Without it an unusable machine is indistinguishable from a vulnerable one:
# every guarded test fails, the failing set still matches the pinned set, and
# the script reports "no change" and exits 0 having measured nothing. After a
# fix lands the same machine reports the fix as ineffective. Both answers are
# wrong in a way no one would notice, so the environment is checked once, here.
preflight() {
    local missing=()

    if ! python3 - <<'PY' 2>/dev/null
import socket, sys
s = socket.socket()
try:
    s.bind(("127.0.0.1", 0))
except OSError:
    sys.exit(1)
finally:
    s.close()
PY
    then
        missing+=("a bindable loopback port (nine tests drive real servers)")
    fi

    for tool in bash curl jq; do
        command -v "$tool" >/dev/null 2>&1 || missing+=("$tool (the harness bridge hooks need it)")
    done

    if [[ "$(uname -s)" == "Linux" ]]; then
        if ! command -v bwrap >/dev/null 2>&1; then
            missing+=("bubblewrap (datagram confinement)")
        elif ! bwrap --ro-bind / / true >/dev/null 2>&1; then
            missing+=("working unprivileged user namespaces for bwrap (datagram confinement)")
        fi
    fi

    if [[ ${#missing[@]} -gt 0 ]]; then
        echo "the security suite cannot run here; nothing was measured:" >&2
        printf '  - %s\n' "${missing[@]}" >&2
        echo >&2
        echo "Run it on a normal host, or in the e2e container:" >&2
        echo "  scripts/e2e-docker.sh build && scripts/e2e-docker.sh shell" >&2
        exit 2
    fi
}

cmd_list() {
    preflight
    run_suite
}

cmd_raw() {
    preflight
    cd "$REPO"
    go test -tags="$TAG" -run "$SELECT" -v ./...
}

cmd_compare() {
    preflight
    local actual expected
    actual="$(run_suite)"
    expected="$(pinned_set)"

    local fixed regressed
    fixed="$(comm -13 <(printf '%s\n' "$actual") <(printf '%s\n' "$expected") || true)"
    regressed="$(comm -23 <(printf '%s\n' "$actual") <(printf '%s\n' "$expected") || true)"

    local rc=0
    if [[ -n "$fixed" ]]; then
        echo "== now PASSING (property is fixed) =="
        printf '%s\n' "$fixed"
        echo
        echo "Remove these lines from ${PINNED#"$REPO"/} in the same commit as the fix."
        rc=1
    fi
    if [[ -n "$regressed" ]]; then
        echo "== now FAILING and not pinned =="
        printf '%s\n' "$regressed"
        echo
        echo "Either a fix regressed, or a new test needs pinning."
        rc=1
    fi
    if [[ $rc -eq 0 ]]; then
        local n
        n="$(printf '%s\n' "$expected" | grep -c . || true)"
        echo "security suite: $n known-broken properties, no change."
    fi
    return $rc
}

main() {
    case "${1:-compare}" in
        compare) cmd_compare ;;
        list)    cmd_list ;;
        raw)     cmd_raw ;;
        *)       sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 1 ;;
    esac
}

main "$@"
