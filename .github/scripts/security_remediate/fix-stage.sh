#!/usr/bin/env bash
#
# fix-stage.sh — one leg of the fix waves (Job 4 of the security-remediate
# workflow). Runs once per plan, from the workflow's wave matrix.
#
# The leg is a small pipeline over four fresh opencode sessions, all
# orchestrated by this runner shell:
#
#   1. test-writer     writes the plan's regression tests (*_security_test.go,
#                      normal suite) and nothing else
#   2. red check       mechanical: the tests must compile and FAIL against the
#                      bare tree, and every planned test name must exist
#   3. test-reviewer   judges the tests against the plan; a failure on either
#                      #2 or #3 buys one test-writer retry, then the leg is
#                      pushed and failed
#   4. fix-writer      makes the tests green, without touching tests at all
#   5. green check     mechanical: build, vet and the whole promoted security
#                      suite pass
#   6. fix-reviewer    judges tests + fix against the plan; an "insufficient"
#                      verdict (or a failed green check) buys one fix-writer
#                      retry — that last pass may also remove impossible tests
#   7. pr-writer       writes the pull request body from the template, run
#                      from the repository checkout rather than the archive
#                      workspace, so it has no plan content to leak
#
# The runner owns every commit (agents only write files) and every credential
# operation (push, pull request), so the PAT never reaches a session. Each
# phase is committed before the next check runs, and a failed leg is always
# pushed: humans can inspect the branch, and the next run continues there
# instead of losing the work.
#
# Stacking: a plan sharing files with an earlier wave's plan whose pull
# request is open starts from that plan's branch, so its pull request merges
# after its base. Without an open base to stack on it starts from the default
# branch.
#
# Usage (from the job workspace, checkout under repo/):
#   bash repo/.github/scripts/security_remediate/fix-stage.sh
#
# Environment:
#   PLAN_ID            the plan this leg executes (required, from the matrix)
#   SCAN_DIR           scan directory name (required)
#   OVERVIEW_ISSUE     the overview issue number, for Refs
#   WAVE_MAP           JSON {"<plan id>": <wave>} from the waves job
#   MODEL, REVIEW_MODEL implementer / reviewer model ids
#   BUDGET_MINUTES     per-session wall-clock budget (default 55)
#   STRICT_REVIEW      true = an unresolved "insufficient" verdict fails the
#                      leg instead of opening the PR
#   REPO_DIR, ARCHIVE_DIR, LOG_DIR   (defaults ./repo, ./archive, ./logs)
#   GH_TOKEN            the write PAT (push, PR creation)
#   plus the lib.sh variables (ARCHIVE_REPO, SECURITY_SCAN_PAT, SKAINET_*)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

PLAN_ID="${PLAN_ID:-}"
SCAN_DIR="${SCAN_DIR:-}"
OVERVIEW_ISSUE="${OVERVIEW_ISSUE:-}"
WAVE_MAP="${WAVE_MAP:-{\}}"
MODEL="${MODEL:-}"
REVIEW_MODEL="${REVIEW_MODEL:-}"
BUDGET_MINUTES="${BUDGET_MINUTES:-55}"
STRICT_REVIEW="${STRICT_REVIEW:-false}"
REPO_DIR="${REPO_DIR:-$PWD/repo}"
ARCHIVE_DIR="${ARCHIVE_DIR:-$PWD/archive}"
LOG_DIR="${LOG_DIR:-$PWD/logs}"

# The PR body is a small, bounded writing task; it does not need a full
# implementation budget. Keeping it short is what keeps the worst-case leg
# (three writers + two reviewers + the PR writer) inside the job ceiling.
PR_WRITER_SECS=600

require_tools jq gh git go curl bun

if [ -z "$PLAN_ID" ] || [ -z "$SCAN_DIR" ]; then
  echo "::error title=Missing inputs::PLAN_ID and SCAN_DIR must be set (the workflow's wave matrix provides them)."
  exit 1
fi
if [ -z "$OVERVIEW_ISSUE" ]; then
  echo "::error title=No overview issue::OVERVIEW_ISSUE is not set — the fix pull request must reference the overview issue."
  exit 1
fi
case "$BUDGET_MINUTES" in ''|*[!0-9]*) echo "::error::budget_minutes must be a whole number" >&2; exit 2 ;; esac
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
my_files="$(printf '%s' "$my_plan" | jq -r '.files | join(", ")')"
plan_tests_csv="$(printf '%s' "$my_plan" | jq -r '.tests | join(", ")')"
test_selector="$(printf '%s' "$my_plan" | jq -r '.tests[]' | sort -u | sed 's/^/^/; s/$/$/' | paste -sd'|' -)"

