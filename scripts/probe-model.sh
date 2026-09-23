#!/usr/bin/env bash
# Pick a model the gateway will actually serve, and print it on stdout.
#
# The SKAINET gateway serves ONE name variant at a time — the plain name or a
# "-TEE" suffixed one — and flips between them without notice. On 2026-07-29 it
# stopped accepting plain "zai-org/GLM-5.2" and every non-opencode `llm` stage
# went red about ten minutes into each leg (issue #184). This script front-loads
# that discovery so a renamed model costs seconds, not a whole matrix, and
# self-heals instead of failing.
#
# Candidate order (see modelCandidates() in internal/e2e/versions.go, which this
# mirrors): the resolved model, its variant flip, then each configured fallback
# and its flip. Duplicates dropped. The flip is bidirectional, so pinning either
# variant survives the next flip in either direction.
#
# Two-stage selection:
#   1. GET $base/models (OpenAI-compatible) lists what the gateway advertises.
#      Candidates on that list are probed FIRST. The listing only orders the
#      candidates — it never excludes one, because a stale or partial listing
#      must not veto a name that actually works.
#   2. A 1-token POST $base/chat/completions is the authority. First HTTP 200
#      wins. This is what the gateway 422s on, so it is what gets asked.
#
# On a run that never needs a fallback, the chain is still checked against the
# listing and a warning is emitted if none of it is advertised — otherwise a
# retired fallback stays invisible until the run that depends on it.
#
# Usage:
#   scripts/probe-model.sh [harness]              # default harness: opencode
#   scripts/probe-model.sh [harness] --github-output
#
# --github-output additionally writes model= / fallback= / candidates= /
# responses_wire= to $GITHUB_OUTPUT so a workflow can consume the result
# without re-parsing stdout.
#
# Environment:
#   SKAINET_INTERNAL | LLM_API_BASE | MODEL_PROBE_BASE   gateway base URL
#   SKAINET_TOKEN    | LLM_API_KEY  | MODEL_PROBE_TOKEN  bearer token
#   E2E_MODEL_FALLBACK   override the fallback chain (comma/space separated)
#   E2E_MODEL[_<HARNESS>]  the model override itself — see resolve-model.sh
#
# Without credentials this is a pure passthrough: it prints what
# resolve-model.sh resolved and exits 0. Model-free jobs and local runs must not
# need network access to name a model.
#
# claude-code is never probed: it talks to ANTHROPIC_BASE_URL, not this gateway,
# so its model is not on the listing and a gateway fallback would silently
# retarget the billed Anthropic account.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
harness="opencode"
github_output=0
for arg in "$@"; do
  case "$arg" in
    --github-output) github_output=1 ;;
    -*) echo "probe-model: unknown flag '$arg'" >&2; exit 2 ;;
    *) harness="$arg" ;;
  esac
done

base="${MODEL_PROBE_BASE:-${SKAINET_INTERNAL:-${LLM_API_BASE:-}}}"
token="${MODEL_PROBE_TOKEN:-${SKAINET_TOKEN:-${LLM_API_KEY:-}}}"
responses_wire="not-probed" # set by assess_responses on any gateway path
responses_model=""          # set by assess_responses; passthroughs fill it in

# The resolved model, before any gateway consideration. Not guarded with
# `|| true`: an unresolvable model is a real misconfiguration and the caller
# needs to see it, exactly as resolve-model.sh's own callers do.
primary="$("$HERE/resolve-model.sh" "$harness")"

emit() {
  local model="$1" fallback="$2" tried="$3"
  if [ "$github_output" = "1" ] && [ -n "${GITHUB_OUTPUT:-}" ]; then
    {
      printf 'model<<OMAC_DELIM\n%s\nOMAC_DELIM\n' "$model"
      printf 'fallback<<OMAC_DELIM\n%s\nOMAC_DELIM\n' "$fallback"
      printf 'candidates<<OMAC_DELIM\n%s\nOMAC_DELIM\n' "$tried"
      printf 'responses_wire<<OMAC_DELIM\n%s\nOMAC_DELIM\n' "$responses_wire"
      printf 'responses_model<<OMAC_DELIM\n%s\nOMAC_DELIM\n' "$responses_model"
    } >> "$GITHUB_OUTPUT"
  fi
  printf '%s\n' "$model"
}

