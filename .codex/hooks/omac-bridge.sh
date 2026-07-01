#!/usr/bin/env bash
# omac Codex CLI bridge hook
# ==========================
#
# This is the Codex CLI-side counterpart to the Claude Code bridge
# (.claude/hooks/omac-bridge.sh). It wires Codex, running inside
# `omac start codex` / `omac serve codex`, to the omac control plane so the
# session's skills come online and are surfaced to the agent.
#
# It implements the common omac bridge interface:
#   1. Activate on session start — POST /__omac__/activate {dir}
#   2. Surface skills to the agent — emit the skills manifest as the
#      SessionStart hook's additionalContext
#
# NOTE: No deactivate on Stop. Codex's Stop event is turn-scoped (fires every
# turn end), NOT session-scoped. Codex has no SessionEnd event. Deactivate
# relies on omac's TTL-based reaper (same as a crash). See design.md.
#
# Degradation: if OMAC_CONTROL_BASE is unset (Codex not running under omac),
# every branch is a no-op. The hook is inert and safe to ship anywhere.
#
# Requirements: bash, curl, and jq (degrades gracefully without jq).

set -euo pipefail

control_base="${OMAC_CONTROL_BASE:-}"
control_base="${control_base%/}"

# Codex delivers the hook payload as JSON on stdin. We read it once.
payload="$(cat || true)"

have_jq=0
if command -v jq >/dev/null 2>&1; then
  have_jq=1
fi

json_get() {
  local json="$1" filter="$2"
  if [ "$have_jq" -eq 1 ]; then
    printf '%s' "$json" | jq -r "$filter // empty" 2>/dev/null || true
  fi
}

# The hook event name ("SessionStart", "Stop", …) and the session's
# working directory both come from the payload.
event="$(json_get "$payload" '.hook_event_name')"
dir="$(json_get "$payload" '.cwd')"
if [ -z "$dir" ]; then
  dir="${CODEX_PROJECT_DIR:-$PWD}"
fi

# Inert when not running under omac.
if [ -z "$control_base" ]; then
  exit 0
fi

control_post() {
  local path="$1" body="$2"
  curl -fsS -X POST "${control_base}${path}" \
    -H 'content-type: application/json' \
    -d "$body" 2>/dev/null || true
}

# Render the manifest from the activate response JSON.
render_manifest() {
  local manifest="$1"
  [ "$have_jq" -eq 1 ] || return 0
  local skills_dir="${OMAC_HARNESS_SKILLS_DIR:-.codex/skills}"
  printf '%s' "$manifest" | jq -r --arg skillsdir "$skills_dir" '
    def skillsarr: (.skills // []);
    "## omac skills available in this workspace\n" +
    "\n" +
    "You can call the following skill HTTP endpoints. Each `base` is the root URL for that skill'"'"'s sidecar; append the skill'"'"'s documented path.\n" +
    "\n" +
    "This workspace'"'"'s project directory is: `" + (.dir // "") + "`\n" +
    (if (skillsarr | map(select(.scope == "global" and .state == "ready")) | length) > 0 then
      "\n" +
      "IMPORTANT: **global** skills are shared by every workspace. When a global skill writes into the project (e.g. the marketplace installing a skill), you MUST pass this workspace'"'"'s project directory explicitly — for the marketplace use `\"target_path\": \"" + (.dir // "") + "/" + $skillsdir + "\"` (the active harness'"'"'s skills directory) in the /install request body. Otherwise it installs into the wrong directory.\n"
     else "" end) +
    "\n" +
    ( skillsarr | sort_by(.name) | map(
        . as $sk |
        if .state == "ready" and (.base // "") != "" then
          "- **" + .name + "** (" + (.scope // "") + ") — ready — base: `" + .base + "`"
        elif .state == "pending-credentials" then
          "- **" + .name + "** (" + (.scope // "") + ") — UNAVAILABLE (missing credentials: " + ((.missing // []) | join(", ")) + "). Run in your own terminal: " + ((.missing // []) | map("omac secrets set " + ($sk.name) + " " + .) | join(" ; "))
        elif .state == "broken" then
          "- **" + .name + "** (" + (.scope // "") + ") — BROKEN: " + (.detail // "see omac logs")
        else empty end
      ) | join("\n") )
  ' 2>/dev/null || true
}

emit_context() {
  local context="$1"
  [ -n "$context" ] || return 0
  if [ "$have_jq" -eq 1 ]; then
    jq -n --arg ctx "$context" '{
      hookSpecificOutput: {
        hookEventName: "SessionStart",
        additionalContext: $ctx
      }
    }'
  fi
}

case "$event" in
  SessionStart)
    manifest="$(control_post "/__omac__/activate" "{\"dir\":\"${dir}\"}")"
    if [ -n "$manifest" ]; then
      context="$(render_manifest "$manifest")"
      emit_context "$context"
    fi
    exit 0
    ;;
  *)
    # Stop and other events: no-op. Codex Stop is turn-scoped; deactivate
    # relies on omac's TTL reaper. See design.md.
    exit 0
    ;;
esac
