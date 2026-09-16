#!/usr/bin/env bash
#
# test-suite-stage.sh — Job 4 of the security-remediate workflow: the
# regression-test pull request.
#
# One headless agent session writes the selected plans' regression tests
# behind the `vuln` build tag and pins them in
# scripts/security-suite-expected-failures.txt — extending the existing
# security-suite mechanism, red tests being the point: they fail against
# the current tree and the fix PRs turn them (and their pin lines) green.
# The review loop then runs with test-review criteria before anything goes
# public, because the disclosure check has to precede the push, not follow
# it. Idempotent: an existing suite branch for this generation gains the
# newly selected plans' tests and the pull request is updated, tests for
# deferred plans are never written early.
#
# Usage (from the job workspace, checkout under repo/):
#   bash repo/.github/scripts/security_remediate/test-suite-stage.sh
#
# Environment:
#   SCAN_DIR            scan directory name (required)
#   SELECTED_IDS        plan ids to cover, comma-separated (required)
#   OVERVIEW_ISSUE      the overview issue number, for Refs
#   MODEL, REVIEW_MODEL implementer / reviewer model ids
#   BUDGET_MINUTES      per-session wall-clock budget (default 60)
#   MAX_REVIEW_PASSES   review passes before opening the PR anyway (default 2)
#   STRICT_REVIEW       true = fail on unresolved findings instead of
#                       annotating the PR (default false)
#   REPO_DIR, ARCHIVE_DIR, LOG_DIR   (defaults ./repo, ./archive, ./logs)
#   GH_TOKEN            the write PAT (push, PR creation)
#   plus the lib.sh variables (ARCHIVE_REPO, SECURITY_SCAN_PAT, SKAINET_*)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"
. "$HERE/review-loop.sh"

SCAN_DIR="${SCAN_DIR:-}"
SELECTED_IDS="${SELECTED_IDS:-}"
OVERVIEW_ISSUE="${OVERVIEW_ISSUE:-}"
MODEL="${MODEL:-}"
REVIEW_MODEL="${REVIEW_MODEL:-}"
BUDGET_MINUTES="${BUDGET_MINUTES:-60}"
MAX_REVIEW_PASSES="${MAX_REVIEW_PASSES:-2}"
STRICT_REVIEW="${STRICT_REVIEW:-false}"
REPO_DIR="${REPO_DIR:-$PWD/repo}"
ARCHIVE_DIR="${ARCHIVE_DIR:-$PWD/archive}"
LOG_DIR="${LOG_DIR:-$PWD/logs}"

require_tools jq gh git go curl bun

if [ -z "$SCAN_DIR" ]; then
  echo "::error title=Missing inputs::SCAN_DIR must be set (the preflight/plan stages provide it)."
  exit 1
fi
if [ -z "$SELECTED_IDS" ]; then
  # Legitimate: every eligible plan already has an open or merged PR, or the
  # operator filtered to nothing. Not an error — an empty run must be cheap.
  echo "no plans selected for this run — nothing to test"
  exit 0
fi
if [ -z "$OVERVIEW_ISSUE" ]; then
  echo "::error title=No overview issue::OVERVIEW_ISSUE is not set — the test-suite pull request must reference the overview issue, so the issue stage has to run first."
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

# Session budget is the stage's cost, so the delivery paths are probed first.
probe_write_access "$ARCHIVE_REPO" "$SECURITY_SCAN_PAT" "Archive repo"

scan_abs="$ARCHIVE_DIR/scans/$SCAN_DIR"
plans_json="$scan_abs/mitigation-plans/plans.json"
[ -f "$plans_json" ] || { echo "::error title=No manifest::plans.json is missing for scan '$SCAN_DIR'." >&2; exit 1; }

selected_manifest="$(jq -c --arg ids "$SELECTED_IDS" '
  ($ids | split(",") | map(select(length > 0))) as $want
  | map(select(.id as $i | ($want | index($i))))
  | sort_by(.priority, .id)' "$plans_json")"