# Two-track selection: the CHAT track picks a model the way the script always
# did (chat/completions probes); the RESPONSES track exists because codex can
# only drive OpenAI-compat /responses (see codexConfig in internal/e2e) and the
# two routes' health diverges per model — a July run had a chat-dead /
# responses-fine model at the same time a responses-dead / chat-fine one was
# being served. The workflow wires E2E_MODEL_CODEX from responses_model, so
# codex picks a responses-healthy model independently of the chat harnesses.
#
# responses_probe_one echoes "<verdict> <code>" for ONE model on /responses
# (both stdout because it runs in a command substitution), with a small retry
# budget so a single blip is not misread as a route outage.
responses_probe_one() {
  local m="$1" attempt=1 max=2 code verdict rbody
  rbody=/tmp/probe-model-responses.$$
  while :; do
    code=$(curl -s -o "$rbody" -w '%{http_code}' --max-time 30 \
      -X POST "${base%/}/responses" \
      -H "authorization: Bearer $token" \
      -H 'content-type: application/json' \
      -d "{\"model\":\"${m}\",\"input\":[{\"role\":\"user\",\"content\":[{\"type\":\"input_text\",\"text\":\"ping\"}]}]}" \
      2>/dev/null) || code="000"
    verdict=$(classify_code "$code")
    [ "$verdict" != "transient" ] && break
    if [ "$attempt" -ge "$max" ]; then break; fi
    echo "probe-model: responses wire hit a transient gateway error (HTTP $code) — retrying ($((attempt + 1))/$max)" >&2
    attempt=$((attempt + 1)); sleep $((attempt * 2))
  done
  rm -f "$rbody"
  printf '%s %s' "$verdict" "$code"
}

# assess_responses <chat-selected-model>: sets the track outputs for the emit.
#   responses_wire  ok | unhealthy | not-probed (parenthetically: passthrough)
#   responses_model the model codex should run (healthy on /responses),
#                   falling back to the chat selection with a loud warning
#                   when nothing on the chain answers on that wire.
assess_responses() {
  local selected="$1" c verdict code
  read -r verdict code <<EOF
$(responses_probe_one "$selected")
EOF
  if [ "$verdict" = "ok" ]; then
    responses_wire="ok"
    responses_model="$selected"
    return 0
  fi
  responses_wire="unhealthy"
  echo "probe-model: responses wire unhealthy for '$selected' (HTTP $code) — scanning the chain for a responses-healthy fallback" >&2
  for c in $ordered; do
    case " $selected " in *" $c "*) continue ;; esac
    read -r verdict code <<EOF
$(responses_probe_one "$c")
EOF
    if [ "$verdict" = "ok" ]; then
      responses_model="$c"
      echo "::warning title=Responses wire fallback::the gateway's /responses route is unhealthy for '$selected' (last HTTP $code); codex will run '$c' instead. The other gateway harnesses keep '$selected' on the chat wire." >&2
      return 0
    fi
    echo "probe-model: responses probe inconclusive for '$c' (HTTP $code) — trying the next candidate" >&2
  done
  responses_model="$selected"
  echo "::warning title=Responses wire unhealthy::no candidate answered on /responses (last HTTP $code); codex will attempt '$selected' — its model-driven legs are expected to fail until the route recovers. This does NOT invalidate the chat-wire legs." >&2
}


# --- passthrough paths (no network) -----------------------------------------

if [ "$harness" = "claude-code" ]; then
  echo "probe-model: claude-code is on its own provider — not probing the gateway" >&2
  emit "$primary" false "$primary"
  exit 0
fi

if [ -z "$base" ] || [ -z "$token" ]; then
  echo "probe-model: no gateway base URL / token on the env — using '$primary' unprobed" >&2
  responses_model="$primary"
  emit "$primary" false "$primary"
  exit 0
fi

# --- candidate list ---------------------------------------------------------

flip_tee() {
  case "$1" in
    "") printf '' ;;
    *-TEE) printf '%s' "${1%-TEE}" ;;
    *) printf '%s-TEE' "$1" ;;
  esac
}

# Fallback chain: E2E_MODEL_FALLBACK, else fallbackModels in versions.go — the
# single source of truth shared with the Go tests.
fallbacks() {
  if [ -n "${E2E_MODEL_FALLBACK:-}" ]; then
    printf '%s' "$E2E_MODEL_FALLBACK" | tr ',\t\n' '   '
    return
  fi
  local versions_go="${VERSIONS_GO:-$HERE/../internal/e2e/versions.go}"
  [ -f "$versions_go" ] || return 0
  awk '/^var fallbackModels = \[\]string\{/{ \
         line=$0; sub(/.*\{/,"",line); sub(/\}.*/,"",line); print line; exit }' \
    "$versions_go" | tr -d '"' | tr ',' ' '
}

