#!/usr/bin/env bash
# Tests for scripts/probe-model.sh against a stub gateway.
#
# A stub, not the real gateway: the whole point of the script is behaviour when
# the gateway renames or drops a model, and those states can't be produced on
# demand upstream. The stub reproduces exactly the two shapes that matter —
# an OpenAI-style GET /models listing, and a POST /chat/completions that 422s
# with "Requested model name ... is currently not available" for anything it
# does not serve, which is what broke CI in #184.
set -euo pipefail

ROOT="$(git rev-parse --show-toplevel)"
PROBE="$ROOT/scripts/probe-model.sh"
TMP="$(mktemp -d)"
STUB_PID=""
cleanup() {
  [ -n "$STUB_PID" ] && kill "$STUB_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

failures=0
fail() { printf 'FAIL: %s\n' "$*" >&2; failures=$((failures + 1)); }

cat > "$TMP/stub.py" <<'PY'
"""Stub gateway. STUB_LISTED / STUB_ACCEPTED are space-separated model ids.

STUB_LIST_STATUS != 200 makes the listing endpoint fail, so the test can prove
the listing never vetoes a model the chat endpoint accepts.
"""
import json, os
from http.server import BaseHTTPRequestHandler, HTTPServer

LISTED = os.environ.get("STUB_LISTED", "").split()
ACCEPTED = os.environ.get("STUB_ACCEPTED", "").split()
LIST_STATUS = int(os.environ.get("STUB_LIST_STATUS", "200"))
# Models that answer with a transient 500 rather than a name rejection — the
# real "internal error occurred while invoking the model" case.
TRANSIENT = os.environ.get("STUB_TRANSIENT", "").split()
HITS = os.environ.get("STUB_HITS", "/tmp/stub-hits")
# Cumulative across every stub restart, so the per-case trace can report how
# many requests the run has made so far.
ALL_HITS = os.environ.get("STUB_ALL_HITS", "")


class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _record(self, what):
        with open(HITS, "a") as f:
            f.write(what + "\n")
        if ALL_HITS:
            with open(ALL_HITS, "a") as f:
                f.write(what + "\n")

    def _send(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        self._record("GET " + self.path)
        if not self.path.endswith("/models"):
            return self._send(404, {"error": "no such path"})
        if LIST_STATUS != 200:
            return self._send(LIST_STATUS, {"error": {"message": "listing down"}})
        self._send(200, {"data": [{"id": m} for m in LISTED]})

    def do_POST(self):
        n = int(self.headers.get("content-length", 0))
        req = json.loads(self.rfile.read(n) or b"{}")
        model = req.get("model", "")
        self._record("POST " + model)
        if model in TRANSIENT:
            return self._send(500, {"error": {"message":
                "An internal error occurred while invoking the model. The model "
                "inference should recover automatically within a few minutes."}})
        if model in ACCEPTED:
            return self._send(200, {"choices": [{"message": {"content": "ok"}}]})
        self._send(422, {"error": {"message":
            "Requested model name '%s' is currently not available. "
            "Supported model names are: {%s}" % (model, ", ".join(repr(m) for m in LISTED))}})


HTTPServer(("127.0.0.1", int(os.environ["STUB_PORT"])), H).serve_forever()
PY

PORT=8931
HITS="$TMP/hits"

# --- trace ------------------------------------------------------------------
# This file runs in ~25s on Linux and ~700s on a macOS CI runner, with no
# intermediate output to say where the difference goes. The trace prints one
# line per case — elapsed seconds, cumulative stub requests, stub restarts, and
# the seconds spent inside start_stub — so a CI log localizes the cost without a
# second run. See scripts/diag-loopback-timing.sh for the mechanism breakdown.
ALL_HITS="$TMP/all-hits"
: > "$ALL_HITS"
stub_starts=0
stub_secs=0
mark() {
  printf 'TRACE t=%ss reqs=%s stubs=%s stub_secs=%s :: %s\n' \
    "$SECONDS" "$(wc -l < "$ALL_HITS" | tr -d ' ')" "$stub_starts" "$stub_secs" "$1"
}

start_stub() {
  local t0=$SECONDS
  [ -n "$STUB_PID" ] && { kill "$STUB_PID" 2>/dev/null || true; wait "$STUB_PID" 2>/dev/null || true; }
  : > "$HITS"
  STUB_LISTED="$1" STUB_ACCEPTED="$2" STUB_LIST_STATUS="${3:-200}" \
    STUB_TRANSIENT="${4:-}" \
    STUB_PORT="$PORT" STUB_HITS="$HITS" STUB_ALL_HITS="$ALL_HITS" \
    python3 "$TMP/stub.py" &
  STUB_PID=$!
  local up=0 _
  for _ in $(seq 1 40); do
    if curl -s -o /dev/null "http://127.0.0.1:$PORT/models" 2>/dev/null; then up=1; break; fi
    sleep 0.1
  done
  [ "$up" = "1" ] || { echo "stub did not come up" >&2; exit 1; }
  stub_starts=$((stub_starts + 1))
  stub_secs=$((stub_secs + SECONDS - t0))
  # Discard the readiness request: the "did this run touch the network at all"
  # assertions below must see only what the probe itself sent.
  : > "$HITS"
}

# Runs the probe against the stub with a clean model env. VAR=value args become
# env assignments; a bare arg is the harness.
probe() {
  local assignments=() args=() a
  for a in "$@"; do
    case "$a" in *=*) assignments+=("$a") ;; *) args+=("$a") ;; esac
  done
  env -u E2E_MODEL -u E2E_MODEL_OPENCODE -u E2E_MODEL_CLAUDE_CODE \
      -u E2E_MODEL_CODEX -u E2E_MODEL_COPILOT -u E2E_MODEL_PI \
      -u E2E_MODEL_FALLBACK -u LLM_API_BASE -u LLM_API_KEY -u GITHUB_OUTPUT \
      -u SKAINET_TOKEN -u SKAINET_INTERNAL \
      MODEL_PROBE_BASE="http://127.0.0.1:$PORT" MODEL_PROBE_TOKEN=stub-token \
      ${assignments[@]+"${assignments[@]}"} \
      "$PROBE" ${args[@]+"${args[@]}"}
}