if [ -n "$(git -C "$REPO_DIR" status --porcelain)" ]; then
  echo "::error title=Dirty checkout::The source checkout is not clean; the ownership guards could not attribute changes. Failing the leg."
  exit 1
fi

# --- Branch and base ------------------------------------------------------------
# The default branch comes from the API: actions/checkout leaves a detached
# HEAD, so origin/HEAD is not reliable in a shallow clone.
default_branch="$(gh repo view --json defaultBranchRef --jq '.defaultBranchRef.name')"
[ -n "$default_branch" ] || { echo "::error title=No default branch::gh returned no default branch name." >&2; exit 1; }

base_branch=""
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
[ -n "$base_branch" ] || base_branch="$default_branch"
git -C "$REPO_DIR" fetch --quiet origin "$base_branch"

if git -C "$REPO_DIR" fetch --quiet origin "$my_branch" 2>/dev/null; then
  echo "continuing plan $PLAN_ID on its existing branch $my_branch"
  git -C "$REPO_DIR" checkout --quiet -B "$my_branch" "origin/$my_branch"
else
  echo "starting plan $PLAN_ID from $base_branch"
  git -C "$REPO_DIR" checkout --quiet -B "$my_branch" "origin/$base_branch"
fi

# The fork point, after the checkout: for a stacked branch this is the stack
# tip, so the red-check tree and the reviewer's diff contain exactly this
# leg's commits.
base_sha="$(git -C "$REPO_DIR" merge-base HEAD "origin/$base_branch")"

# --- Guard patterns and prompt scaffolding ---------------------------------------
mkdir -p "$LOG_DIR"
WORK="$(mktemp -d)"
trap 'chmod -R u+w "$WORK" 2>/dev/null; rm -rf "$WORK"' EXIT
plan_tests_file="$WORK/plan-tests.txt"
printf '%s\n' "$my_plan" | jq -r '.tests[]' > "$plan_tests_file"

# Test sessions own security test files only; fix sessions own the plan's
# files only; only the final fix pass may also touch tests, so it alone gets
# the combined pattern. The guard files live outside the session workspace so
# no session can read its own leash.
guard_tests="$WORK/guard-tests"
printf '%s\n' '_security_test\.go$' > "$guard_tests"
guard_fix="$WORK/guard-fix"
printf '%s\n' "$my_plan" | jq -r '.files[]' | sed 's/[^A-Za-z0-9_/.-]/\\&/g; s/^/^/; s/$/$/' > "$guard_fix"
guard_fix_retry="$WORK/guard-fix-retry"
{ cat "$guard_fix"; printf '%s\n' '_security_test\.go$'; } > "$guard_fix_retry"

MODEL="${MODEL:-$(bash "$REPO_DIR/scripts/resolve-model.sh" opencode)}"
REVIEWER="${REVIEW_MODEL:-$MODEL}"
install_opencode
DRIVER_HOME="$(session_home "$WORK" "$MODEL" "$REVIEWER")"

# --- Runner helpers ---------------------------------------------------------------
push_branch() {
  git -C "$REPO_DIR" remote set-url origin "https://x-access-token:${SECURITY_SCAN_PAT}@github.com/${GITHUB_REPOSITORY}.git"
  archive_git "$REPO_DIR" push --quiet -u origin "$my_branch"
}

has_commits_beyond_base() {
  [ "$(git -C "$REPO_DIR" rev-parse HEAD)" != "$base_sha" ]
}

# Fail the leg, but never silently: leftovers are committed as a wip commit
# and the branch is pushed so a human can inspect exactly what the pipeline
# produced, and so the next run can continue on it.
fail_leg() {
  local title=$1 msg=$2
  if [ -n "$(git -C "$REPO_DIR" status --porcelain)" ]; then
    git -C "$REPO_DIR" add -A
    commit_as "$REPO_DIR" "$RUNNER_NAME" "$RUNNER_EMAIL" "wip(security): plan $PLAN_ID — pipeline leg failed" || true
  fi
  if has_commits_beyond_base; then
    push_branch || echo "::warning title=Push failed::The branch could not be pushed; its work is lost with the runner."
  fi
  echo "::error title=${title}::${msg}"
  exit 1
}

