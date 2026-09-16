#!/usr/bin/env bash
#
# fix-stage.sh — one leg of the fix waves (Job 5 of the security-remediate
# workflow). Runs once per plan, from the workflow's wave matrix.
#
# One headless implementer session executes the plan file's fix strategy on
# a branch cut from the regression-suite branch — so the branch already
# carries the plan's red tests, and the fix's proof is mechanical:
# `go test -tags=vuln -run <selector>` goes green and the plan's pin-file
# entries are deleted in the same commit. The review loop runs before
# anything is pushed, with the plan's own review criteria plus the shared
# fix-review criteria. The runner shell owns everything credential-bearing:
# branch, push, pull request.
#
# Re-entry: a leg that failed after committing (session ran out of budget,
# review deadlock) pushes its branch WITHOUT a pull request; the next run
# re-selects the plan (no PR = still eligible) and this script continues on
# that branch instead of losing the progress — the "internal order" sub-step
# mechanism for plans too large for one session.
#
# Stacking: a plan sharing files with an earlier wave's plan whose pull
# request is open starts from that plan's branch, so its pull request merges
# after its base. Without an open base to stack on it falls back to the
# regression-suite branch.
#
# Usage (from the job workspace, checkout under repo/):
#   bash repo/.github/scripts/security_remediate/fix-stage.sh
#
# Environment:
#   PLAN_ID            the plan this leg executes (required, from the matrix)
#   SCAN_DIR           scan directory name (required)
#   OVERVIEW_ISSUE     the overview issue number, for Refs
#   TEST_BRANCH        the regression-suite branch (required)
#   WAVE_MAP           JSON {"<plan id>": <wave>} from the waves job
#   MODEL, REVIEW_MODEL implementer / reviewer model ids
#   BUDGET_MINUTES     per-session wall-clock budget (default 60)
#   MAX_REVIEW_PASSES  review passes before opening the PR anyway (default 2)
#   STRICT_REVIEW       true = fail on unresolved findings instead of PR
#   REPO_DIR, ARCHIVE_DIR, LOG_DIR   (defaults ./repo, ./archive, ./logs)
#   GH_TOKEN            the write PAT (push, PR creation)
#   plus the lib.sh variables (ARCHIVE_REPO, SECURITY_SCAN_PAT, SKAINET_*)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"
. "$HERE/review-loop.sh"

PLAN_ID="${PLAN_ID:-}"
SCAN_DIR="${SCAN_DIR:-}"
OVERVIEW_ISSUE="${OVERVIEW_ISSUE:-}"
TEST_BRANCH="${TEST_BRANCH:-}"
WAVE_MAP="${WAVE_MAP:-{\}}"
MODEL="${MODEL:-}"
REVIEW_MODEL="${REVIEW_MODEL:-}"
BUDGET_MINUTES="${BUDGET_MINUTES:-60}"
MAX_REVIEW_PASSES="${MAX_REVIEW_PASSES:-2}"
STRICT_REVIEW="${STRICT_REVIEW:-false}"
REPO_DIR="${REPO_DIR:-$PWD/repo}"
ARCHIVE_DIR="${ARCHIVE_DIR:-$PWD/archive}"
LOG_DIR="${LOG_DIR:-$PWD/logs}"

require_tools jq gh git go curl bun

if [ -z "$PLAN_ID" ] || [ -z "$SCAN_DIR" ] || [ -z "$TEST_BRANCH" ]; then
  echo "::error title=Missing inputs::PLAN_ID, SCAN_DIR and TEST_BRANCH must be set (the workflow's wave matrix provides them)."
  exit 1
fi
if [ -z "$OVERVIEW_ISSUE" ]; then
  echo "::error title=No overview issue::OVERVIEW_ISSUE is not set — the fix pull request must reference the overview issue."
  exit 1
fi
case "$BUDGET_MINUTES" in ''|*[!0-9]*) echo "::error::budget_minutes must be a whole number" >&2; exit 2 ;; esac
case "$MAX_REVIEW_PASSES" in ''|*[!0-9]*) echo "::error::max_review_passes must be a whole number" >&2; exit 2 ;; esac
[ "$MAX_REVIEW_PASSES" -ge 1 ] || { echo "::error::max_review_passes must be >= 1" >&2; exit 2; }
if [ -z "${SECURITY_SCAN_PAT:-}" ] || [ -z "${ARCHIVE_REPO:-}" ]; then
  echo "::error title=Missing secrets::SECURITY_SCAN_PAT or SECURITY_ARCHIVE_REPO is not set."
  exit 1