PIN=$(awk '/^var modelIDs = map\[string\]string\{/{f=1;next} /^\}/{f=0} f' \
  "$ROOT/internal/e2e/versions.go" | grep '"opencode"' | sed -E 's/.*:[[:space:]]*"([^"]+)".*/\1/')
case "$PIN" in
  *-TEE) PIN_FLIP="${PIN%-TEE}" ;;
  *)     PIN_FLIP="$PIN-TEE" ;;
esac
echo "committed pin: $PIN (variant: $PIN_FLIP)"

mark "case 1 — the pin itself is served"
# --- 1. the pin itself is served -------------------------------------------
start_stub "$PIN" "$PIN"
got=$(probe opencode 2>/dev/null || true)
[ "$got" = "$PIN" ] || fail "pin served: got '$got', want '$PIN'"

mark "case 2 — only the OTHER variant is served (the #184 breakage, both ways)"
# --- 2. only the OTHER variant is served (the #184 breakage, both ways) -----
start_stub "$PIN_FLIP" "$PIN_FLIP"
got=$(probe opencode 2>/dev/null || true)
[ "$got" = "$PIN_FLIP" ] || fail "variant flip: got '$got', want '$PIN_FLIP'"
# A same-model variant flip must NOT be announced as a fallback.
err=$(probe opencode 2>&1 >/dev/null || true)
case "$err" in
  *"title=Model fallback"*) fail "variant flip was mislabelled as a cross-model fallback" ;;
esac

mark "case 3 — neither variant served, fallback wins, loudly"
# --- 3. neither variant served, fallback wins, loudly ----------------------
start_stub "fb/model" "fb/model"
out=$(probe E2E_MODEL_FALLBACK=fb/model opencode 2>"$TMP/err" || true)
[ "$out" = "fb/model" ] || fail "fallback: got '$out', want 'fb/model'"
grep -q "title=Model fallback" "$TMP/err" || fail "cross-model fallback was not announced with a warning"
grep -q "NOT $PIN" "$TMP/err" || fail "fallback warning does not name the model that was intended"

mark "case 4 — nothing usable -> loud failure, non-zero"
# --- 4. nothing usable -> loud failure, non-zero ---------------------------
start_stub "other/thing" ""
if out=$(probe E2E_MODEL_FALLBACK=fb/model opencode 2>"$TMP/err"); then
  fail "no usable model: expected a non-zero exit, got '$out'"
fi
grep -q "title=No usable model" "$TMP/err" || fail "exhausted candidates did not emit an error annotation"
grep -q "other/thing" "$TMP/err" || fail "failure diagnostic omits what the gateway does serve"