# Run one writer session in $1, log to $2, enforce the $3 ownership guard, and
# commit as $4 <$5> with message $6. Returns 1 when the session produced no
# changes (empty diffs are legal in the retry phases, decided by the caller).
run_phase() {
  local prompt=$1 log=$2 guard=$3 name=$4 email=$5 message=$6 status=0
  set +e
  run_session "$DRIVER_HOME" "$((BUDGET_MINUTES * 60))" "$MODEL" "$WORKSPACE" "$prompt" "$log"
  status=$?
  set -e
  echo "session exit status: $status"
  if [ "$status" -ne 0 ]; then
    fail_leg "Session incomplete" "The session exited with status $status mid-phase; its output is committed to the branch and the next run continues there."
  fi
  if [ -n "$(changed_files_within "$REPO_DIR" "$guard")" ]; then
    fail_leg "Ownership guard tripped" "The session edited files outside its allowed set; its output is on the branch as a wip commit and no pull request was opened."
  fi
  git -C "$REPO_DIR" add -N . >/dev/null 2>&1 || true
  if git -C "$REPO_DIR" diff --quiet; then
    return 1
  fi
  git -C "$REPO_DIR" add -A
  commit_as "$REPO_DIR" "$name" "$email" "$message"
  return 0
}

# The red proof: at $1 the plan's tests must exist, compile, and FAIL. A tree
# that does not compile, or tests that pass, is not red.
red_check() {
  local sha=$1 rc=0
  git -C "$REPO_DIR" checkout --quiet --detach "$sha"
  {
    echo "== red check at $sha =="
    echo "== the plan's tests must fail against this tree =="
  } > "$LOG_DIR/red-check.log"
  if ! ( cd "$REPO_DIR" && go build ./... ) >> "$LOG_DIR/red-check.log" 2>&1; then
    echo "red check: the test-only tree does not compile"
    rc=1
  elif [ -n "$(missing_test_names "$REPO_DIR" "$plan_tests_file")" ]; then
    echo "red check: planned test names are missing from the tree"
    rc=1
  elif ( cd "$REPO_DIR" && go test -run "$test_selector" ./... ) >> "$LOG_DIR/red-check.log" 2>&1; then
    echo "red check: the tests PASS before the fix — not red"
    rc=1
  fi
  git -C "$REPO_DIR" checkout --quiet "$my_branch"
  return "$rc"
}

# The green proof: at HEAD the tree builds, vets, and the whole promoted
# security suite passes — not just the plan's selector, because a session may
# have added or inverted security tests along the way.
green_check() {
  {
    echo "== green check at $(git -C "$REPO_DIR" rev-parse --short HEAD) =="
    echo "== the whole promoted security suite must pass =="
  } > "$LOG_DIR/green-check.log"
  ( cd "$REPO_DIR" && go build ./... ) >> "$LOG_DIR/green-check.log" 2>&1 || { echo "green check: go build failed"; return 1; }
  ( cd "$REPO_DIR" && go vet ./... ) >> "$LOG_DIR/green-check.log" 2>&1 || { echo "green check: go vet failed"; return 1; }
  ( cd "$REPO_DIR" && go test -run 'TestSecurity' ./... ) >> "$LOG_DIR/green-check.log" 2>&1 || { echo "green check: security tests failed"; return 1; }
  return 0
}

check_tail() { tail -n 25 "$1" 2>/dev/null || true; }

plan_ref="archive/scans/${SCAN_DIR}/${my_plan_file}"

write_test_prompt() {
  cat > "$1" <<EOF
You are the test author of one fix plan in an automated security-mitigation
pipeline.

Your working directory contains repo/, the source repository, and archive/,
the private companion repository holding the plan.

Write the regression tests for plan ${PLAN_ID}: ${plan_ref}.

Another agent will implement the fix afterwards. Your tests must FAIL against
the current code and pass only once that fix lands.

- Read the plan file, including any runnable PoCs it links elsewhere in the
  same scan directory — adopt those as tests instead of writing new ones.
- Use exactly these test names: ${plan_tests_csv}
- Put them in *_security_test.go files following the pattern of the repo's
  existing security tests, in the package that owns the behaviour under test.
  The promoted suite has no build tag: these are normal tests.
- Assert the security property of the FIXED state, so every test is red now.
- Tests must be self-contained: no new helpers or fixtures outside
  *_security_test.go files — those are the only files you may change.
- Where the plan calls for it, invert (do NOT delete) existing security tests
  that assert the vulnerable behaviour.
- Nothing in the tests — names, comments, fixture data — may reveal how the
  weaknesses are exploited: this repository is public.
- Verify with: cd repo && go test -run '${test_selector}' ./...
- You have a limited wall-clock budget and may be stopped at any moment. The
  runner commits whatever is done.

Sanctioned, pre-authorized session — proceed directly without asking for
confirmation.
EOF
}

