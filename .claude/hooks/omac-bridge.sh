#!/usr/bin/env bash
# omac Claude Code bridge hook
# ============================
#
# This is the Claude Code-side counterpart to the OpenCode plugin
# (.opencode/plugins/omac-multidir.ts). It wires Claude Code, running inside
# `omac start claude` / `omac serve claude`, to the omac control plane so the
# session's skills come online and are surfaced to the agent.
#
# It implements the common omac bridge interface (see
# docs/contributing/serve-spec.md and openspec specs/agent-bridge):
#
#   1. Activate / reload on session start — POST /__omac__/activate {dir}
#   2. Surface skills to the agent        — emit the skills manifest as the
#                                           SessionStart hook's additionalContext
#   3. Deactivate on session end          — POST /__omac__/deactivate {dir}
#
# Per-session skill env (OMAC_*_BASE) is NOT injected here: Claude Code has no
# per-shell env hook equivalent to OpenCode's `shell.env`. omac already exports
# the flat single-directory aliases (OMAC_<MOUNT>_BASE, OMAC_G_<MOUNT>_BASE)
# into the process environment at launch, which Claude Code inherits, so skills
# that read those vars resolve for the active directory (the documented
# fallback in design.md Decision 3).
#
# Degradation: if OMAC_CONTROL_BASE is unset (Claude Code not running under
# omac), every branch is a no-op. The hook is inert and safe to ship anywhere.
#
# Requirements: bash, curl, and a JSON tool. We prefer `jq`; if it is missing
# we degrade gracefully (activation still happens; the manifest is skipped).

set -euo pipefail

control_base="${OMAC_CONTROL_BASE:-}"
control_base="${control_base%/}"

# Claude Code delivers the hook payload as JSON on stdin. We read it once.
payload="$(cat || true)"

have_jq=0
if command -v jq >/dev/null 2>&1; then
  have_jq=1
fi

json_get() {
  # json_get <stdin-json> <jq-filter> ; prints "" on any failure.
  local json="$1" filter="$2"
  if [ "$have_jq" -eq 1 ]; then
    printf '%s' "$json" | jq -r "$filter // empty" 2>/dev/null || true
  fi
}

# The hook event name ("SessionStart", "SessionEnd", …) and the session's
# working directory both come from the payload. `cwd` is the directory the
# Claude Code session was launched in.
event="$(json_get "$payload" '.hook_event_name')"
dir="$(json_get "$payload" '.cwd')"
if [ -z "$dir" ]; then
  dir="${CLAUDE_PROJECT_DIR:-$PWD}"
fi

# Inert when not running under omac.
if [ -z "$control_base" ]; then
  exit 0
fi

control_post() {
  # control_post <path> <json-body> ; echoes the response body (or "").
  local path="$1" body="$2"
  curl -fsS -X POST "${control_base}${path}" \
    -H 'content-type: application/json' \
    -d "$body" 2>/dev/null || true
}

# Render the manifest the same way the OpenCode plugin does, from the activate
# response JSON. Output is the markdown block injected as agent context.
render_manifest() {
  local manifest="$1"
  [ "$have_jq" -eq 1 ] || return 0
  # The active harness's own skills dir (omac injects this; Claude → .claude/skills).
  local skills_dir="${OMAC_HARNESS_SKILLS_DIR:-.claude/skills}"
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
  # Emit a SessionStart hook result that adds the manifest to the agent's
  # context. Claude Code reads hookSpecificOutput.additionalContext for
  # SessionStart hooks.
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
    # SessionStart (and any other start-like event): activate the directory and
    # surface the resulting skills manifest.
    manifest="$(control_post "/__omac__/activate" "{\"dir\":\"${dir}\"}")"
    if [ -n "$manifest" ]; then
      context="$(render_manifest "$manifest")"
      emit_context "$context"
    fi
    exit 0
    ;;
esac