# The fallback chain is only reached once the primary is gone, so a retired
# fallback rots invisibly: "moonshotai/Kimi-K3" sat in the chain unserved for
# days while every run stayed green on the primary, and the rot would only have
# surfaced at the moment the backup was actually needed. Report it on the way
# past instead.
#
# Requires ALL of the chain to be unadvertised before saying anything. A single
# missing name proves nothing — the listing under-advertises, which is why it
# only orders candidates and never excludes them (see stage 1) — but an entire
# chain missing means there is no backup left.
warn_if_chain_rotted() {
  [ "$listing_ok" = "1" ] || return 0
  local chain f
  chain="$(fallbacks)"
  [ -n "$(printf '%s' "$chain" | tr -d '[:space:]')" ] || return 0
  for f in $chain; do
    case " $served " in *" $f "*) return 0 ;; esac
  done
  echo "::warning title=Fallback chain unserved::the gateway advertises none of the configured fallback models ($chain). This run is unaffected — '$primary' resolved — but there would be no working backup if it disappeared. Refresh fallbackModels in internal/e2e/versions.go." >&2
}

# Candidates are grouped into PRIORITY TIERS, one per configured model: tier 0
# is the primary and its variant flip, tier 1 the first fallback and its flip,
# and so on. The listing may reorder within a tier but never across tiers —
# a backup must only ever be reached after the primary has actually been
# refused by chat/completions. Letting the listing promote a tier would silently
# downgrade a run whenever the gateway under-advertises a working model, which
# is the same failure mode as the advertised-but-broken case below.
tiers=""   # newline-separated; each line is one tier's space-separated models
seen_all=" "
for m in "$primary" $(fallbacks); do
  tier=""
  for v in "$m" "$(flip_tee "$m")"; do
    [ -z "$v" ] && continue
    case "$seen_all" in *" $v "*) continue ;; esac
    seen_all="$seen_all$v "
    tier="${tier:+$tier }$v"
  done
  [ -n "$tier" ] && tiers="${tiers}${tier}
"
done

# --- stage 1: what does the gateway advertise? ------------------------------

served=""
listing_ok=0
if listing=$(curl -sS --max-time 30 "${base%/}/models" \
      -H "authorization: Bearer $token" 2>/dev/null); then
  # jq if available (it is on GH runners), else a grep fallback so the script
  # still works on a bare machine.
  if command -v jq >/dev/null 2>&1; then
    served=$(printf '%s' "$listing" | jq -r '(.data // [])[]? | .id // empty' 2>/dev/null | tr '\n' ' ' || true)
  else
    served=$(printf '%s' "$listing" | grep -oE '"id"[[:space:]]*:[[:space:]]*"[^"]+"' \
      | sed -E 's/.*"([^"]+)"$/\1/' | tr '\n' ' ' || true)
  fi
  [ -n "$(printf '%s' "$served" | tr -d '[:space:]')" ] && listing_ok=1
fi
if [ "$listing_ok" = "1" ]; then
  echo "probe-model: gateway advertises: $served" >&2
else
  echo "probe-model: model listing unavailable — probing all candidates directly" >&2
fi

# Within each tier, advertised names go first; the listing orders, it never
# excludes, and it never reorders across tiers.
ordered=""
while IFS= read -r tier; do
  [ -z "$tier" ] && continue
  if [ "$listing_ok" = "1" ]; then
    for m in $tier; do
      case " $served " in *" $m "*) ordered="${ordered:+$ordered }$m" ;; esac
    done
  fi
  for m in $tier; do
    case " $ordered " in *" $m "*) ;; *) ordered="${ordered:+$ordered }$m" ;; esac
  done
done <<EOF
$tiers
EOF

# --- stage 2: chat/completions is the authority -----------------------------

endpoint="${base%/}/chat/completions"
tried=""
inconclusive=""

# A non-200 means one of two very different things, and conflating them is how a
# 30-second GPU-cluster hiccup silently changes which model a run tested:
#
#   unavailable — the gateway refuses this NAME (422 model_unavailable, 404).
#                 Move on; this is what the variant flip and fallback are for.
#   transient   — the gateway is unwell (5xx, 429, connection failure). Says
#                 nothing about the name. Retry it; never fall back on it.
#
# Observed in the wild: "500 An internal error occurred while invoking the
# model. The model inference should recover automatically within a few minutes."
classify_code() {
  case "$1" in
    200) printf 'ok' ;;
    408|409|425|429|500|502|503|504|000) printf 'transient' ;;
    *) printf 'unavailable' ;;
  esac
}