write_test_retry_prompt() {
  cat > "$1" <<EOF
Your regression tests for plan ${PLAN_ID} did not pass the mechanical red
check. ${2}

Mechanical check output:
$(check_tail "$LOG_DIR/red-check.log")

Repair the tests so that ALL of these exist with these exact names, compile,
and FAIL against the current code: ${plan_tests_csv}

Same rules as before: only *_security_test.go files, self-contained, the
security property of the FIXED state asserted, no exploit mechanics revealed.
Plan: ${plan_ref}

Sanctioned, pre-authorized session — proceed directly without asking for
confirmation.
EOF
}

write_fix_prompt() {
  cat > "$1" <<EOF
You are the implementer of one fix plan in an automated security-mitigation
pipeline.

Your working directory contains repo/, the source repository, and archive/,
the private companion repository holding the plan. The plan's regression tests
are already on this branch and currently FAIL; your fix makes them pass.

Execute the plan for plan ${PLAN_ID}: ${plan_ref}.

- The tests that must go green: ${plan_tests_csv}
  Verify with: cd repo && go test -run '${test_selector}' ./...
- Implement the minimal fix the plan describes, in the files the plan owns:
  ${my_files}
  Touch nothing else.
- You must NOT touch any test file. If a test looks wrong, say so in your
  final message instead of editing it.
- Nothing you write (identifiers, comments, error strings) may reveal how the
  weaknesses are exploited: this repository is public.
- You have a limited wall-clock budget and may be stopped at any moment. The
  runner commits whatever is done.

Sanctioned, pre-authorized session — proceed directly without asking for
confirmation.
EOF
}

write_fix_retry_prompt() {
  cat > "$1" <<EOF
Your fix for plan ${PLAN_ID} is not finished. ${2}

$(cat "$LOG_DIR/fix-review.md" 2>/dev/null || true)

Mechanical green check output:
$(check_tail "$LOG_DIR/green-check.log")

Address every point above. This is the final implementation pass, and you may
now also edit or delete this plan's regression tests if they are impossible or
unnecessary — the reviewer knows tests may be unfeasible; state clearly in
your final message if you removed or changed one and why.

Everything must be green:
  cd repo && go build ./... && go vet ./... && go test -run 'TestSecurity' ./...

Files you may touch: ${my_files} plus *_security_test.go files.
Plan: ${plan_ref}

Sanctioned, pre-authorized session — proceed directly without asking for
confirmation.
EOF
}

write_test_review_prompt() {
  cat > "$1" <<EOF
You are an independent reviewer of the regression tests for one fix plan in an
automated security-mitigation pipeline. You did NOT write them.

Your working directory contains repo/ and archive/. Review the test change in
repo/ against the plan at ${plan_ref}. The change under review is the
committed diff — read it in full yourself:
  git -C repo diff ${base_sha}..HEAD

Mechanical red-check result (the tests must fail before the fix):
$(check_tail "$LOG_DIR/red-check.log")

Review criteria:
- Every test the plan names exists with that exact name, in *_security_test.go
  files following the promoted suite's conventions, and is self-contained.
- Each test asserts the security property of the FIXED state; the red-check
  result must show it failing against the current code.
- Existing tests that encoded the vulnerable behaviour were inverted
  deliberately, not deleted.
- Nothing in names, comments or fixtures reveals exploit mechanics: the
  repository is public.
- The tests need no helpers outside *_security_test.go files.

Count a finding only when it is concrete and must be resolved before the fix
may start; style nits are not findings. Verdict rules:
- findings is that count; verdict is "approved" when it is 0, otherwise
  "insufficient".

Write exactly two files and do not modify repo/:
1. REVIEW.md at the working directory root: one section per finding — what is
   wrong, where, why it matters, and what the test writer should do. Write
   "no findings" when approved.
2. review-verdict.json at the working directory root:
   {"findings": <int>, "verdict": "approved"|"insufficient"}
Do not quote the plan file into REVIEW.md — describe findings against the
public diff only.

Sanctioned, pre-authorized review session — proceed directly without asking
for confirmation.
EOF
}