mark "case 5 — listing endpoint down must not veto a working model"
# --- 5. listing endpoint down must not veto a working model ---------------
start_stub "$PIN" "$PIN" 500
got=$(probe opencode 2>/dev/null || true)
[ "$got" = "$PIN" ] || fail "listing down: got '$got', want '$PIN' (chat probe must still decide)"

mark "case 6 — advertised but rejected by chat -> chat is the authority"
# --- 6. advertised but rejected by chat -> chat is the authority -----------
# The listing claims the pin, chat only accepts the variant. Picking from the
# listing alone would strand the run on a model that 422s on every real call.
start_stub "$PIN" "$PIN_FLIP"
got=$(probe opencode 2>/dev/null || true)
[ "$got" = "$PIN_FLIP" ] || fail "advertised-but-broken: got '$got', want '$PIN_FLIP'"

mark "case 6b — the listing must never promote a fallback over the primary"
# --- 6b. the listing must never promote a fallback over the primary ---------
# The gateway under-advertises the primary but still serves it, while the
# fallback is both advertised and served. Ordering by the listing alone would
# jump straight to the backup and silently downgrade the run — the primary has
# to be refused by chat before a backup is reachable.
start_stub "fb/model" "$PIN fb/model"
got=$(probe E2E_MODEL_FALLBACK=fb/model opencode 2>/dev/null || true)
[ "$got" = "$PIN" ] || fail "listing promoted a fallback over a working primary: got '$got', want '$PIN'"
grep -q "POST $PIN" "$HITS" || fail "the primary was never probed before falling back"

mark "case 7 — claude-code is never probed"
# --- 7. claude-code is never probed ----------------------------------------
start_stub "$PIN" "$PIN"
got=$(probe claude-code 2>/dev/null || true)
CC_PIN=$(awk '/^var modelIDs = map\[string\]string\{/{f=1;next} /^\}/{f=0} f' \
  "$ROOT/internal/e2e/versions.go" | grep '"claude-code"' | sed -E 's/.*:[[:space:]]*"([^"]+)".*/\1/')
[ "$got" = "$CC_PIN" ] || fail "claude-code: got '$got', want '$CC_PIN'"
if [ -s "$HITS" ]; then
  fail "claude-code probed the gateway ($(tr '\n' ';' < "$HITS")) — it is on another provider"
fi

mark "case 8 — --github-output contract"
# --- 8. --github-output contract ------------------------------------------
start_stub "fb/model" "fb/model"
: > "$TMP/gh-out"
probe GITHUB_OUTPUT="$TMP/gh-out" E2E_MODEL_FALLBACK=fb/model opencode --github-output >/dev/null 2>&1 || true
grep -q '^model=fb/model$' "$TMP/gh-out" || fail "--github-output did not write model="
grep -q '^fallback=true$' "$TMP/gh-out" || fail "--github-output did not flag the fallback"
# candidates= is probe ORDER, not the logical candidate order: an advertised
# model is tried first, so the chosen one can legitimately lead. Assert both the
# intended model and the winner appear, not their positions.
cands=$(sed -n 's/^candidates=//p' "$TMP/gh-out")
case "$cands" in
  *"$PIN"*) ;;
  *) fail "--github-output candidates='$cands' omits the intended model '$PIN'" ;;
esac
case "$cands" in
  *fb/model*) ;;
  *) fail "--github-output candidates='$cands' omits the model actually used" ;;
esac

start_stub "$PIN" "$PIN"
: > "$TMP/gh-out"
probe GITHUB_OUTPUT="$TMP/gh-out" opencode --github-output >/dev/null 2>&1 || true
grep -q '^fallback=false$' "$TMP/gh-out" || fail "a primary hit must report fallback=false"

mark "case 9 — no creds -> passthrough, no network"
# --- 9. no creds -> passthrough, no network -------------------------------
start_stub "$PIN" "$PIN"
got=$(env -u SKAINET_TOKEN -u SKAINET_INTERNAL -u LLM_API_BASE -u LLM_API_KEY \
       -u MODEL_PROBE_BASE -u MODEL_PROBE_TOKEN -u E2E_MODEL \
       "$PROBE" opencode 2>/dev/null || true)
[ "$got" = "$PIN" ] || fail "no creds: got '$got', want the unprobed pin '$PIN'"
if [ -s "$HITS" ]; then
  fail "credential-less run still hit the network ($(tr '\n' ';' < "$HITS"))"
fi