probe_candidate() {
  # Echoes "<verdict> <last-http-code>" for one candidate, retrying while
  # transient. Both on stdout because this runs in a command substitution — a
  # variable set here would not survive back to the caller.
  local candidate="$1" attempt=1 max=3 code verdict
  while :; do
    code=$(curl -s -o /tmp/probe-model-resp.$$ -w '%{http_code}' --max-time 30 \
      -X POST "$endpoint" \
      -H "authorization: Bearer $token" \
      -H 'content-type: application/json' \
      -d "{\"model\":\"${candidate}\",\"messages\":[{\"role\":\"user\",\"content\":\"ping\"}],\"max_tokens\":1}" \
      2>/dev/null) || code="000"
    verdict=$(classify_code "$code")
    [ "$verdict" != "transient" ] && break
    if [ "$attempt" -ge "$max" ]; then break; fi
    echo "probe-model: '$candidate' hit a transient gateway error (HTTP $code) — retrying ($((attempt + 1))/$max)" >&2
    attempt=$((attempt + 1))
    sleep $((attempt * 2))
  done
  printf '%s %s' "$verdict" "$code"
}

# Tier 0 — the intended model and its name variant. Definitive
# unavailability (422/404) is what the variant flip and fallback exist for.
#
# A TRANSIENT result used to veto the whole fallback chain (no family swap
# onto a briefly unwell gateway — the #184 rule). Since then the wire-health
# evidence showed the two routes' health diverges per model while chat routes
# for OTHER models stay served, so a persistently-transient primary tier no
# longer blocks the run: the loop falls through with an annotated swap and a
# fatal exit only if literally nothing answers (the end-of-loop path).
# "Persistent" = the full retry budget on both tier-0 names failed.
primary_tier=" $primary $(flip_tee "$primary") "
primary_transient=0

for candidate in $ordered; do
  tried="${tried:+$tried,}$candidate"
  read -r verdict code <<EOF
$(probe_candidate "$candidate")
EOF
  if [ "$verdict" = "ok" ]; then
    rm -f /tmp/probe-model-resp.$$
    if [ "$candidate" = "$primary" ] || [ "$candidate" = "$(flip_tee "$primary")" ]; then
      # Same model, gateway naming quirk — safe to substitute quietly.
      [ "$candidate" != "$primary" ] && \
        echo "probe-model: '$primary' not served; using name variant '$candidate'" >&2
        warn_if_chain_rotted
        assess_responses "$candidate"
        emit "$candidate" false "$tried"
      else
        # A different family. A green run on this means something weaker than a
        # green run on the intended model, so say so where CI will surface it.
        if [ "$primary_transient" = "1" ]; then
          echo "::warning title=Chat route on '$primary' unwell::the pinned model's chat/completions route kept returning transient errors for its whole retry budget, so this run uses '$candidate' on the chat wire instead. Results are attributed to '$candidate', NOT '$primary'." >&2
        else
          echo "::warning title=Model fallback::'$primary' is not served by the gateway; fell back to '$candidate'. Results are for $candidate, NOT $primary." >&2
        fi
        assess_responses "$candidate"
        emit "$candidate" true "$tried"
    fi
    exit 0
  fi
  # A curl timeout / connection failure leaves no response file, so this used
  # to kill the script: sed exits 2 on a missing file and set -euo pipefail
  # treats the substitution's status as fatal (observed as a silent exit 2
  # mid-preflight, 2026-09-23).
  msg=$(sed -n 's/.*"message"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' /tmp/probe-model-resp.$$ 2>/dev/null | head -1 || true)
  rm -f /tmp/probe-model-resp.$$
  if [ "$verdict" = "transient" ]; then
    inconclusive="${inconclusive:+$inconclusive,}$candidate"
    case "$primary_tier" in *" $candidate "*) primary_transient=1 ;; esac
    echo "probe-model: '$candidate' inconclusive — gateway unwell (HTTP $code)${msg:+: $msg}" >&2
  else
    echo "probe-model: '$candidate' rejected (HTTP $code)${msg:+: $msg}" >&2
  fi
done

if [ -n "$inconclusive" ]; then
  # Distinguished from "the models are gone" so whoever reads the log knows to
  # re-run rather than go hunting for a renamed model.
  echo "::error title=Gateway unhealthy::could not verify any model — these were inconclusive (transient gateway errors, not name rejections): $inconclusive. Tried: $tried. Re-run once the gateway recovers." >&2
  exit 1
fi

echo "::error title=No usable model::none of these models was accepted by the gateway: $tried" >&2
[ "$listing_ok" = "1" ] && echo "probe-model: gateway advertises: $served" >&2
echo "probe-model: check the model provider, SKAINET_TOKEN validity, and the pins in internal/e2e/versions.go" >&2
exit 1