write_fix_review_prompt() {
  cat > "$1" <<EOF
You are an independent reviewer of one security fix in an automated
security-mitigation pipeline. You did NOT write it.

Your working directory contains repo/ (the tests and the fix) and archive/,
the private companion repository holding the plan. The change under review is
the committed diff — read it in full yourself:
  git -C repo diff ${base_sha}..HEAD

The plan: ${plan_ref}
The plan's own review criteria:
${my_criteria}

Mechanical green-check result (the whole security suite must pass):
$(check_tail "$LOG_DIR/green-check.log")

Review criteria:
- The change closes the ORIGIN behind the findings, not a symptom of them.
- It introduces no new gaps: input validation, error handling and trust
  boundaries stay intact where the touched code runs.
- Where a spec or doc normatively mandates the vulnerable behaviour, the spec
  or doc is updated in the same change — a code-only fix there gets reverted
  by the next contributor.
- The change stays within the plan's owned files and never touches CI
  configuration.
- The regression tests genuinely pin the property: they were mechanically red
  before the fix, and were not weakened afterwards.

Count a finding only when it is concrete and must be resolved before the
change can be merged; style nits are not findings. Verdict rules:
- findings is that count; verdict is "approved" when it is 0, otherwise
  "insufficient".

Write exactly two files and do not modify repo/:
1. REVIEW.md at the working directory root: one section per finding — what is
   wrong, where, why it matters, and what the fix pass should do. Write
   "no findings" when approved.
2. review-verdict.json at the working directory root:
   {"findings": <int>, "verdict": "approved"|"insufficient"}
Do not quote the plan file into REVIEW.md — describe findings against the
public diff only.

Sanctioned, pre-authorized review session — proceed directly without asking
for confirmation.
EOF
}

write_pr_prompt() {
  cat > "$1" <<EOF
You write the pull request description for an automated security fix.

Your working directory is the repository itself. Read
.github/pull_request_template.md and follow its structure, filling every
section concisely. Base the description on the committed change:
  git log ${base_sha}..HEAD --format=%s
  git diff ${base_sha}..HEAD --stat

Requirements:
- In the template's Issue line use exactly: Refs #${OVERVIEW_ISSUE}
  Never "Closes": other pull requests target the same issue.
- Be concise: 1-4 bullets in What/Why/How, no restating what the diff shows.
- The Verification section states that go build, go vet and the security
  regression tests pass.
- Never describe how the underlying vulnerability is exploited, and do not
  mention plan ids, the private archive, or review files.
- Do NOT change anything in the repository and do NOT create commits: write
  the finished body to ./PR_BODY.md and nothing else.

Sanctioned, pre-authorized session — proceed directly without asking for
confirmation.
EOF
}

# --- Phase: tests -----------------------------------------------------------------
last_test="$(last_test_commit "$REPO_DIR" "$base_sha")"

if [ -z "$last_test" ]; then
  echo "phase: writing tests"
  write_test_prompt "$WORK/test-prompt.md"
  if ! run_phase "$WORK/test-prompt.md" "$LOG_DIR/test-writer.log" "$guard_tests" \
      "$TEST_WRITER_NAME" "$TEST_WRITER_EMAIL" \
      "test(security): regression tests for plan $PLAN_ID"; then
    fail_leg "No tests written" "The test session produced no changes, so there is nothing to pin the fix."
  fi
  last_test="$(git -C "$REPO_DIR" rev-parse HEAD)"

  red_ok=false
  red_reason="the tests did not fail against the current code"
  if red_check "$last_test"; then
    red_ok=true
  fi

  test_review_ok=false
  if [ "$red_ok" = true ]; then
    write_test_review_prompt "$WORK/test-review-prompt.md"
    run_review_session "$DRIVER_HOME" "$((BUDGET_MINUTES * 60))" "$REVIEWER" "$WORKSPACE" \
      "$WORK/test-review-prompt.md" "$LOG_DIR/test-review.log" "$LOG_DIR/test-review.md" \
      || fail_leg "Test review produced no verdict" "The reviewer session wrote no usable verdict; failing the leg rather than guessing."
    [ "$REVIEW_VERDICT" = approved ] && test_review_ok=true || red_reason="the test review found the suite insufficient"
  fi

  if [ "$red_ok" != true ] || [ "$test_review_ok" != true ]; then
    echo "phase: test retry"
    write_test_retry_prompt "$WORK/test-retry-prompt.md" "$red_reason"
    if ! run_phase "$WORK/test-retry-prompt.md" "$LOG_DIR/test-retry.log" "$guard_tests" \
        "$TEST_WRITER_NAME" "$TEST_WRITER_EMAIL" \
        "test(security): regression tests for plan $PLAN_ID"; then
      fail_leg "Test retry wrote nothing" "The retry session produced no changes; the tests still do not pin the fix."
    fi
    last_test="$(git -C "$REPO_DIR" rev-parse HEAD)"
    red_check "$last_test" || fail_leg "Tests are not red" "After the retry the plan's tests still do not fail against the current code (missing, not compiling, or passing). The branch carries the attempt."
  fi
