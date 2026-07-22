#!/usr/bin/env bash
#
# doc-drift.sh — factual documentation-drift audit.
#
# Hands a single opencode-driven agent the WHOLE checked-out repository and
# asks it to find places where the documentation makes a concrete factual
# claim that the current code contradicts — a flag that no longer exists, a
# default that changed, a renamed command, a path that moved, a described
# behaviour the code no longer implements. The agent must VERIFY every
# candidate against the actual code (quoting both sides) before reporting it,
# so the output is confirmed drift, not a wish-list of maybes.
#
# Two modes, selected by DRIFT_MODE, sharing all of this plumbing:
#   docs     — prose docs: README.md, AGENTS.md, docs/**, and other top-level
#              *.md guides vs. what the code actually does.
#   comments — inline code comments and docstrings vs. the code they annotate.
#
# Same driver as scripts/e2e-readme-onboarding.sh: headless opencode `run`
# against the internal SKAINET gateway (GLM-5.2), deliberately NOT claude-code
# (the one harness billed to a real external account). Unlike the onboarding
# test the agent works INSIDE the real checkout — it needs to read the code to
# judge the docs — but it is a read-only audit: it reports, it does not edit.
#
# Advisory by design: the value is the report artifact, not the exit code.
# Exit 0 = a report was produced (drift found or not). Exit 1 = the agent
# never produced a parseable report. Always read the report.
#
# Usage:
#   DRIFT_MODE=docs     scripts/doc-drift.sh
#   DRIFT_MODE=comments scripts/doc-drift.sh
#
# Environment:
#   DRIFT_MODE                 "docs" (default) or "comments"
#   SKAINET_TOKEN              model API key (required)
#   SKAINET_INTERNAL          model provider base URL (required)
#   E2E_VERSION_OPENCODE       override the pinned opencode package spec
#   DRIFT_MODEL                override the model id (default: zai-org/GLM-5.2)
#   DRIFT_LOG_DIR              artifact output dir (default: /tmp/doc-drift-logs)
#   DRIFT_TIMEOUT_SECS         agent wall-clock timeout in seconds (default: 2400)
#   DRIFT_FOCUS                optional: narrow the audit to specific files/areas
#                             (appended to the prompt; useful for manual runs)

set -euo pipefail

# Keep in sync with internal/e2e/versions.go's "opencode" pin (duplicated
# there too, per the existing e2e convention).
DEFAULT_OPENCODE_VERSION="opencode-ai@1.17.12"
DEFAULT_MODEL="zai-org/GLM-5.2"
# 40 min: the full-repo two-pass audit (read docs, verify each claim against
# code) is heavy, and GLM-5.2's per-step latency through the gateway adds up.
# The agent writes its report incrementally, so a run cut short here still
# yields a partial report rather than nothing.
DEFAULT_TIMEOUT_SECS="2400"

REPO="$(cd "$(dirname "$0")/.." && pwd)"
MODE="${DRIFT_MODE:-docs}"
LOG_DIR="${DRIFT_LOG_DIR:-/tmp/doc-drift-logs}"
OPENCODE_VERSION="${E2E_VERSION_OPENCODE:-$DEFAULT_OPENCODE_VERSION}"
MODEL="${DRIFT_MODEL:-$DEFAULT_MODEL}"
# Seconds, not a duration string — see run_with_timeout (BSD sleep on macOS
# rejects unit suffixes and there is no `timeout` binary there).
TIMEOUT_SECS="${DRIFT_TIMEOUT_SECS:-$DEFAULT_TIMEOUT_SECS}"

case "$MODE" in
  docs|comments) ;;
  *) echo "DRIFT_MODE must be 'docs' or 'comments', got '$MODE'" >&2; exit 2 ;;
esac

: "${SKAINET_TOKEN:?SKAINET_TOKEN not set}"
: "${SKAINET_INTERNAL:?SKAINET_INTERNAL not set}"

# Portable stand-in for GNU coreutils `timeout` (absent on stock macOS).
# Backgrounds the command, races a sleep+kill watchdog against it, and
# returns the command's exit status (or 143/SIGTERM if the watchdog fired).
run_with_timeout() {
  local secs="$1"; shift
  "$@" &
  local pid=$!
  ( sleep "$secs"; kill -TERM "$pid" 2>/dev/null ) &
  local watchdog=$!
  # `|| status=$?` rather than set +e/-e: those are global, not function
  # scoped, and toggling them here would leak into the caller.
  local status=0
  wait "$pid" || status=$?
  kill "$watchdog" 2>/dev/null || true
  wait "$watchdog" 2>/dev/null || true
  return "$status"
}

