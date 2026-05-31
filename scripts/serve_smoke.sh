#!/usr/bin/env bash
#
# serve_smoke.sh — end-to-end smoke test for `omac serve`'s control plane,
# facade routing, isolation, and a real skill round trip.
#
# Requires an environment where loopback TCP connect is permitted (i.e. NOT
# the restricted sandbox). Needs: go, curl, python3 (for the echo-rest skill).
#
# Usage:
#   scripts/serve_smoke.sh
#
# Exit code 0 = all checks passed; non-zero = first failure.

set -u
REPO="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO"

PASS=0
FAIL=0
ok()   { echo "  [ok]   $1"; PASS=$((PASS+1)); }
bad()  { echo "  [FAIL] $1"; FAIL=$((FAIL+1)); }
hdr()  { echo; echo "== $1 =="; }

# --- preflight: loopback must work ---
hdr "preflight"
if ! python3 - <<'PY' 2>/dev/null
import socket
s=socket.socket(); s.bind(("127.0.0.1",0)); a=s.getsockname()
s.listen(1)
c=socket.create_connection(a,timeout=2); c.close(); s.close()
PY
then
  echo "  loopback TCP connect is blocked in this environment; cannot run."
  echo "  (lift the restriction and re-run)"
  exit 99
fi
ok "loopback works"

# --- build ---
hdr "build"
go build -o /tmp/omac-smoke ./cmd/omac || { bad "go build"; exit 1; }
ok "built omac"
OMAC=/tmp/omac-smoke

# --- stage two workdirs + one global skill ---
hdr "stage fixtures"
TMP="$(mktemp -d)"
export HOME="$TMP/home"; export XDG_CONFIG_HOME="$TMP/xdg"
mkdir -p "$HOME" "$XDG_CONFIG_HOME"
ROOT="$TMP/projects"; mkdir -p "$ROOT"

# Two project dirs, each with a same-named pending-credentials skill.
for P in projA projB; do
  D="$ROOT/$P/.opencode/skills/demo"
  mkdir -p "$D"
  cat > "$D/omac.yaml" <<YAML
name: demo
sidecar:
  command: ["true"]
  secrets:
    - name: API_TOKEN
      required: true
YAML
done

# A real global skill: copy the bundled echo-rest into the user-global root
# and register it so cold start spawns it under /__global__/.
GSRC="$REPO/.opencode/skills/echo-rest"
if [ -d "$GSRC" ]; then
  GDST="$XDG_CONFIG_HOME/opencode/skills/echo-rest"
  mkdir -p "$(dirname "$GDST")"
  cp -R "$GSRC" "$GDST"
  # Register it globally (echo-rest's secrets/config all have defaults).
  "$OMAC" --workdir "$ROOT/projA" register echo-rest --no-secrets >/dev/null 2>&1 || true
  ok "staged global echo-rest skill"
else
  echo "  [warn] echo-rest skill not found; skipping global/round-trip tiers"
fi

# --- start serve --no-inner (control plane only) ---
hdr "start omac serve --no-inner"
"$OMAC" serve --no-inner --no-sandbox \
  --root "$ROOT" \
  --control-addr 127.0.0.1:0 \
  --verbose >"$TMP/serve.log" 2>&1 &
SVPID=$!
trap 'kill $SVPID 2>/dev/null; rm -rf "$TMP" /tmp/omac-smoke' EXIT
sleep 1.2

CTRL="$(grep -oE 'control plane on http://127.0.0.1:[0-9]+' "$TMP/serve.log" | grep -oE 'http://127.0.0.1:[0-9]+')"
FAC_PORT="$(grep -oE 'facade tcp=127.0.0.1:[0-9]+' "$TMP/serve.log" | grep -oE '[0-9]+$')"
if [ -z "$CTRL" ]; then bad "could not read control URL"; cat "$TMP/serve.log"; exit 1; fi
ok "control plane: $CTRL  facade: 127.0.0.1:$FAC_PORT"

jqget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(eval('d'+sys.argv[1]))" "$1" 2>/dev/null; }

# --- Tier 1: control plane ---
hdr "Tier 1: control plane"
MA="$(curl -s -m 5 -X POST "$CTRL/__omac__/activate" -d "{\"dir\":\"$ROOT/projA\"}")"
TOKA="$(printf '%s' "$MA" | jqget "['dir_token']")"
[ -n "$TOKA" ] && ok "activate projA -> token $TOKA" || { bad "activate projA ($MA)"; }