fi
: "${SKAINET_TOKEN:?SKAINET_TOKEN not set}"
: "${SKAINET_INTERNAL:?SKAINET_INTERNAL not set}"

[ -d "$REPO_DIR" ] || { echo "::error title=Missing checkout::REPO_DIR '$REPO_DIR' does not exist." >&2; exit 1; }
WORKSPACE="$(cd "$REPO_DIR/.." && pwd)"
[ -d "$ARCHIVE_DIR" ] || clone_archive "$ARCHIVE_DIR" "$ARCHIVE_REPO" "$SECURITY_SCAN_PAT"
if [ "$(cd "$ARCHIVE_DIR/.." && pwd)" != "$WORKSPACE" ]; then
  echo "::error title=Layout::REPO_DIR and ARCHIVE_DIR must be siblings — the session's cwd is their parent."
  exit 1
fi
probe_write_access "$ARCHIVE_REPO" "$SECURITY_SCAN_PAT" "Archive repo"

scan_abs="$ARCHIVE_DIR/scans/$SCAN_DIR"
plans_json="$scan_abs/mitigation-plans/plans.json"
[ -f "$plans_json" ] || { echo "::error title=No manifest::plans.json is missing for scan '$SCAN_DIR'." >&2; exit 1; }

my_plan="$(jq -c --arg id "$PLAN_ID" '.[] | select(.id == $id)' "$plans_json")"
[ -n "$my_plan" ] || { echo "::error title=Unknown plan::No plan '$PLAN_ID' in the manifest." >&2; exit 1; }
my_branch="$(printf '%s' "$my_plan" | jq -r '.branch')"
my_plan_file="$(printf '%s' "$my_plan" | jq -r '.plan_file')"
my_issue_line="$(printf '%s' "$my_plan" | jq -r '.issue_line')"
my_criteria="$(printf '%s' "$my_plan" | jq -r '.review_criteria')"
my_files="$(printf '%s' "$my_plan" | jq -r '.files | join(" ")')"
test_selector="$(printf '%s' "$my_plan" | jq -r '.tests[]' | sort -u | sed 's/^/^/; s/$/$/' | paste -sd'|' -)"

# --- Branch --------------------------------------------------------------------
git -C "$REPO_DIR" config user.name "security-remediate-bot"
git -C "$REPO_DIR" config user.email "actions@users.noreply.github.com"
if [ -n "$(git -C "$REPO_DIR" status --porcelain)" ]; then
  echo "::error title=Dirty checkout::The source checkout is not clean; the ownership guard could not attribute changes. Failing the leg."
  exit 1
fi

# Re-entry: continue a branch a previous leg pushed without a PR.
base_branch=""
if git -C "$REPO_DIR" fetch --quiet origin "$my_branch" 2>/dev/null; then
  base_branch="$my_branch"
  echo "continuing plan $PLAN_ID on its existing branch $my_branch"