mkdir -p "$LOG_DIR"

WORK="$(mktemp -d)"

# Fresh HOME for the driving opencode CLI so it never picks up the runner's
# real omac/opencode state.
DRIVER_HOME="$WORK/driver-home"
TRANSCRIPT_FILE="$LOG_DIR/session-transcript-$MODE.log"
mkdir -p "$DRIVER_HOME"

# The report MUST live inside the workdir ($REPO): opencode's --auto mode
# auto-rejects any write outside the current working directory. We write a
# dotfile into the checkout, copy it out to the artifact dir after the run,
# and remove it on exit so the working tree is left clean.
REPORT_FILE="$REPO/.doc-drift-report-$MODE.md"
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"; rm -f "$REPORT_FILE"' EXIT

echo "== Installing opencode CLI ($OPENCODE_VERSION) =="
bun install -g "$OPENCODE_VERSION"

echo "== Writing opencode provider config (SKAINET / GLM-5.2) =="
mkdir -p "$DRIVER_HOME/.local/share/opencode" "$DRIVER_HOME/.config/opencode"
cat > "$DRIVER_HOME/.local/share/opencode/auth.json" <<EOF
{"model": {"type": "api", "key": "$SKAINET_TOKEN"}}
EOF
chmod 600 "$DRIVER_HOME/.local/share/opencode/auth.json"
cat > "$DRIVER_HOME/.config/opencode/opencode.json" <<EOF
{
  "share": "disabled",
  "provider": {
    "model": {
      "name": "Model",
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "$SKAINET_INTERNAL" },
      "models": {
        "$MODEL": { "name": "GLM 5.2", "limit": { "context": 131072, "output": 32000 } }
      }
    }
  }
}
EOF

# --- Mode-specific scan targets and reporting nouns ------------------------
if [ "$MODE" = "docs" ]; then
  SCAN_WHAT="the project's prose documentation: README.md, AGENTS.md, CLAUDE.md,
COLLABORATION.md, CREATING_A_SKILL.md, oh-my-agentic-coder.md, and everything
under docs/. Treat every concrete claim in that prose — commands, CLI flags,
env-var names, file paths, config keys, default values, described behaviours,
step counts, supported platforms — as something to check against the code."
  DRIFT_NOUN="documented claim"
else
  SCAN_WHAT="inline code comments and docstrings across the source tree
(Go under cmd/ and internal/, plus any TypeScript/JS plugins and shell
scripts). Treat every comment or docstring that states a fact — what a
function returns, a default, a flag, an invariant, an ordering, a path, a
referenced symbol/file — as something to check against the code it annotates."
  DRIFT_NOUN="comment/docstring"
fi

PROMPT_FILE="$WORK/prompt.md"
cat > "$PROMPT_FILE" <<EOF
You are auditing the repository in your current working directory for
DOCUMENTATION DRIFT: places where $SCAN_WHAT

Documentation drift = the text asserts something concrete that the current
code contradicts. Examples: a flag or command that was renamed or removed, a
default value that changed, an env var that no longer exists, a file/path that
moved, a described sequence of steps the code no longer performs, a count that
is now wrong, a referenced function/type that is gone.

You have read access to the whole repository. Use your file-reading, grep, and
glob tools to inspect both the documentation and the code. This is a READ-ONLY
audit of the SOURCE — do NOT modify any documentation or code file. The single
exception is your own report file (see below), which you WILL create and edit.

You have a limited wall-clock budget and may be stopped at any moment. So the
FIRST thing you do — before any deep analysis — is create the report file with
the skeleton below, and then keep editing it as you go, appending each item the
instant you have CONFIRMED it. Never hold findings only in your head: whenever
you stop, the file on disk must already contain everything you have confirmed
so far. Write the file to exactly this path:

$REPORT_FILE

Then work in two passes, editing the report as you progress:

