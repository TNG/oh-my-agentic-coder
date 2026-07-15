#!/usr/bin/env bash
#
# e2e-readme-onboarding.sh — README-completeness e2e test.
#
# Independent of internal/e2e (the harness×skill matrix suite): that suite
# hands each harness a fixed 3-command script inside an already-configured
# omac sandbox. This test hands a single agent NOTHING but README.md — no
# other file from this repository — and asks it to self-serve a normal
# omac dev setup exactly as a brand-new developer would. It verifies the
# README itself is complete, not that omac's plumbing works.
#
# The agent runs unsandboxed, directly on the (already-ephemeral) CI
# runner — omac isn't installed yet, so there is nothing to sandbox it
# with. It gets full shell + internet access, deliberately, since a real
# new developer has both.
#
# Driver: opencode (headless `run`, non-interactive by design — no
# permission-bypass flag needed) against the internal SKAINET gateway,
# same GLM-5.2 model used by the rest of the harness matrix. Deliberately
# NOT claude-code: this session is open-ended (real installs, retries) and
# claude-code is the one harness billed against a real external Anthropic
# account.
#
# Usage:
#   scripts/e2e-readme-onboarding.sh
#
# Environment:
#   SKAINET_TOKEN                 model API key (required)
#   SKAINET_INTERNAL               model provider base URL (required)
#   E2E_VERSION_OPENCODE           override the pinned opencode package spec
#   E2E_ONBOARDING_MODEL           override the model id (default: zai-org/GLM-5.2)
#   E2E_LOG_DIR                    artifact output dir (default: /tmp/e2e-readme-logs)
#   E2E_README_REF                 git ref to extract README.md from (default: HEAD)
#   E2E_ONBOARDING_TIMEOUT_SECS    agent wall-clock timeout in seconds (default: 1200)
#
# Exit code 0 = README was sufficient (doctor healthy + report written).
# Exit code 1 = gaps blocked a working setup, or the agent never produced
#               a report. Either way, the report/log artifacts are what
#               matters here, not the exit code alone — always inspect them.

set -euo pipefail

# Keep this in sync with internal/e2e/versions.go's "opencode" pin
# (duplicated there too, per existing e2e.yml convention).
DEFAULT_OPENCODE_VERSION="opencode-ai@1.17.12"
DEFAULT_MODEL="zai-org/GLM-5.2"
DEFAULT_TIMEOUT_SECS="1200"

REPO="$(cd "$(dirname "$0")/.." && pwd)"
LOG_DIR="${E2E_LOG_DIR:-/tmp/e2e-readme-logs}"
README_REF="${E2E_README_REF:-HEAD}"
OPENCODE_VERSION="${E2E_VERSION_OPENCODE:-$DEFAULT_OPENCODE_VERSION}"
MODEL="${E2E_ONBOARDING_MODEL:-$DEFAULT_MODEL}"
# Seconds, not a duration string ("20m") — macOS ships only BSD sleep,
# which (unlike GNU sleep) rejects unit suffixes, and has no `timeout`
# binary at all. See run_with_timeout below.
TIMEOUT_SECS="${E2E_ONBOARDING_TIMEOUT_SECS:-$DEFAULT_TIMEOUT_SECS}"

# Portable stand-in for GNU coreutils `timeout` (absent on macOS by
# default — only Linux runners have it). Backgrounds the command, races
# a sleep+kill watchdog against it, and returns the command's exit
# status (or 143/SIGTERM if the watchdog fired first).
run_with_timeout() {
  local secs="$1"; shift
  "$@" &
  local pid=$!
  ( sleep "$secs"; kill -TERM "$pid" 2>/dev/null ) &
  local watchdog=$!
  # `|| status=$?` (not set +e/-e) — set -e/+e are global, not scoped to
  # this function, so toggling them here would leak into the caller and
  # (if the caller had errexit off to inspect $?, as the one call site
  # below does) silently re-enable it before this function returns,
  # aborting the script on a non-zero exit instead of letting the caller
  # read agent_status.
  local status=0
  wait "$pid" || status=$?
  kill "$watchdog" 2>/dev/null || true
  wait "$watchdog" 2>/dev/null || true
  return "$status"
}

: "${SKAINET_TOKEN:?SKAINET_TOKEN not set}"
: "${SKAINET_INTERNAL:?SKAINET_INTERNAL not set}"

mkdir -p "$LOG_DIR"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

# Fresh HOME for the driving opencode CLI — a real onboarding session
# starts with no prior omac/opencode state, and this must never see the
# rest of this checkout (no CLAUDE.md, no docs/, no .git).
DRIVER_HOME="$WORK/driver-home"
ONBOARD_DIR="$WORK/onboarding"
REPORT_FILE="$ONBOARD_DIR/ONBOARDING_REPORT.md"
TRANSCRIPT_FILE="$LOG_DIR/session-transcript.log"
mkdir -p "$DRIVER_HOME" "$ONBOARD_DIR"

# The one and only file the agent starts with.
git -C "$REPO" show "${README_REF}:README.md" > "$ONBOARD_DIR/README.md"

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

PROMPT_FILE="$WORK/prompt.md"
cat > "$PROMPT_FILE" <<EOF
You are a brand-new developer who just joined a project called "omac"
(oh-my-agentic-coder). The ONLY thing you have been given is the
README.md file in your current directory — nothing else from the
project's repository. You do have normal internet access and a normal
shell, exactly like a real new hire would on day one.

