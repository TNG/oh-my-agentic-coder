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
# Tests that would damage the machine they run on (clobbering /tmp state, the
# real tool cache, a live `omac serve`) skip unless OMAC_SECURITY_CONTAINER=1
# is set. Run those in a throwaway container, never on a working host.
#
# Exit code 0 = failing set matches the pinned set exactly.

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
    json="$(cd "$REPO" && go test -tags="$TAG" -run "$SELECT" -json ./... 2>/dev/null || true)"
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

# pinned_set strips comments and blanks so the pin file can be annotated.
pinned_set() {
    grep -vE '^\s*(#|$)' "$PINNED" | sort
}

cmd_list() {
    run_suite
}

cmd_raw() {
    cd "$REPO"
    go test -tags="$TAG" -run "$SELECT" -v ./...
}

cmd_compare() {
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