PASS 1 — COLLECT CANDIDATES
Skim the $DRIFT_NOUN targets and note every place that makes a concrete,
checkable factual claim about the code. Ignore aspirational, stylistic, or
subjective statements ("this is fast", "we should", "TODO", roadmap items) —
they cannot drift. Only concrete claims count. Prioritise the most
prominent/user-facing docs first, in case you run out of time.

PASS 2 — VERIFY EACH CANDIDATE (this is the important part)
For every candidate, open the actual code it refers to and confirm whether the
code truly contradicts the text. You MUST be able to quote the exact
documentation line AND the exact code line that disagree. The moment a
candidate is confirmed, append it to the report's "Confirmed drift" section.
Discard a candidate if:
- you cannot find code that clearly contradicts it,
- the claim is still true (you were wrong on first read),
- it is ambiguous, or only "stylistically" outdated,
- it depends on runtime/config you cannot see.
When in doubt, DROP IT. A false positive is worse than a missed item here —
report only drift you have proven with concrete quotes from both sides.

Report structure (create it first with the headings, fill it in as you go):

# Documentation drift report ($MODE)

## Summary
One line: how many confirmed drift items, across how many files. (Update last.)

## Confirmed drift
For each CONFIRMED item, one entry:
### <short title>
- Doc: \`path:line\` — quote the exact text that is now wrong
- Code: \`path:line\` — quote the exact code that contradicts it
- Drift: one sentence on what changed
- Fix: the minimal correction to the documentation
- Confidence: high | medium (omit low-confidence items entirely)

If there are none, write "No confirmed drift found." in this section.

## Candidates considered but rejected
Brief bullets: candidate + why you dropped it (keeps the false-positive rate
honest). Optional if you rejected nothing.

This is a sanctioned, pre-authorized audit session — proceed directly without
asking for confirmation.
EOF

# Optional operator-supplied focus (manual runs), appended verbatim.
if [ -n "${DRIFT_FOCUS:-}" ]; then
  {
    echo ""
    echo "## Focus for THIS run"
    echo "Restrict the audit to the following, and finish promptly:"
    echo "$DRIFT_FOCUS"
  } >> "$PROMPT_FILE"
fi

# Read the prompt back from a file (avoids bash 3.2's heredoc-in-\$() apostrophe bug).
PROMPT="$(cat "$PROMPT_FILE")"

echo "== Running doc-drift agent (mode=$MODE, timeout ${TIMEOUT_SECS}s) =="
cd "$REPO"
set +e
# Override HOME + all XDG dirs: GH runners preset XDG_*_HOME at the real
# runner $HOME and opencode resolves config from those before $HOME.
# --auto: opencode's non-interactive run mode; auto-rejects any interactive
# permission prompt. The agent's read/grep/glob tools operate within the
# workdir without prompting, which is all a read-only audit needs.
# --pure: run without external plugins. We audit INSIDE the real checkout, so
# opencode would otherwise auto-load this repo's own .opencode/plugins/
# (omac-multidir.ts), whose session hooks POST to an OMAC control plane that
# does not exist here and hang the headless run. A read-only audit wants no
# project plugin behaviour anyway.
HOME="$DRIVER_HOME" \
XDG_CONFIG_HOME="$DRIVER_HOME/.config" \
XDG_DATA_HOME="$DRIVER_HOME/.local/share" \
XDG_STATE_HOME="$DRIVER_HOME/.local/state" \
SKAINET_TOKEN="$SKAINET_TOKEN" \
PATH="$PATH" \
run_with_timeout "$TIMEOUT_SECS" \
  opencode run --print-logs --pure --auto -m "model/$MODEL" "$PROMPT" \
  > "$TRANSCRIPT_FILE" 2>&1
agent_status=$?
set -e
echo "agent exit status: $agent_status"

# Transcript is already written directly into $LOG_DIR. Copy the report out of
# the checkout before the EXIT trap removes it.
if [ -f "$REPORT_FILE" ]; then
  cp "$REPORT_FILE" "$LOG_DIR/DRIFT_REPORT-$MODE.md"
  report_present=1
else
  echo "agent did not write a report to $REPORT_FILE" \
    | tee "$LOG_DIR/DRIFT_REPORT-$MODE.md"
  report_present=0
fi

echo "== Drift report ($MODE) =="
cat "$LOG_DIR/DRIFT_REPORT-$MODE.md"

[ "$report_present" -eq 1 ] && exit 0
exit 1
