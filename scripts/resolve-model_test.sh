#!/usr/bin/env bash
# Tests for scripts/resolve-model.sh.
#
# The point of these is parity: the resolver is the shell counterpart of
# modelID()/validateModel() in internal/e2e/versions.go, and the two must agree
# or a workflow's reported model won't be the one its Go tests ran. The
# expectations below are the same ones TestModelIDOverride and TestValidateModel
# assert on the Go side, plus the fallback that reads the pin out of
# versions.go — the one behaviour that has no Go equivalent.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
RESOLVE="$ROOT/scripts/resolve-model.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  failures=$((failures + 1))
}

# Every case runs with a clean slate: the caller's own E2E_MODEL* must not leak
# into an expectation about the committed pin. Args of the form VAR=value become
# env assignments; anything else is passed through to the resolver, so a case
# reads as `resolve E2E_MODEL=x claude-code` regardless of order.
resolve() {
  local assignments=() args=() a
  for a in "$@"; do
    case "$a" in
      *=*) assignments+=("$a") ;;
      *)   args+=("$a") ;;
    esac
  done
  env -u E2E_MODEL -u E2E_MODEL_OPENCODE -u E2E_MODEL_CLAUDE_CODE \
      -u E2E_MODEL_CODEX -u E2E_MODEL_COPILOT -u E2E_MODEL_PI \
      -u VERSIONS_GO \
      ${assignments[@]+"${assignments[@]}"} \
      "$RESOLVE" ${args[@]+"${args[@]}"}
}

assert_model() {
  local want="$1" desc="$2"; shift 2
  local got
  if ! got=$(resolve "$@" 2>&1); then
    fail "$desc: exited non-zero: $got"
    return
  fi
  [ "$got" = "$want" ] || fail "$desc: got '$got', want '$want'"
}

assert_rejected() {
  local desc="$1"; shift
  local got
  if got=$(resolve "$@" 2>&1); then
    fail "$desc: expected a non-zero exit, got '$got'"
  fi
}

# --- the committed pin (no override) ---------------------------------------
# Read from versions.go, never hardcoded: the pin moves whenever the gateway
# renames a variant (#184), and this asserts the resolution mechanism, not which
# model happens to be pinned today.
pin_for() {
  awk '/^var modelIDs = map\[string\]string\{/{f=1;next} /^\}/{f=0} f' \
    "$ROOT/internal/e2e/versions.go" \
    | grep "\"$1\"" | sed -E 's/.*:[[:space:]]*"([^"]+)".*/\1/' | head -1
}
PIN_OPENCODE=$(pin_for opencode)
PIN_CLAUDE=$(pin_for claude-code)
PIN_PI=$(pin_for pi)
[ -n "$PIN_OPENCODE" ] || fail "could not read the opencode pin out of versions.go"

assert_model "$PIN_OPENCODE" "default harness is opencode"
assert_model "$PIN_OPENCODE" "explicit opencode" opencode
assert_model "$PIN_CLAUDE" "claude-code pin" claude-code
assert_model "$PIN_PI" "pi pin" pi

# --- precedence ------------------------------------------------------------
assert_model "vendor/candidate-1" "cross-harness override" \
  E2E_MODEL=vendor/candidate-1 codex
assert_model "claude-haiku-4-5" "per-harness override wins over cross-harness" \
  E2E_MODEL=vendor/candidate-1 E2E_MODEL_CLAUDE_CODE=claude-haiku-4-5 claude-code
assert_model "vendor/candidate-1" "per-harness override must not leak to others" \
  E2E_MODEL=vendor/candidate-1 E2E_MODEL_CLAUDE_CODE=claude-haiku-4-5 opencode
assert_model "$PIN_OPENCODE" "an empty override falls through to the pin" \
  E2E_MODEL= E2E_MODEL_OPENCODE= opencode

# --- claude-code's launchable-model constraint -----------------------------
assert_rejected "claude-code rejects a non-sonnet/haiku cross-harness override" \
  E2E_MODEL=zai-org/GLM-5.2 claude-code
assert_rejected "claude-code rejects a non-sonnet/haiku per-harness override" \
  E2E_MODEL_CLAUDE_CODE=claude-opus-5 claude-code
assert_model "claude-sonnet-5-20260101" "claude-code accepts a sonnet variant" \
  E2E_MODEL=claude-sonnet-5-20260101 claude-code
# The constraint is claude-code's alone — the rest share one gateway.
for h in opencode codex copilot pi; do
  assert_model "claude-opus-5" "$h is unconstrained" E2E_MODEL=claude-opus-5 "$h"
done

# --- failure modes ---------------------------------------------------------
assert_rejected "unknown harness has no pin to fall back to" nope
assert_rejected "missing versions.go is a loud failure" \
  VERSIONS_GO="$TMP/absent.go" opencode

# A VERSIONS_GO elsewhere is honoured — the escape hatch for a caller reading
# pins from a checkout the resolver does not sit inside.
cat > "$TMP/versions.go" <<'EOF'
var modelIDs = map[string]string{
	"opencode": "elsewhere/model-9",
}
EOF
assert_model "elsewhere/model-9" "VERSIONS_GO points at another checkout" \
  VERSIONS_GO="$TMP/versions.go" opencode

if [ "$failures" -ne 0 ]; then
  printf '\n%d check(s) failed\n' "$failures" >&2
  exit 1
fi
echo "all resolve-model.sh checks passed"
