#!/usr/bin/env bash
#
# plan-stage.sh — Job 2 of the security-remediate workflow: batch the scan
# findings by origin into fix plans.
#
# One headless opencode session reads strix's vulnerabilities.json from the
# private archive clone plus this repo's checkout, groups the findings by
# their shared root cause, and writes into the scan directory of the archive:
#   - mitigation-plans/README.md (priority order, parallelization table,
#     conflict matrix, manual follow-up section)
#   - one mitigation-plans/NN-<origin>.md plan file per origin
#   - mitigation-plans/plans.json, the manifest every later stage consumes
#
# The runner shell (this script), never the agent, validates the manifest and
# pushes it. Validation failures keep the manifest absent so the next run
# re-plans: the session's output is archived as drafts for a human instead.
# Only counts and plan ids ever reach the public job log.
#
# Usage (from the job workspace, checkout under repo/):
#   bash repo/.github/scripts/security_remediate/plan-stage.sh
#
# Environment:
#   SCAN_DIR          name of the scan directory inside scans/ (required)
#   MODEL              model id; empty = resolve the harness pin
#   BUDGET_MINUTES    wall-clock budget for the session (default 60)
#   MAX_PLANS         budget for the post-planning selection (default 3)
#   PLANS_FILTER      restrict selection to these plan ids, comma-separated
#   REPO_DIR          this repo's checkout (default ./repo)
#   ARCHIVE_DIR       archive clone, created if missing (default ./archive)
#   LOG_DIR           transcript dir (default ./logs)
#   plus the lib.sh variables (PAT, ARCHIVE_REPO, GH_TOKEN, SKAINET_*)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

SCAN_DIR="${SCAN_DIR:-}"
REPO_DIR="${REPO_DIR:-$PWD/repo}"
ARCHIVE_DIR="${ARCHIVE_DIR:-$PWD/archive}"
LOG_DIR="${LOG_DIR:-$PWD/logs}"
BUDGET_MINUTES="${BUDGET_MINUTES:-60}"
MAX_PLANS="${MAX_PLANS:-3}"
PLANS_FILTER="${PLANS_FILTER:-}"

require_tools jq gh git curl bun

if [ -z "$SCAN_DIR" ]; then
  echo "::error title=No scan::SCAN_DIR is not set — the preflight stage names the scan to remediate. Nothing to plan for."
  exit 1
fi
case "$BUDGET_MINUTES" in
  ''|*[!0-9]*)
    echo "::error::budget_minutes must be a whole number of minutes, got '$BUDGET_MINUTES'" >&2
    exit 2 ;;
esac
case "$MAX_PLANS" in
  ''|*[!0-9]*)
    echo "::error::max_plans must be a whole number, got '$MAX_PLANS'" >&2
    exit 2 ;;
esac
if [ -z "${SECURITY_SCAN_PAT:-}" ] || [ -z "${ARCHIVE_REPO:-}" ]; then
  echo "::error title=Missing secrets::SECURITY_SCAN_PAT or SECURITY_ARCHIVE_REPO is not set — plans could be produced but not delivered."
  exit 1
fi
: "${SKAINET_TOKEN:?SKAINET_TOKEN not set}"
: "${SKAINET_INTERNAL:?SKAINET_INTERNAL not set}"

[ -d "$REPO_DIR" ] || {
  echo "::error title=Missing checkout::REPO_DIR '$REPO_DIR' does not exist."
  exit 1
}
# The session's cwd is the parent of the checkout, so it can read repo/ and
# write archive/ side by side. opencode's --auto mode auto-rejects writes
# outside the cwd, which is the outer fence here; the repo-clean check below
# is the inner one.
WORKSPACE="$(cd "$REPO_DIR/.." && pwd)"
[ -d "$ARCHIVE_DIR" ] || clone_archive "$ARCHIVE_DIR" "$ARCHIVE_REPO" "$SECURITY_SCAN_PAT"
ARCHIVE_DIR="$(cd "$ARCHIVE_DIR" && pwd)"
if [ "$(cd "$ARCHIVE_DIR/.." && pwd)" != "$WORKSPACE" ]; then
  echo "::error title=Layout::REPO_DIR and ARCHIVE_DIR must be siblings — the session's cwd is their parent, and opencode --auto rejects writes outside it."
  exit 1
fi

if [ -n "$(git -C "$REPO_DIR" status --porcelain)" ]; then
  echo "::error title=Dirty checkout::The source checkout under $REPO_DIR is not clean. The post-session guard could not tell the session's writes from pre-existing ones, so it refuses to run."
  exit 1
fi

# The session's budget is the only credential-gated resource this stage
# spends, so probe the delivery path before, not after.
probe_write_access "$ARCHIVE_REPO" "$SECURITY_SCAN_PAT" "Archive repo"

scan_abs="$ARCHIVE_DIR/scans/$SCAN_DIR"
if [ ! -d "$scan_abs" ]; then
  echo "::error title=Scan not found::Scan directory '$SCAN_DIR' does not exist in the archive repo. The preflight stage names it from the newest complete scan."
  exit 1