plan_count="$(printf '%s\n' "$selected_manifest" | jq 'length')"
if [ "$plan_count" -eq 0 ]; then
  echo "no plans selected for the test suite — nothing to do"
  exit 0
fi
test_selector="$(printf '%s\n' "$selected_manifest" | jq -r '.[].tests[]' | sort -u | sed 's/^/^/; s/$/$/' | paste -sd'|' -)"

# --- Suite branch --------------------------------------------------------------
test_branch="security-regression-suite-$SCAN_DIR"
git -C "$REPO_DIR" config user.name "security-remediate-bot"
git -C "$REPO_DIR" config user.email "actions@users.noreply.github.com"
if git -C "$REPO_DIR" fetch --quiet origin "$test_branch" 2>/dev/null \
   && git -C "$REPO_DIR" checkout --quiet -B "$test_branch" "origin/$test_branch"; then
  echo "appending to existing suite branch $test_branch"
else
  git -C "$REPO_DIR" checkout --quiet -B "$test_branch"
  echo "created suite branch $test_branch"
fi

pin_file="$REPO_DIR/scripts/security-suite-expected-failures.txt"
missing_pin_entries() {
  local t missing=""
  while IFS= read -r t; do
    # A test name is a Go identifier (regex-safe), matched at line end so a
    # prefix name never counts for a longer one.
    grep -qE "[[:space:]]${t}\$" "$pin_file" || missing="$missing$t
"
  done < <(printf '%s\n' "$selected_manifest" | jq -r '.[].tests[]')
  printf '%s' "$missing"
}

mkdir -p "$LOG_DIR"
TRANSCRIPT="$LOG_DIR/implement-pass.log"
WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT

MODEL="${MODEL:-$(bash "$REPO_DIR/scripts/resolve-model.sh" opencode)}"
install_opencode
DRIVER_HOME="$(session_home "$WORK" "$MODEL")"

# The ownership rules for this stage: test files and the pin file, nothing
# else. Plans never own .github/ either, and a test stage that edits CI or
# production code is a bug, not a review matter.
GUARD="$WORK/allowed-paths"
cat > "$GUARD" <<'EOF'
_test\.go$
^scripts/security-suite-expected-failures\.txt$
EOF

CRITERIA="$WORK/criteria.md"
cat > "$CRITERIA" <<'EOF'
- Disclosure: test names, comments and fixture data must not reveal how the
  underlying weaknesses are exploited — this suite is public. Finding titles
  and proof-of-concept strings from the scan must not appear.
- Every new test asserts the security property of the FIXED state and is
  red against the current tree, so it pins the vulnerable behaviour.
- Existing tests that encode the vulnerable behaviour are inverted
  deliberately, not deleted.
- Every new red test is pinned exactly once in
  scripts/security-suite-expected-failures.txt, in the file's
  "<package> <TestName>" format, keeping the file sorted.
- The change touches only *_test.go files and the pin file.
EOF

PROMPT="$WORK/prompt.md"
cat > "$PROMPT" <<EOF
You are the test-suite stage of an automated security-mitigation pipeline.

Your working directory contains repo/, the source repository (already on
the regression-suite branch for this scan), and archive/, the private
companion repository with the scan results and fix plans.

Write the failing regression tests for exactly these plans:

$(printf '%s\n' "$selected_manifest" | jq -r '.[] | "plan \(.id): \(.plan_file) — tests: \(.tests | join(", "))"')

For each plan:
1. Read its plan file at archive/scans/${SCAN_DIR}/\${plan_file} (the plan_file
   path from the listing above) and follow its regression-test specification,
   including any runnable PoCs it links elsewhere in the same scan directory —
   adopt those as tests where present instead of writing new ones.
2. Write the plan's tests (the exact names above) behind the \`vuln\` build
   tag, following the pattern of the repo's existing security-suite tests.
   Each test asserts the security property of the FIXED state, so it FAILS
   against the current code and passes once the fix lands.