else
  echo "phase: verifying the existing tests are red"
  if ! red_check "$last_test"; then
    write_test_retry_prompt "$WORK/test-retry-prompt.md" "the tests did not fail against the current code"
    if ! run_phase "$WORK/test-retry-prompt.md" "$LOG_DIR/test-retry.log" "$guard_tests" \
        "$TEST_WRITER_NAME" "$TEST_WRITER_EMAIL" \
        "test(security): regression tests for plan $PLAN_ID"; then
      fail_leg "Test retry wrote nothing" "The retry session produced no changes; the tests still do not pin the fix."
    fi
    last_test="$(git -C "$REPO_DIR" rev-parse HEAD)"
    red_check "$last_test" || fail_leg "Tests are not red" "After the retry the plan's tests still do not fail against the current code (missing, not compiling, or passing). The branch carries the attempt."
  fi
fi

# --- Phase: fix -------------------------------------------------------------------
fix_ok=false
fix_review_ok=false

if green_check; then
  echo "phase: already green"
  fix_ok=true
else
  echo "phase: writing the fix"
  write_fix_prompt "$WORK/fix-prompt.md"
  # An empty diff is not fatal here: the review below sees the green result
  # and can send it back once with the test-removal permission.
  run_phase "$WORK/fix-prompt.md" "$LOG_DIR/fix-writer.log" "$guard_fix" \
    "$FIX_WRITER_NAME" "$FIX_WRITER_EMAIL" \
    "fix(security): $my_issue_line" || true
  green_check && fix_ok=true || fix_ok=false
fi

echo "phase: reviewing the fix"
write_fix_review_prompt "$WORK/fix-review-prompt.md"
run_review_session "$DRIVER_HOME" "$((BUDGET_MINUTES * 60))" "$REVIEWER" "$WORKSPACE" \
  "$WORK/fix-review-prompt.md" "$LOG_DIR/fix-review.log" "$LOG_DIR/fix-review.md" \
  || fail_leg "Fix review produced no verdict" "The reviewer session wrote no usable verdict; failing the leg rather than guessing."
[ "$REVIEW_VERDICT" = approved ] && fix_review_ok=true

if [ "$fix_ok" != true ] || [ "$fix_review_ok" != true ]; then
  echo "phase: fix retry (tests may be adjusted)"
  if [ "$fix_ok" = true ]; then
    reason="the independent review found ${REVIEW_FINDINGS} unresolved point(s)"
  else
    reason="the security tests are not green"
  fi
  write_fix_retry_prompt "$WORK/fix-retry-prompt.md" "$reason"
  run_phase "$WORK/fix-retry-prompt.md" "$LOG_DIR/fix-retry.log" "$guard_fix_retry" \
    "$FIX_WRITER_NAME" "$FIX_WRITER_EMAIL" \
    "fix(security): $my_issue_line" || true
  green_check || fail_leg "Tests are not green" "After the final implementation pass the security suite still fails. The branch carries the attempt; no pull request was opened."
fi

# A planned test missing from the final tree can only have been deleted by a
# session with test-edit permission — the first fix writer is barred by its
# ownership guard, and a test retry is followed by a red check that requires
# every planned name. So a removal here is legitimate (the final pass found a
# test impossible or unnecessary); it is reported rather than failing the leg.
removed_tests="$(missing_test_names "$REPO_DIR" "$plan_tests_file")"
if [ "$fix_review_ok" != true ] && [ "$STRICT_REVIEW" = true ]; then
  fail_leg "Strict review" "The review ended insufficient with ${REVIEW_FINDINGS} findings and strict_review is on; failing instead of opening the pull request."
