#!/usr/bin/env bash
# Resolve the model id a CI step should use, and print it on stdout.
#
# Single source of truth for the shell side of the model configuration, the
# counterpart of modelID() in internal/e2e/versions.go. Both apply the same
# precedence, so a workflow's shell steps and its Go tests never disagree
# about which model a run used:
#
#   1. E2E_MODEL_<HARNESS>  — per-harness override (harness upper-cased, '-'
#                             → '_'), e.g. E2E_MODEL_CLAUDE_CODE
#   2. E2E_MODEL            — cross-harness override; what the workflows'
#                             single `model` workflow_dispatch input sets
#   3. internal/e2e/versions.go's modelIDs map — the committed pin
#
# Usage:
#   scripts/resolve-model.sh [harness]      # default harness: opencode
#
# The default matters: the workflows that summarise with an LLM (release
# notes, drift summaries) and the strix security scan are not tied to a
# harness — they all use opencode's entry, the plain gateway model name.
#
# Exits non-zero with a diagnostic on stderr when no model can be resolved,
# so a caller under `set -e` fails loudly rather than sending an empty model
# name to the gateway.

set -euo pipefail

harness="${1:-opencode}"

# claude-code is the one harness bound to a single provider, and only that
# provider's sonnet/haiku tiers are launchable — anything else is rejected by
# the CLI with an opaque startup error. Reject it here instead, so a bad model
# input fails a workflow in seconds rather than after the install and agent
# turn. Mirrors validateModel() in internal/e2e/versions.go.
reject_unlaunchable() {
  local model="$1"
  [ "$harness" = "claude-code" ] || return 0
  case "$(printf '%s' "$model" | tr '[:upper:]' '[:lower:]')" in
    *sonnet*|*haiku*) return 0 ;;
  esac
  echo "resolve-model: claude-code cannot run model '$model' — it accepts only sonnet or haiku models (override it separately via E2E_MODEL_CLAUDE_CODE / the claude_code_model workflow input)" >&2
  exit 1
}

# Reject model values that contain characters outside a safe allowlist.
# Model IDs are provider/name pairs like "zai-org/GLM-5.2" or
# "claude-sonnet-5"; only alphanumerics, hyphens, dots, underscores,
# slashes, and colons are needed. Anything else (newlines, shell
# metacharacters, quotes) is a sign of injection, not a valid model name.
reject_unsafe_model() {
  local model="$1"
  case "$model" in
    *[!A-Za-z0-9._/:@-]*)
      echo "resolve-model: model value contains unsafe characters: '$model'" >&2
      exit 1
      ;;
  esac
}

# 1. Per-harness override.
# Two tr passes, not one combined set: mixing a character class with a literal
# in a single SET1 is unspecified, and macOS ships BSD tr.
per_harness_var="E2E_MODEL_$(printf '%s' "$harness" | tr '[:lower:]' '[:upper:]' | tr '-' '_')"
per_harness="${!per_harness_var:-}"
if [ -n "$per_harness" ]; then
  reject_unsafe_model "$per_harness"
  reject_unlaunchable "$per_harness"
  printf '%s\n' "$per_harness"
  exit 0
fi

# 2. Cross-harness override.
if [ -n "${E2E_MODEL:-}" ]; then
  reject_unsafe_model "$E2E_MODEL"
  reject_unlaunchable "$E2E_MODEL"
  printf '%s\n' "$E2E_MODEL"
  exit 0
fi

# 3. The committed pin. VERSIONS_GO lets a caller whose checkout is not the
# repo root point at the file (security-scan.yml checks out into target/).
repo="$(cd "$(dirname "$0")/.." && pwd)"
versions_go="${VERSIONS_GO:-$repo/internal/e2e/versions.go}"
if [ ! -f "$versions_go" ]; then
  echo "resolve-model: $versions_go not found (set VERSIONS_GO or pass an explicit model via E2E_MODEL)" >&2
  exit 1
fi
pinned=$(awk '/^var modelIDs = map\[string\]string\{/{f=1;next} /^\}/{f=0} f' "$versions_go" \
  | grep "\"$harness\"" \
  | sed -E 's/.*:[[:space:]]*"([^"]+)".*/\1/' \
  | head -1)
if [ -z "$pinned" ]; then
  echo "resolve-model: no modelIDs entry for harness '$harness' in $versions_go" >&2
  exit 1
fi
printf '%s\n' "$pinned"