3. Pin every new red test in scripts/security-suite-expected-failures.txt,
   matching the file's "<package> <TestName>" format exactly and keeping
   the file sorted.
4. Where the plan calls for it, invert (do NOT delete) existing tests that
   currently assert the vulnerable behaviour, and pin those too.

Rules:
- Only create or edit *_test.go files and the pin file. Never fix non-test
  code, never touch anything else.
- If a plan's tests already exist on this branch, verify and extend them
  instead of duplicating.
- Nothing in the tests — names, comments, fixture data — may reveal how the
  weaknesses are exploited: this repository is public.
- You have a limited wall-clock budget and may be stopped at any moment.
  Work plan by plan; the runner shell commits.

Sanctioned, pre-authorized session — proceed directly without asking for
confirmation.
EOF

echo "== Running test-writing session (model=$MODEL, budget=${BUDGET_MINUTES}m, plans=$SELECTED_IDS) =="
set +e
run_session "$DRIVER_HOME" "$((BUDGET_MINUTES * 60))" "$MODEL" "$WORKSPACE" "$PROMPT" "$TRANSCRIPT"
session_status=$?
set -e
echo "test-writing session exit status: $session_status"

# --- Guards, then the review loop ---------------------------------------------
if [ -n "$(changed_files_within "$REPO_DIR" "$GUARD")" ]; then
  echo "::error title=Ownership guard tripped::The session edited files outside *_test.go and the pin file. Failing the leg; nothing is pushed."
  exit 1
fi

unpinned="$(missing_pin_entries)"
if git -C "$REPO_DIR" add -N . >/dev/null 2>&1 \
   && git -C "$REPO_DIR" diff --quiet \
   && [ -z "$unpinned" ]; then
  # Idempotent re-run: the selected plans' tests are already on the branch
  # and nothing new was written. Nothing to review, nothing to commit.
  echo "suite branch already carries the selected plans' tests — no new changes"
  review_rc=0
  REVIEW_FINDINGS=0
  REVIEW_PASSES_RUN=0
else
  if git -C "$REPO_DIR" diff --quiet; then
    echo "::error title=Empty diff::The session wrote nothing, and the selected plans' tests are not on the branch either. An empty diff has nothing to review — failing the leg."
    exit 1
  fi
  [ -z "$unpinned" ] || {
    echo "::error title=Unpinned tests::Some selected tests are missing from the pin file. The suite's compare mode treats an unpinned red test as a regression, so the leg fails here."
    exit 1
  }
  set +e
  run_review_loop "$REPO_DIR" "$WORKSPACE" "$DRIVER_HOME" "$MODEL" "$REVIEW_MODEL" \
    "$((BUDGET_MINUTES * 60))" "$MAX_REVIEW_PASSES" "$CRITERIA" "$GUARD" "$LOG_DIR" \
    "regression-test change"
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

# --- Red proof: the new tests must fail against the current tree ---------------
# Re-check the pin file first: a fix pass is just as able to delete a pin
# entry as a review pass is to demand one.
unpinned="$(missing_pin_entries)"
[ -z "$unpinned" ] || {
  echo "::error title=Unpinned tests::After the review loop some selected tests are missing from the pin file. The suite's compare mode treats an unpinned red test as a regression, so the leg fails here."
  exit 1
}
( cd "$REPO_DIR" && go build ./... ) || { echo "::error title=go build failed"; exit 1; }
( cd "$REPO_DIR" && go vet -tags=vuln ./... ) || { echo "::error title=go vet failed (the suite must compile)"; exit 1; }
red_log="$LOG_DIR/red-check.log"
set +e
( cd "$REPO_DIR" && go test -tags=vuln -run "$test_selector" ./... ) > "$red_log" 2>&1
red_rc=$?
set -e
if [ "$red_rc" -eq 0 ]; then
  echo "::error title=Tests are not red::The selected plans' tests PASS against the current tree, so they do not reproduce the findings. The whole red-to-green story collapses — failing the leg."
  exit 1
