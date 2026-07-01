#!/usr/bin/env bash
# omac Copilot CLI bridge hook
# =============================
#
# This is the Copilot CLI-side counterpart to the Claude Code bridge
# (.claude/hooks/omac-bridge.sh). It wires Copilot, running inside
# `omac start copilot` / `omac serve copilot`, to the omac control plane so
# the session's skills come online and are surfaced to the agent.
#
# It implements the common omac bridge interface:
#   1. Activate on session start — POST /__omac__/activate {dir}
#   2. Surface skills to the agent — emit the skills manifest as the
#      SessionStart hook's additionalContext
#   3. Deactivate on session end — POST /__omac__/deactivate {dir}
#
# Registered with PascalCase event names (SessionStart, SessionEnd) so the
# payload uses snake_case fields (hook_event_name, session_id, cwd) —
# compatible with the claude bridge's dispatch logic. The output shape uses
# hookSpecificOutput.additionalContext nesting (VS Code-compatible format).
#
# Degradation: if OMAC_CONTROL_BASE is unset (Copilot not running under omac),
# every branch is a no-op. The hook is inert and safe to ship anywhere.
#
# Requirements: bash, curl, and jq (degrades gracefully without jq).

set -euo pipefail

control_base="${OMAC_CONTROL_BASE:-}"
control_base="${control_base%/}"

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

event="$(json_get "$payload" '.hook_event_name')"
dir="$(json_get "$payload" '.cwd')"
if [ -z "$dir" ]; then
  dir="${COPILOT_PROJECT_DIR:-$PWD}"
fi

if [ -z "$control_base" ]; then
  exit 0
fi

control_post() {
  local path="$1" body="$2"
  curl -fsS -X POST "${control_base}${path}" \
    -H 'content-type: application/json' \
    -d "$body" 2>/dev/null || true
}

render_manifest() {
  local manifest="$1"
  [ "$have_jq" -eq 1 ] || return 0
  local skills_dir="${OMAC_HARNESS_SKILLS_DIR:-.copilot/skills}"
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
  SessionEnd)
    control_post "/__omac__/deactivate" "{\"dir\":\"${dir}\"}" >/dev/null
    exit 0
    ;;
  *)
    manifest="$(control_post "/__omac__/activate" "{\"dir\":\"${dir}\"}")"
    if [ -n "$manifest" ]; then
      context="$(render_manifest "$manifest")"
      emit_context "$context"
    fi
    exit 0
    ;;
esac