fi
vulns_json="$scan_abs/vulnerabilities.json"
if [ ! -f "$vulns_json" ]; then
  echo "::error title=No findings::Scan '$SCAN_DIR' has no vulnerabilities.json. There is nothing to plan for — clean scans are preflight no-ops, so this dispatch is off the intended path."
  exit 1
fi
vuln_count="$(jq 'length' "$vulns_json")"
if [ "$vuln_count" -eq 0 ]; then
  echo "::error title=No findings::Scan '$SCAN_DIR' has 0 findings. There is nothing to plan for."
  exit 1
fi

MODEL="${MODEL:-$(bash "$REPO_DIR/scripts/resolve-model.sh" opencode)}"

mitigation_dir="$scan_abs/mitigation-plans"
plans_json="$mitigation_dir/plans.json"
mkdir -p "$mitigation_dir"

# Keep the previous manifest recoverable: a re-plan that fails validation
# must not destroy the last good one.
WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT
had_plans=false
if [ -f "$plans_json" ]; then
  cp "$plans_json" "$WORK/plans.json.prev"
  had_plans=true
  # Findings already covered by an open or merged pull request are done;
  # the agent must carry their plans forward instead of re-planning them.
  covered="$(pr_covered_branches || true)"
  covered_desc="$(jq -r --arg covered "$covered" '
    ($covered | split("\n") | map(select(length > 0))) as $c
    | map(select(.branch as $b | ($c | index($b))))
    | if length == 0 then "none — no existing plan has an open or merged pull request"
      else map("\(.id) (\(.plan_file // .branch))") | join(", ")
      end' "$plans_json")"
else
  covered_desc="none — no plans exist for this scan yet"
fi

mkdir -p "$LOG_DIR"
TRANSCRIPT="$LOG_DIR/planning-session.log"

install_opencode
DRIVER_HOME="$(session_home "$WORK" "$MODEL")"

PROMPT_FILE="$WORK/prompt.md"
cat > "$PROMPT_FILE" <<EOF
You are the planning stage of an automated security-mitigation pipeline.

Your working directory contains two directories:
- repo/ — the source repository that was scanned. READ it to understand each
  finding's root cause. You must not modify anything in repo/.
- archive/ — the private companion repository holding the scan results. This
  is the only place you write to.

A security scanner produced ${vuln_count} findings for the code in repo/.
Read them in archive/scans/${SCAN_DIR}/vulnerabilities.json, plus any
validation notes and triage report in the same scan directory.

TASK: batch the findings by ORIGIN — the shared root cause behind a group of
findings — and write one fix plan per origin into
archive/scans/${SCAN_DIR}/mitigation-plans/:

1. README.md, containing:
   - "Where to start": the plans in priority order, one sentence of
     justification each.
   - A parallelization table: which plan owns which files.
   - A conflict matrix for files shared between plans.
   - "Manual follow-up": findings whose fix requires CI or workflow changes.
     These are NOT planned (see the rules) and must be listed here instead.

2. One plan file per origin, named NN-<origin-slug>.md where NN is the plan
   id, ascending with priority. Each plan file contains:
   - the finding ids it covers,
   - the root cause,
   - the minimal fix strategy,
   - an "internal order" section, if the fix is too large for one session:
     sub-steps a later stage executes sequentially on the same branch,
   - the regression tests that will pin the fixed behaviour,
   - the review criteria an independent reviewer will check the fix against.

3. plans.json — the machine-readable manifest every later stage consumes. It
   is a JSON array with one object per plan, EXACTLY this schema:
   [
     {
       "id": "01",
       "priority": 1,
       "branch": "fix/security-${SCAN_DIR}-plan-01",
       "plan_file": "mitigation-plans/01-<origin-slug>.md",
       "files": ["internal/some/pkg/file.go"],
       "tests": ["TestSecuritySomeProperty"],
       "issue_line": "one sentence for the public tracking issue",
       "review_criteria": "what an independent reviewer must verify"
     }
   ]

RULES:
- priority: 1 is most urgent. Derive it from finding severity and validation
  status.
- branch: exactly fix/security-${SCAN_DIR}-plan-<id>, with the same id as the
  entry.
- files: the files this plan may edit. Every path must exist in repo/ (check
  with your file tools before writing it down). Include any existing test
  files the fix will break because they relied on the old behaviour as a
  shortcut — the fix sessions may always edit Go test files, but listing them
  tells the reviewer what to expect. NEVER include .github/ paths — findings
  whose fix requires CI or workflow changes go under "Manual follow-up" in
  the README, not into a plan.
- tests: TestSecurity* names for the regression tests a later stage will
  write. Name them after the security property they assert.
- issue_line: this text becomes a checklist item in a PUBLIC GitHub issue.
  Describe the gap's origin, WITHOUT revealing how it is exploited: no
  finding titles, no proof-of-concept strings, no file paths.
- Findings already covered by an existing plan whose pull request is open or
  merged are done. Carry every existing plans.json entry forward unchanged
  (same ids, same branches — renumber nothing), and give new findings fresh
  ids. Covered plans so far: ${covered_desc}