fi
if ! grep -q '^FAIL' "$red_log"; then
  echo "::error title=Red check inconclusive::go test exited $red_rc but reported no FAIL line — the tests did not run (see the archived log in the private repo)."
  exit 1
fi
echo "selected tests are red against the current tree, as they must be"

# --- Commit, push, pull request -----------------------------------------------
git -C "$REPO_DIR" add .
if ! git -C "$REPO_DIR" diff --cached --quiet; then
  git -C "$REPO_DIR" commit --quiet -m "test(security): regression tests for scan $(printf '%s' "$SCAN_DIR" | tr -c 'A-Za-z0-9._-' '-') plans $(printf '%s' "$SELECTED_IDS" | tr -c 'A-Za-z0-9._-' '-')"
  git -C "$REPO_DIR" remote set-url origin "https://x-access-token:${SECURITY_SCAN_PAT}@github.com/${GITHUB_REPOSITORY}.git"
  # Through archive_git for the redaction: a failed push prints the remote
  # URL, credentials included, and this job log is public.
  archive_git "$REPO_DIR" push --quiet -u origin "$test_branch" || {
    echo "::error title=Push failed::The suite branch commit exists locally but was not pushed. The leg fails; the commit is lost with the runner."
    exit 1
  }
else
  git -C "$REPO_DIR" fetch --quiet origin "$test_branch" 2>/dev/null || true
fi

body_file="$WORK/pr-body.md"
{
  echo "Refs #${OVERVIEW_ISSUE}"
  echo ""
  echo "Regression tests for plans \`$(printf '%s' "$SELECTED_IDS" | tr -c 'A-Za-z0-9._-' '-')\` of the current scan: $plan_count plan(s), all red and pinned in \`scripts/security-suite-expected-failures.txt\`. The matching fix pull requests turn them green and delete their pin lines."
  echo ""
  echo "Review loop: ${REVIEW_PASSES_RUN} pass(es), ${REVIEW_FINDINGS} unresolved findings."
  if [ "${REVIEW_FINDINGS:-0}" -gt 0 ] && [ -f "$LOG_DIR/REVIEW.md" ]; then
    echo ""
    echo "## Unresolved review findings"
    echo ""
    cat "$LOG_DIR/REVIEW.md"
  fi
  echo ""
  echo "---"
  echo "🤖 Generated with opencode"
} > "$body_file"

pr_url="$(gh pr list --head "$test_branch" --state open --json url --jq '.[0].url' 2>/dev/null || true)"
if [ -n "$pr_url" ]; then
  gh pr edit "$test_branch" --body-file "$body_file" >/dev/null
  echo "updated suite pull request: $pr_url"
else
  set +e
  pr_url="$(gh pr create --head "$test_branch" \
    --title "test(security): regression suite for scan $(printf '%s' "$SCAN_DIR" | tr -c 'A-Za-z0-9._-' '-')" \
    --body-file "$body_file" --label do-not-merge 2>&1)"
  pr_status=$?
  set -e
  if [ "$pr_status" -ne 0 ]; then
    echo "::error title=Pull request creation failed::The suite branch is pushed; the pull request is not open. Create the 'do-not-merge' label and re-run. Details: $pr_url"
    exit 1
  fi
  echo "opened suite pull request: $pr_url"
fi

# --- Archive the session logs privately -----------------------------------------
# Session and review logs can carry finding detail, so they go to the private
# repo, never to public artifacts.
stage_logs="$scan_abs/remediation/test-suite"
mkdir -p "$stage_logs"
cp "$LOG_DIR"/*.log "$stage_logs"/ 2>/dev/null || true
cp "$LOG_DIR"/REVIEW.md "$stage_logs"/ 2>/dev/null || true
push_archive "$ARCHIVE_DIR" "remediation: test-suite logs for ${SCAN_DIR} ($(date -u +%F))"

emit_output test_branch "$test_branch"
printf 'test-suite stage: %s plans, review %s/%s\n' "$plan_count" "$REVIEW_PASSES_RUN" "$REVIEW_FINDINGS" >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