MA2="$(curl -s -m 5 -X POST "$CTRL/__omac__/activate" -d "{\"dir\":\"$ROOT/projA\"}")"
TOKA2="$(printf '%s' "$MA2" | jqget "['dir_token']")"
[ "$TOKA" = "$TOKA2" ] && ok "activate idempotent (same token)" || bad "token changed: $TOKA vs $TOKA2"

STATE="$(printf '%s' "$MA" | jqget "['state']")"
[ "$STATE" = "active_partial" ] && ok "state=active_partial (pending creds)" || bad "state=$STATE want active_partial"

DIRS="$(curl -s -m 5 "$CTRL/__omac__/dirs")"
printf '%s' "$DIRS" | grep -q "$ROOT/projA" && ok "/dirs lists projA" || bad "/dirs missing projA ($DIRS)"

# roots policy: a dir outside --root must be refused
OUT="$(curl -s -m 5 -o /dev/null -w '%{http_code}' -X POST "$CTRL/__omac__/activate" -d "{\"dir\":\"$TMP/outside\"}")"
mkdir -p "$TMP/outside"
OUT2="$(curl -s -m 5 -X POST "$CTRL/__omac__/activate" -d "{\"dir\":\"$TMP/outside\"}")"
printf '%s' "$OUT2" | grep -qi 'not under any allowed' && ok "roots policy rejects outside dir" || bad "roots policy not enforced ($OUT2)"

# --- Tier 2: isolation (distinct tokens) ---
hdr "Tier 2: isolation"
MB="$(curl -s -m 5 -X POST "$CTRL/__omac__/activate" -d "{\"dir\":\"$ROOT/projB\"}")"
TOKB="$(printf '%s' "$MB" | jqget "['dir_token']")"
[ -n "$TOKB" ] && [ "$TOKA" != "$TOKB" ] && ok "projA/projB distinct tokens" || bad "tokens not distinct: $TOKA / $TOKB"

# pending-credentials route returns 409 through the facade
CODE="$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$FAC_PORT/$TOKA/demo/x")"
[ "$CODE" = "409" ] && ok "pending-credentials route -> 409" || bad "pending route code=$CODE want 409"

# a never-minted token cannot reach anything
CODE="$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$FAC_PORT/deadbeefdeadbeefdeadbeefdeadbeef/demo/x")"
[ "$CODE" = "404" ] && ok "unminted token -> 404 (isolation)" || bad "unminted token code=$CODE want 404"

# --- Tier 3: real skill round trip (echo-rest global) ---
hdr "Tier 3: echo-rest round trip"
GLOB="$(curl -s -m 5 "$CTRL/__omac__/global")"
if printf '%s' "$GLOB" | grep -q 'echo-rest\|"mount":"echo"'; then
  # echo-rest mounts at "echo"; POST /echo round-trips JSON.
  RESP="$(curl -s -m 5 -X POST "http://127.0.0.1:$FAC_PORT/__global__/echo/echo" \
            -H 'content-type: application/json' -d '{"hello":"world"}')"
  printf '%s' "$RESP" | grep -q 'hello' && ok "global echo-rest round trip" || bad "echo round trip ($RESP)"
else
  echo "  [skip] echo-rest not registered globally ($GLOB)"
fi

# --- Tier 4: deactivate ---
hdr "Tier 4: deactivate"
curl -s -m 5 -X POST "$CTRL/__omac__/deactivate" -d "{\"dir\":\"$ROOT/projA\"}" >/dev/null
DIRS="$(curl -s -m 5 "$CTRL/__omac__/dirs")"
printf '%s' "$DIRS" | grep -q "$ROOT/projA" && bad "projA still listed after deactivate" || ok "deactivate removed projA"
CODE="$(curl -s -m 5 -o /dev/null -w '%{http_code}' "http://127.0.0.1:$FAC_PORT/$TOKA/demo/x")"
[ "$CODE" = "404" ] && ok "route gone after deactivate" || bad "route still up code=$CODE"

# --- summary ---
hdr "summary"
echo "  PASS=$PASS  FAIL=$FAIL"
[ "$FAIL" -eq 0 ] && { echo "  ALL GREEN"; exit 0; } || { echo "  FAILURES PRESENT"; exit 1; }