- You have a limited wall-clock budget and may be stopped at any moment.
  Write plans.json (copy any existing entries into it) FIRST, then work plan
  by plan, updating plans.json and the README the moment each plan file is
  complete. Never hold completed work only in your head.

This is a sanctioned, pre-authorized planning session — proceed directly
without asking for confirmation.
EOF

echo "== Running planning session (model=$MODEL, budget=${BUDGET_MINUTES}m, findings=$vuln_count) =="
# The transcript can carry finding detail, so it goes to a file and to the
# private archive only — never to this public job log.
set +e
run_session "$DRIVER_HOME" "$((BUDGET_MINUTES * 60))" "$MODEL" "$WORKSPACE" "$PROMPT_FILE" "$TRANSCRIPT"
session_status=$?
set -e
echo "planning session exit status: $session_status"

# The session contract is archive-only. A dirty checkout means the contract
# is broken (prompt injection, or a confused session): treat ALL of its
# output as untrusted. Keep the archive push so a human can debug the
# transcript, but keep the manifest untouched.
if [ -n "$(git -C "$REPO_DIR" status --porcelain)" ]; then
  git -C "$ARCHIVE_DIR" reset --hard --quiet
  git -C "$ARCHIVE_DIR" clean -fdq scans/
  cp "$TRANSCRIPT" "$mitigation_dir/planning-session.log" 2>/dev/null || true
  push_archive "$ARCHIVE_DIR" "plans: REJECTED session for ${SCAN_DIR} — it wrote into the source repo" \
    "scans/$SCAN_DIR"
  echo "::error title=Planning session rejected::The session wrote into the source checkout instead of the archive clone. Its output was discarded (transcript archived privately); nothing was promoted. This is the prompt-injection guard firing."
  exit 1
fi

# ---- Validate the manifest --------------------------------------------------
# Error details go to a file in the private archive; the public log sees the
# count and the plan ids only.
report="$mitigation_dir/plans-validation.txt"
: > "$report"
if [ -f "$plans_json" ]; then
  plans_schema_errors "$plans_json" "$SCAN_DIR" >> "$report"

  # jq cannot stat: every owned file must exist in the checkout, every
  # plan_file in the scan dir.
  jq -c '.[] | {id: (.id // "-"), plan_file: (.plan_file // ""), files: (.files // [])}' \
    "$plans_json" 2>/dev/null | while IFS= read -r entry; do
    id="$(printf '%s' "$entry" | jq -r '.id')"
    pf="$(printf '%s' "$entry" | jq -r '.plan_file')"
    if [ -n "$pf" ] && [ ! -f "$scan_abs/$pf" ]; then
      echo "$id: plan_file does not exist in the scan dir: $pf" >> "$report"
    fi
    printf '%s' "$entry" | jq -r '.files[]' | while IFS= read -r f; do
      if [ ! -e "$REPO_DIR/$f" ]; then
        echo "$id: owned file does not exist in the checkout: $f" >> "$report"
      fi
    done
  done
else
  echo "-: plans.json is missing (the session may have run out of budget before writing it)" >> "$report"
fi

if [ -s "$report" ]; then
  problem_count="$(wc -l < "$report")"
  # Preserve the last good manifest (if any) and archive the session's raw
  # output as drafts for a human, then fail the stage: the next run sees no
  # manifest and re-plans.
  if [ -f "$plans_json" ]; then
    cp "$plans_json" "$mitigation_dir/plans.draft.json"
  fi
  if [ "$had_plans" = true ]; then
    cp "$WORK/plans.json.prev" "$plans_json"
  else
    rm -f "$plans_json"
  fi
  cp "$TRANSCRIPT" "$mitigation_dir/planning-session.log" 2>/dev/null || true
  push_archive "$ARCHIVE_DIR" "plans: DRAFT for ${SCAN_DIR} — validation failed, manifest left as-is ($(date -u +%F))" \
    "scans/$SCAN_DIR"
  echo "::error title=Plans rejected::plans.json failed ${problem_count} validation checks. The session's output and the checklist of problems are archived to the private repo (plans-validation.txt, plans.draft.json); the manifest itself was left unchanged, so the next run re-plans."
  exit 1
fi
rm -f "$report"

plan_count="$(jq 'length' "$plans_json")"
{ read -r selected_ids; read -r deferred_count; } <<< "$(select_plans "$plans_json" "$MAX_PLANS" "$PLANS_FILTER")"

cp "$TRANSCRIPT" "$mitigation_dir/planning-session.log" 2>/dev/null || true
push_archive "$ARCHIVE_DIR" "plans: ${SCAN_DIR} — ${plan_count} plans ($(date -u +%F))" \
  "scans/$SCAN_DIR"

emit_output selected_ids "$selected_ids"
emit_output deferred_count "$deferred_count"
emit_output plan_count "$plan_count"

{
  echo "## Security remediation: planning stage"
  echo ""
  echo "Produced ${plan_count} plans for \`$SCAN_DIR\`. Selected for this run: ${selected_ids:-none}. Deferred: ${deferred_count}."
} >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