fi

note=""
if [ -n "$removed_tests" ]; then
  note="; tests removed by the final pass: $(printf '%s' "$removed_tests" | paste -sd', ' -)"
fi
write_pr_prompt "$WORK/pr-prompt.md"
set +e
run_session "$DRIVER_HOME" "$PR_WRITER_SECS" "$MODEL" "$REPO_DIR" "$WORK/pr-prompt.md" "$LOG_DIR/pr-writer.log"
pr_session_status=$?
set -e
echo "pr-writer session exit status: $pr_session_status"

body_file="$REPO_DIR/PR_BODY.md"
if [ "$pr_session_status" -ne 0 ] || [ ! -s "$body_file" ]; then
  # The description is a convenience; the fix itself is proven. Fall back to
  # a mechanical body rather than losing the pull request.
  {
    echo "Refs #${OVERVIEW_ISSUE}"
    echo ""
    echo "Automated security fix for one planned workstream. The plan's regression tests are green and the full security suite passes."
    echo ""
    echo "Review: $([ "$fix_review_ok" = true ] && echo approved || echo "insufficient (${REVIEW_FINDINGS} findings; a follow-up pass applied them, not re-reviewed)")."
  } > "$body_file"
  echo "::warning title=Pull request body fallback::The description session wrote no body; using the mechanical fallback."
fi
# The description writer may only produce the body file: anything else it
# touched means it misunderstood the task, and the change would be unproven.
other_changes="$(git -C "$REPO_DIR" status --porcelain | grep -v 'PR_BODY.md' || true)"
if [ -n "$other_changes" ]; then
  rm -f "$body_file"
  fail_leg "PR writer modified the repository" "The description session changed files beyond PR_BODY.md; those changes were not reviewed, so the leg fails."
fi
if ! grep -qF "Refs #${OVERVIEW_ISSUE}" "$body_file"; then
  printf 'Refs #%s\n\n%s' "$OVERVIEW_ISSUE" "$(cat "$body_file")" > "$body_file.tmp"
  mv "$body_file.tmp" "$body_file"
fi
body_copy="$LOG_DIR/pr-body.md"
cp "$body_file" "$body_copy"
rm -f "$body_file"

push_branch || fail_leg "Push failed" "The fix branch was committed locally but not pushed; the leg fails and the commit is lost with the runner."

stacked_on=""
if [ "$base_branch" != "$default_branch" ] && [ "$base_branch" != "$my_branch" ]; then
  stacked_on="$base_branch"
fi

pr_url="$(gh pr list --head "$my_branch" --state open --json url --jq '.[0].url' 2>/dev/null || true)"
if [ -n "$pr_url" ]; then
  gh pr edit "$my_branch" --body-file "$body_copy" >/dev/null
  echo "updated fix pull request: $pr_url"
else
  set +e
  if [ -n "$stacked_on" ]; then
    pr_url="$(gh pr create --head "$my_branch" --base "$stacked_on" \
      --title "fix(security): $my_issue_line" --body-file "$body_copy" --label do-not-merge 2>&1)"
  else
    pr_url="$(gh pr create --head "$my_branch" \
      --title "fix(security): $my_issue_line" --body-file "$body_copy" --label do-not-merge 2>&1)"
  fi
  pr_status=$?
  set -e
  if [ "$pr_status" -ne 0 ]; then
    fail_leg "Pull request creation failed" "The fix branch is pushed; the pull request is not open. Create the 'do-not-merge' label and re-run. Details: $pr_url"
  fi
  echo "opened fix pull request: $pr_url"
fi

# --- Archive the session logs privately ------------------------------------------
stage_logs="$scan_abs/remediation/fix-$PLAN_ID"
mkdir -p "$stage_logs"
cp "$LOG_DIR"/*.log "$LOG_DIR"/*.md "$stage_logs"/ 2>/dev/null || true
push_archive "$ARCHIVE_DIR" "remediation: fix logs for ${SCAN_DIR} plan ${PLAN_ID} ($(date -u +%F))"

printf 'fix stage: plan %s, fix review %s (%s findings), tests %s%s\n' \
  "$PLAN_ID" "$REVIEW_VERDICT" "$REVIEW_FINDINGS" "$([ "$fix_ok" = true ] && echo green || echo retried)" "$note" \
  >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
