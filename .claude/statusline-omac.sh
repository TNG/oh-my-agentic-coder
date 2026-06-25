#!/usr/bin/env bash
# omac status-line indicator for Claude Code
# ============================================
#
# Prints a persistent bottom-bar marker showing whether Claude Code is
# running under omac (and which version) or bare.
#
# Reads ONLY the OMAC_VERSION env var that omac injects at launch. It does
# NOT parse the JSON payload Claude Code pipes on stdin, so future changes
# to the statusLine JSON schema cannot break it. When the var is unset
# (bare harness), it prints "bare".
#
# Registered via .claude/settings.json:
#   "statusLine": { "type": "command", "command": "~/.claude/statusline-omac.sh" }
#
# See https://docs.claude.com/en/docs/claude-code/statusline

# Drain stdin (Claude Code sends JSON we intentionally ignore).
cat >/dev/null 2>&1

if [ -n "${OMAC_VERSION:-}" ]; then
  printf 'omac v%s' "$OMAC_VERSION"
else
  printf 'bare'
fi