Your task: follow the README to set up omac for NORMAL, EVERYDAY
DEVELOPMENT on this machine, the way any of its instructions describe.
Concretely, aim to reach all of the following, in order, using only what
the README tells you (or what you discover from --help output, doctor
output, error messages, or the README's own linked pages/URLs):

1. Install every prerequisite the README says this OS needs.
2. Install omac itself, by whichever documented method makes sense here.
3. Run whatever the README says verifies the install, and get it healthy.
4. Install at least one inner coding harness the README lists, and get
   as far as the README's own instructions take you toward actually
   launching omac with it.

Rules:
- Do not use prior/background knowledge about this specific project's
  internals that isn't written in README.md or reachable by following a
  URL the README itself gives you. If you get stuck, say so — don't
  quietly paper over a gap using outside knowledge.
- Do not invent commands or paths beyond what's documented; if the
  README's instructions don't work as written, that IS the finding.
- If a step is genuinely ambiguous, pick the most literal reading, note
  the ambiguity, and continue.
- You may install system packages, clone/download things the README
  points you at, and run any commands needed — this is a real machine,
  not a sandbox.

When you are done (whether fully successful, partially blocked, or
stuck), write your findings to exactly this path, in exactly this
structure, and nothing else:

$REPORT_FILE

# Onboarding report

## Outcome
One of: SUCCESS / PARTIAL / BLOCKED, plus one sentence.

## Steps taken
Numbered list of the commands you ran, in order.

## README gaps
For each problem you hit — a missing step, wrong command, unstated
assumption, contradiction, or missing prerequisite — one entry with:
- Where: which README section/line
- What's wrong
- What you had to do instead (if anything)
- Suggested README fix
If there were none, write "None found."

## Assumptions / outside knowledge used
Anything you had to infer or look up beyond the README text itself.

## Verification
The exact command(s) and output that show how far you actually got
(e.g. doctor output, version strings). Be literal — quote real output,
don't summarize it as working if it didn't.

This is a sanctioned, pre-authorized test session — proceed directly
without asking for confirmation.
EOF
# Read back from a plain file rather than nesting the heredoc inside
# $(...) directly: bash 3.2 (macOS's stock /bin/bash) misparses a
# command-substitution-nested heredoc whose body contains an apostrophe
# (e.g. "project's", "don't") — "unexpected EOF while looking for
# matching `''" or a stray body line executed as a command. A plain file
# read sidesteps the bug entirely, regardless of body content.
PROMPT="$(cat "$PROMPT_FILE")"

echo "== Running onboarding agent (timeout ${TIMEOUT_SECS}s) =="
cd "$ONBOARD_DIR"
set +e
# GH Actions runners preset XDG_CONFIG_HOME/XDG_DATA_HOME (pointing at the
# real runner $HOME), and opencode resolves config from those before
# falling back to $HOME — so HOME alone does not isolate it. Must
# override all three to match where auth.json/opencode.json were
# written above, or opencode silently reads the real machine's config
# instead of ours. Same fix as withHome() in internal/e2e/harnesses.go.
#
# --auto: opencode's non-interactive `run` mode auto-REJECTS any
# permission request it can't ask about interactively — including
# reading paths outside the workdir (e.g. /etc/os-release) or running
# package managers. Without it the agent can't do anything this test
# needs (installing prerequisites, omac, a harness). Equivalent to
# claude-code's --dangerously-skip-permissions; same rationale as that
# flag in claudeCodeConfig() (internal/e2e/harnesses.go).
HOME="$DRIVER_HOME" \
XDG_CONFIG_HOME="$DRIVER_HOME/.config" \
XDG_DATA_HOME="$DRIVER_HOME/.local/share" \
XDG_STATE_HOME="$DRIVER_HOME/.local/state" \
SKAINET_TOKEN="$SKAINET_TOKEN" \
PATH="$PATH" \
run_with_timeout "$TIMEOUT_SECS" \
  opencode run --print-logs --auto -m "model/$MODEL" "$PROMPT" \
  > "$TRANSCRIPT_FILE" 2>&1
agent_status=$?
set -e
echo "agent exit status: $agent_status"

# Independent, objective check — never trust the agent's prose alone.
# The agent's own PATH/HOME additions (e.g. installing to
# $DRIVER_HOME/go/bin, ~/.local/bin) are what "doctor healthy" must find,
# since that's genuinely what the README told it to do.
doctor_output="$LOG_DIR/doctor-output.txt"
doctor_ok=0
if command -v omac >/dev/null 2>&1 || [ -x "$DRIVER_HOME/go/bin/omac" ]; then
  OMAC_BIN="$(command -v omac || echo "$DRIVER_HOME/go/bin/omac")"
  if HOME="$DRIVER_HOME" \
     XDG_CONFIG_HOME="$DRIVER_HOME/.config" \
     XDG_DATA_HOME="$DRIVER_HOME/.local/share" \
     XDG_STATE_HOME="$DRIVER_HOME/.local/state" \
     "$OMAC_BIN" doctor > "$doctor_output" 2>&1; then
    doctor_ok=1
  fi
else
  echo "omac binary not found on PATH or in \$DRIVER_HOME/go/bin" > "$doctor_output"
fi
echo "omac doctor exit ok: $doctor_ok"

cp "$TRANSCRIPT_FILE" "$LOG_DIR/" 2>/dev/null || true
if [ -f "$REPORT_FILE" ]; then
  cp "$REPORT_FILE" "$LOG_DIR/"
  report_present=1
else
  echo "agent did not write a report to $REPORT_FILE" | tee "$LOG_DIR/ONBOARDING_REPORT.md"
  report_present=0
fi

echo "== Onboarding report =="
cat "$LOG_DIR/ONBOARDING_REPORT.md"

if [ "$report_present" -eq 1 ] && [ "$doctor_ok" -eq 1 ]; then
  exit 0
fi
exit 1