else
  # Stacking: the highest-wave earlier plan that shares files with this one
  # AND has an open PR is the base; the lower waves' fixes are in it
  # already. No such plan -> the regression-suite branch.
  stack_ids="$(jq -r --arg id "$PLAN_ID" --argjson map "$WAVE_MAP" '
    (map(select(.id == $id)) | .[0]) as $me
    | map(select(.id != $id))
      | map(select(. as $o | (($map[$o.id] // 0) < ($map[$id] // 0))))
      | map(select(. as $o | any($me.files[]; . as $f | ($o.files | index($f)) != null)))
      | sort_by($map[.id] // 0)
      | reverse
      | map(.id)
      | join(" ")' "$plans_json")"
  for cand in $stack_ids; do
    cand_branch="fix/security-$SCAN_DIR-plan-$cand"
    if gh pr list --head "$cand_branch" --state open --json url --jq 'length' 2>/dev/null | grep -q '^1$' \
       && git -C "$REPO_DIR" fetch --quiet origin "$cand_branch" 2>/dev/null; then
      base_branch="$cand_branch"
      echo "stacking plan $PLAN_ID on $cand_branch (shares files with plan $cand, whose PR is open)"
      break
    fi
  done
  [ -n "$base_branch" ] || base_branch="$TEST_BRANCH"
  # The shallow checkout only fetched the default ref; the base branch must
  # be fetched explicitly before origin/<base> can be checked out.
  git -C "$REPO_DIR" fetch --quiet origin "$base_branch"
  echo "starting plan $PLAN_ID from $base_branch"
fi
git -C "$REPO_DIR" checkout --quiet -B "$my_branch" "origin/$base_branch"

# --- Session setup ---------------------------------------------------------------
mkdir -p "$LOG_DIR"
TRANSCRIPT="$LOG_DIR/implement-pass.log"
WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT

MODEL="${MODEL:-$(bash "$REPO_DIR/scripts/resolve-model.sh" opencode)}"
install_opencode
DRIVER_HOME="$(session_home "$WORK" "$MODEL")"

# Ownership: the plan's files plus the pin file (the fix deletes its pin
# entries there), nothing else. Literal paths, regex-escaped and anchored.
GUARD="$WORK/allowed-paths"
{
  printf '%s\n' "$my_plan" | jq -r '.files[]' | sed 's/[^A-Za-z0-9_/.-]/\\&/g; s/^/^/; s/$/$/'
  printf '%s\n' '^scripts/security-suite-expected-failures\.txt$'
} > "$GUARD"

CRITERIA="$WORK/criteria.md"
{
  cat <<'EOF'
- The change closes the ORIGIN behind the findings, not a symptom of them.
- The change introduces no new gaps: input validation, error handling and
  trust boundaries stay intact where the touched code runs.
- Where a spec or doc normatively mandates the vulnerable behaviour, the
  spec or doc is updated in the same change — a code-only fix there gets
  reverted by the next contributor.
- The change stays within the plan's owned files and never touches CI
  configuration.
- Quote no text from the plan file into REVIEW.md: findings must be
  described against the public diff only.
EOF
  echo ""
  echo "Plan-specific criteria:"
  echo ""
  printf '%s\n' "$my_criteria"
} > "$CRITERIA"

PROMPT="$WORK/prompt.md"
cat > "$PROMPT" <<EOF
You are the implementer of one fix plan in an automated security-mitigation
pipeline.

Your working directory contains repo/, the source repository (on the
plan's branch — the regression tests for your plan are here already and are
currently RED), and archive/, the private companion repository with the scan
results and the plan.

Execute the plan for plan ${PLAN_ID}: archive/scans/${SCAN_DIR}/${my_plan_file}.

- The tests pinned for this plan are red now and your fix makes them pass.
  Verify with: cd repo && go test -tags=vuln -run '${test_selector}' ./...
- Implement the minimal fix the plan describes, in the files the plan owns:
  ${my_files}
  Touch NOTHING else — no other files, no tests, no CI configuration.
- Nothing you write (identifiers, comments, error strings) may reveal how
  the weaknesses are exploited: this repository is public.
- When the tests pass, delete this plan's test entries from
  scripts/security-suite-expected-failures.txt.
- You have a limited wall-clock budget and may be stopped at any moment. If
  the plan is too large, follow its "internal order" section: complete the
  earliest sub-step fully rather than everything half-way. The runner
  commits whatever is done.

Sanctioned, pre-authorized fix session — proceed directly without asking
for confirmation.
EOF

echo "== Running fix session (model=$MODEL, budget=${BUDGET_MINUTES}m, plan=$PLAN_ID) =="
set +e
run_session "$DRIVER_HOME" "$((BUDGET_MINUTES * 60))" "$MODEL" "$WORKSPACE" "$PROMPT" "$TRANSCRIPT"
session_status=$?
set -e
echo "fix session exit status: $session_status"

# --- Guards, progress save, then the review loop --------------------------------
# A test name is a Go identifier (regex-safe), matched at line end so a
# prefix name never counts for a longer one.
pin_test_pinned() {
  grep -qE "[[:space:]]${1}\$" "$pin_file"
}

pin_file="$REPO_DIR/scripts/security-suite-expected-failures.txt"
pinned_tests() {
  local t found=""
  while IFS= read -r t; do
    pin_test_pinned "$t" && found="$found$t
"
  done < <(printf '%s' "$my_plan" | jq -r '.tests[]')
  printf '%s' "$found"
}

tests_green() {
  ( cd "$REPO_DIR" && go build ./... ) || return 1
  ( cd "$REPO_DIR" && go test -tags=vuln -run "$test_selector" ./... >/dev/null 2>&1 )
}

# Commit and push whatever the session managed, WITHOUT a pull request: the
# plan stays eligible (no PR) and the next run continues on this branch —
# the sub-step mechanism for plans too large for one session. The branch is
# pushed after all sessions for this plan have ended, so no session ever
# sees the PAT.
save_progress() {
  local why=$1
  git -C "$REPO_DIR" add .
  if ! git -C "$REPO_DIR" diff --cached --quiet; then
    git -C "$REPO_DIR" commit --quiet -m "wip(security): partial fix for plan $PLAN_ID"
    git -C "$REPO_DIR" remote set-url origin "https://x-access-token:${SECURITY_SCAN_PAT}@github.com/${GITHUB_REPOSITORY}.git"
    archive_git "$REPO_DIR" push --quiet -u origin "$my_branch" || {
      echo "::error title=Progress push failed::The partial work could not be pushed; it is lost with the runner."
      return 1
    }
  fi
  echo "::warning title=Progress saved::$why"
}

if [ -n "$(changed_files_within "$REPO_DIR" "$GUARD")" ]; then
  echo "::error title=Ownership guard tripped::The session edited files outside the plan's owned set. Failing the leg; nothing is pushed."
  exit 1
fi

# The session did not finish (watchdog, crash, gateway): save whatever it
# managed and fail the leg without spending review sessions on half-done
# work. The next run continues on the pushed branch.
if [ "$session_status" -ne 0 ]; then
  save_progress "The session exited with status $session_status mid-plan; the next run continues on the branch."
  echo "::error title=Fix session incomplete::Failing the leg; partial work is on the branch, not in a pull request."
  exit 1
fi

if git -C "$REPO_DIR" add -N . >/dev/null 2>&1 && git -C "$REPO_DIR" diff --quiet; then
  # Empty diff: only legitimate as the idempotent completion of a re-entered
  # branch — tests already green and pins already gone. Then the leg goes
  # straight to the pull request; otherwise it has nothing to review.
  if tests_green && [ -z "$(pinned_tests)" ]; then
    echo "branch already carries the completed fix — going straight to the pull request"
    REVIEW_PASSES_RUN=0
    REVIEW_FINDINGS=0
    review_rc=0
  else
    echo "::error title=Empty diff::The session wrote nothing and the branch does not carry a completed fix. An empty diff has nothing to review — failing the leg."
    exit 1
  fi
else
  set +e
  run_review_loop "$REPO_DIR" "$WORKSPACE" "$DRIVER_HOME" "$MODEL" "$REVIEW_MODEL" \
    "$((BUDGET_MINUTES * 60))" "$MAX_REVIEW_PASSES" "$CRITERIA" "$GUARD" "$LOG_DIR" \
    "fix for plan ${PLAN_ID}"
  review_rc=$?
  set -e
fi

case "$review_rc" in
  0) ;;
  1) echo "review loop failed — see the errors above"; exit 1 ;;
  2)
    if [ "$STRICT_REVIEW" = true ]; then
      echo "::error title=Strict review::The review loop ended with ${REVIEW_FINDINGS} unresolved findings and strict_review is on — failing instead of opening the PR."
      exit 1
    fi
    echo "::warning title=Unresolved review findings::Opening the PR with ${REVIEW_FINDINGS} unresolved findings, listed in its body."
    ;;
esac

# --- Green proof ----------------------------------------------------------------
if ! tests_green; then
  echo "::error title=Tests are not green::The plan's pinned tests still fail after the fix. The red-to-green story is incomplete — failing the leg (partial work stays on the branch only if the session ran out of budget, which is handled above)."
  exit 1
fi
leftover_pins="$(pinned_tests)"
if [ -n "$leftover_pins" ]; then
  # The fix is proven green; removing the pin entries is bookkeeping, so the
  # runner does it rather than trusting the session to have done it. The
  # line match is anchored like the presence check above.
  while IFS= read -r t; do
    [ -n "$t" ] || continue
    grep -vE "[[:space:]]${t}\$" "$pin_file" > "$pin_file.tmp" || true
    mv "$pin_file.tmp" "$pin_file"
  done <<< "$leftover_pins"
  echo "removed leftover pin entries for plan $PLAN_ID"
fi

# --- Commit, push, pull request ---------------------------------------------------
git -C "$REPO_DIR" add .
if ! git -C "$REPO_DIR" diff --cached --quiet; then
  git -C "$REPO_DIR" commit --quiet -m "fix(security): $(printf '%s' "$my_issue_line" | tr -c 'A-Za-z0-9._-' '-')"
  git -C "$REPO_DIR" remote set-url origin "https://x-access-token:${SECURITY_SCAN_PAT}@github.com/${GITHUB_REPOSITORY}.git"
  # Through archive_git for the redaction: a failed push prints the remote
  # URL, credentials included, and this job log is public.
  archive_git "$REPO_DIR" push --quiet -u origin "$my_branch" || {
    echo "::error title=Push failed::The fix branch commit exists locally but was not pushed. The leg fails; the commit is lost with the runner."
    exit 1
  }
fi

stacked_on=""
if [ "$base_branch" != "$TEST_BRANCH" ] && [ "$base_branch" != "$my_branch" ]; then
  stacked_on="$base_branch"
fi

body_file="$WORK/pr-body.md"
{
  echo "Refs #${OVERVIEW_ISSUE}"
  echo ""
  echo "Fixes the origin behind one workstream of the current scan: the plan's pinned regression tests now pass and their pin-file entries are gone. Detailed planning lives in the private companion repo."
  echo ""
  echo "Review loop: ${REVIEW_PASSES_RUN} pass(es), ${REVIEW_FINDINGS} unresolved findings."
  if [ "${REVIEW_FINDINGS:-0}" -gt 0 ] && [ -f "$LOG_DIR/REVIEW.md" ]; then
    echo ""
    echo "## Unresolved review findings"
    echo ""
    cat "$LOG_DIR/REVIEW.md"
  fi
  if [ -n "$stacked_on" ]; then
    base_number="$(gh pr list --head "$stacked_on" --state open --json number --jq '.[0].number' 2>/dev/null || printf '0')"
    [ "$base_number" != "0" ] && echo "" && echo "Stacked on #${base_number} — merge that first."
  fi
  echo ""
  echo "---"
  echo "🤖 Generated with opencode"
} > "$body_file"

pr_url="$(gh pr list --head "$my_branch" --state open --json url --jq '.[0].url' 2>/dev/null || true)"
if [ -n "$pr_url" ]; then
  gh pr edit "$my_branch" --body-file "$body_file" >/dev/null
  echo "updated fix pull request: $pr_url"
else
  set +e
  if [ -n "$stacked_on" ]; then
    pr_url="$(gh pr create --head "$my_branch" --base "$stacked_on" \
      --title "fix(security): $(printf '%s' "$my_issue_line" | tr -c 'A-Za-z0-9._-' '-')" \
      --body-file "$body_file" --label do-not-merge 2>&1)"
  else
    pr_url="$(gh pr create --head "$my_branch" \
      --title "fix(security): $(printf '%s' "$my_issue_line" | tr -c 'A-Za-z0-9._-' '-')" \
      --body-file "$body_file" --label do-not-merge 2>&1)"
  fi
  pr_status=$?
  set -e
  if [ "$pr_status" -ne 0 ]; then
    echo "::error title=Pull request creation failed::The fix branch is pushed; the pull request is not open. Create the 'do-not-merge' label and re-run. Details: $pr_url"
    exit 1
  fi
  echo "opened fix pull request: $pr_url"
fi

# --- Archive the session logs privately -----------------------------------------
stage_logs="$scan_abs/remediation/fix-$PLAN_ID"
mkdir -p "$stage_logs"
cp "$LOG_DIR"/*.log "$stage_logs"/ 2>/dev/null || true
cp "$LOG_DIR"/REVIEW.md "$stage_logs"/ 2>/dev/null || true
push_archive "$ARCHIVE_DIR" "remediation: fix logs for ${SCAN_DIR} plan ${PLAN_ID} ($(date -u +%F))"

printf 'fix stage: plan %s, review %s/%s\n' "$PLAN_ID" "$REVIEW_PASSES_RUN" "$REVIEW_FINDINGS" >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
