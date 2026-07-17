#!/usr/bin/env bash
#
# classify-compat-failure_test.sh — behavioural tests for the e2e-smoke
# failure classifier. Feeds synthetic .ctx fixtures and asserts the reason
# lines and the infra_only verdict.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
CLASSIFY="$ROOT/scripts/classify-compat-failure.py"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

fail() { echo "FAIL: $1" >&2; exit 1; }

ctx() { # ctx <slug> <header-suffix> <body>
  printf '=== %s ===\n%s\n' "$2" "$3" > "$1/$(echo "$2" | tr ' /@' '___').ctx"
}

# --- Case 1: every leg is model-unavailable -> infra_only=true ---
d1="$WORK/c1"; mkdir -p "$d1"
ctx "$d1" "opencode / ubuntu-latest / omac@main — failing stage(s): llm" \
  "error=\"AI_APICallError: Requested model name 'zai-org/GLM-5.2' is currently not available.\""
ctx "$d1" "codex / ubuntu-latest / omac@release — failing stage(s): llm" \
  "Supported model names are: set()"
out=$(python3 "$CLASSIFY" --ctx-dir "$d1" --out "$d1/r.txt")
echo "$out" | grep -qx 'infra_only=true'   || fail "case1 expected infra_only=true; got: $out"
echo "$out" | grep -qx 'reason_count=2'     || fail "case1 expected reason_count=2; got: $out"
grep -q 'LLM backend unavailable (infra)' "$d1/r.txt" || fail "case1 missing LLM label"

# --- Case 2: a contract-drift leg present -> infra_only=false ---
d2="$WORK/c2"; mkdir -p "$d2"
ctx "$d2" "pi / macos-latest / omac@main — failing stage(s): contract" \
  "CLI contract drift: pi no longer exposes --foo"
ctx "$d2" "opencode / ubuntu-latest / omac@main — failing stage(s): llm" \
  "Requested model name 'zai-org/GLM-5.2' is currently not available"
out=$(python3 "$CLASSIFY" --ctx-dir "$d2" --out "$d2/r.txt")
echo "$out" | grep -qx 'infra_only=false' || fail "case2 expected infra_only=false; got: $out"
grep -q 'real regression' "$d2/r.txt"     || fail "case2 missing real-regression label"

# --- Case 3: no failures -> empty output, infra_only=false ---
d3="$WORK/c3"; mkdir -p "$d3"
out=$(python3 "$CLASSIFY" --ctx-dir "$d3" --out "$d3/r.txt")
echo "$out" | grep -qx 'infra_only=false' || fail "case3 expected infra_only=false; got: $out"
echo "$out" | grep -qx 'reason_count=0'    || fail "case3 expected reason_count=0; got: $out"

echo "PASS: classify-compat-failure"