mark "case 10 — a transient primary must NOT trigger a fallback"
# --- 10. a transient primary must NOT trigger a fallback -------------------
# Observed on 2026-07-29: the gateway answered 500 "internal error occurred
# while invoking the model ... should recover automatically". That says nothing
# about the model name, so swapping families over it would change what the run
# tested for no reason. Must fail as infra, and must never emit the fallback.
start_stub "$PIN fb/model" "fb/model" 200 "$PIN $PIN_FLIP"
if out=$(probe E2E_MODEL_FALLBACK=fb/model opencode 2>"$TMP/err"); then
  fail "transient primary: expected a non-zero exit, got '$out'"
fi
grep -q "title=Gateway unhealthy" "$TMP/err" || fail "a transient primary was not reported as a gateway-health problem"
grep -q "title=Model fallback" "$TMP/err" && fail "a transient primary must not fall back to another family"
# It must have retried rather than given up on the first 500.
[ "$(grep -c "POST $PIN\$" "$HITS")" -ge 2 ] || fail "a transient result was not retried"

mark "case 11 — a DEFINITIVELY unavailable primary still falls back"
# --- 11. a DEFINITIVELY unavailable primary still falls back ---------------
# The distinction that makes case 10 safe: a name rejection is exactly what the
# backup exists for, so this one must still substitute.
start_stub "fb/model" "fb/model"
out=$(probe E2E_MODEL_FALLBACK=fb/model opencode 2>"$TMP/err" || true)
[ "$out" = "fb/model" ] || fail "unavailable primary should still fall back: got '$out'"
grep -q "title=Model fallback" "$TMP/err" || fail "definitive unavailability did not announce the fallback"

mark "case 12 — a transient blip that clears is not a failure"
# --- 12. a transient blip that clears is not a failure ---------------------
# Only the flip is transient; the primary is served. The primary is probed
# first, so this must succeed without ever reaching the transient name.
start_stub "$PIN" "$PIN" 200 "$PIN_FLIP"
got=$(probe opencode 2>/dev/null || true)
[ "$got" = "$PIN" ] || fail "served primary alongside a transient variant: got '$got', want '$PIN'"

mark "case 13 — a rotted fallback chain is reported on an otherwise-fine run"
# --- 13. a rotted fallback chain is reported on an otherwise-fine run -------
# The chain is only reached once the primary is gone, so an unserved fallback
# stays invisible for as long as the primary keeps working — which is how
# "moonshotai/Kimi-K3" survived in the chain long after the gateway dropped it.
# A run that resolves normally must still say the backup is missing.
start_stub "$PIN" "$PIN"
got=$(probe E2E_MODEL_FALLBACK=gone/model opencode 2>"$TMP/err" || true)
[ "$got" = "$PIN" ] || fail "rotted chain must not change the resolved model: got '$got', want '$PIN'"
grep -q "title=Fallback chain unserved" "$TMP/err" || fail "an entirely unadvertised fallback chain was not reported"
grep -q "gone/model" "$TMP/err" || fail "the rotted-chain warning does not name the chain"
# It is a warning, not a failure: the run itself is fine.
probe E2E_MODEL_FALLBACK=gone/model opencode >/dev/null 2>&1 \
  || fail "a rotted chain must not fail a run whose primary resolved"

# An advertised chain is healthy — no warning.
start_stub "$PIN alive/model" "$PIN"
probe E2E_MODEL_FALLBACK=alive/model opencode 2>"$TMP/err" >/dev/null || true
grep -q "title=Fallback chain unserved" "$TMP/err" && fail "a served fallback chain must not be reported as rotted"

# One live entry is enough; the listing under-advertises, so only a chain with
# nothing left in it is worth reporting.
start_stub "$PIN alive/model" "$PIN"
probe E2E_MODEL_FALLBACK="gone/model alive/model" opencode 2>"$TMP/err" >/dev/null || true
grep -q "title=Fallback chain unserved" "$TMP/err" && fail "a chain with one advertised entry must not be reported as rotted"

# With the listing down there is no evidence either way, so say nothing rather
# than cry wolf on every run the listing endpoint is flaky.
start_stub "$PIN" "$PIN" 500
probe E2E_MODEL_FALLBACK=gone/model opencode 2>"$TMP/err" >/dev/null || true
grep -q "title=Fallback chain unserved" "$TMP/err" && fail "the chain must not be judged rotted when the listing is unavailable"

mark "done"
if [ "$failures" -ne 0 ]; then
  printf '\n%d check(s) failed\n' "$failures" >&2
  exit 1
fi
echo "all probe-model.sh checks passed"
