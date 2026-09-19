#!/usr/bin/env bash
# Integration test for fix-stage.sh's session state machine, with stubbed
# tools and stubbed opencode sessions: no Go, no network, no model calls.
#
# It drives the real stage end to end against a fixture repository and
# asserts the orchestration the plan depends on:
#
#   happy path      test writer -> red check -> test review -> fix writer ->
#                   green check -> fix review -> PR writer -> pull request
#                   open, with signed commits attributed to the right agent
#   retry path      an insufficient test review buys one test retry, an
#                   insufficient fix review one fix retry, then the PR still
#                   opens (findings applied but not re-reviewed)
#   not-red path    tests that pass before the fix fail the leg after the
#                   retry, and the branch is pushed with no pull request
#
# The prompts are the coupling point: a stub session recognises its phase by
# a phrase in the prompt, so a prompt rewrite that breaks the contract fails
# this test loudly.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
STAGE="$HERE/fix-stage.sh"

TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

# Isolate every git operation (the test's and the stage's runner shell, which
# inherits this environment) from the developer's global and system git
# config: no global hooks fire on fixture commits, no identity, signing or
# insteadOf rewrite leaks in or out. This is belt-and-braces — fixture repos
# already live under mktemp and their only remotes are a local bare repo or a
# bogus-token github URL — but it makes local runs touch nothing of the user.
: > "$TMP/empty.gitconfig"
export GIT_CONFIG_GLOBAL="$TMP/empty.gitconfig"
export GIT_CONFIG_SYSTEM="$TMP/empty.gitconfig"

failures=0
fail() { printf 'FAIL: %s\n' "$*" >&2; failures=$((failures + 1)); }

command -v git >/dev/null 2>&1 || { echo "git is required" >&2; exit 1; }
GIT_REAL="$(command -v git)"

# --- Stubs ----------------------------------------------------------------------
# git forwards everything except push, which is recorded instead of hitting a
# remote. gh answers the handful of queries the stage makes.
STUBS="$TMP/bin"
mkdir -p "$STUBS"
cat > "$STUBS/git" <<'EOF'
#!/usr/bin/env bash
d="$PWD"; real="$GIT_REAL"
while [ "$d" != "/" ]; do
  if [ -f "$d/.git-real" ]; then real="$(cat "$d/.git-real")"; break; fi
  d="$(dirname "$d")"
done
for a in "$@"; do
  if [ "$a" = push ]; then
    printf '%s\n' "$*" >> "$SIM_PUSH_LOG"
    exit 0
  fi
done
exec "$real" "$@"
EOF
cat > "$STUBS/gh" <<'EOF'
#!/usr/bin/env bash
# Real gh cannot infer the repo when the cwd is not a checkout (the stages run
# from the workspace), so repo-scoped commands must carry -R/--repo or GH_REPO.
# `gh repo view` is the exception: it takes the repository as a positional
# argument and REJECTS -R, exactly as this stub does — that mismatch is what
# broke a real run once, so it is encoded here.
case "$*" in
  *"repo view"*)
    for a in "$@"; do
      case "$a" in -R|--repo)
        echo "unknown shorthand flag: 'R' in -R" >&2
        exit 1 ;;
      esac
    done
    case "$*" in *"/"*) : ;; *)
      [ -n "${GH_REPO:-}" ] || { echo "gh: no repository context" >&2; exit 1; } ;;
    esac ;;
  *"pr list"*|*"pr create"*|*"pr edit"*|*"issue "*|*"label "*)
    if [ -z "${GH_REPO:-}" ]; then
      has_repo=0; prev=""
      for a in "$@"; do
        { [ "$prev" = -R ] || [ "$prev" = --repo ]; } && has_repo=1
        prev="$a"
      done
      if [ "$has_repo" -eq 0 ]; then
        echo "gh: no repository context: pass -R/--repo or set GH_REPO" >&2
        exit 1
      fi
    fi ;;
esac
case "$*" in
  *"pr list"*) case "$*" in *length*) echo 0 ;; *) echo "" ;; esac ;;
  *"pr create"*) printf 'create\n' >> "$SIM_PR_LOG"; echo "https://example.invalid/pr/1" ;;
  *"pr edit"*) echo ok ;;
  *"repo view"*) echo "main" ;;
esac
exit 0
EOF
cat > "$STUBS/curl" <<'EOF'
#!/usr/bin/env bash
echo 200
EOF
cat > "$STUBS/bun" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

# The stubs read their scenario from a .sim-mode file found by walking up
# from the cwd: the stage runs sessions and the go checks under env -i, so
# nothing can be injected through the environment any more.
cat > "$STUBS/go" <<'EOF'
#!/usr/bin/env bash
d="$PWD"; sim=""
while [ "$d" != "/" ]; do [ -f "$d/.sim-mode" ] && { sim="$(cat "$d/.sim-mode")"; break; }; d="$(dirname "$d")"; done
sub=$1; shift
case "$sub" in
  build) exit 0 ;;
  vet) [ "$sim" = brokenvet ] && { echo "vet: test file does not compile"; exit 1; }; exit 0 ;;
  test)
    has_run=0; prev=""
    for a in "$@"; do [ "$prev" = -run ] && has_run=1; prev="$a"; done
    grep -rq 'func TestSecurity' --include='*_security_test.go' . || { echo "no tests to run"; exit 0; }
    [ "$sim" = notred ] && exit 0
    fixed=0
    for f in $(find . -name 'prod*.go' -not -name '*_test.go'); do
      grep -q fixed "$f" && fixed=1
    done
    if [ "$has_run" -eq 1 ]; then
      [ "$fixed" -eq 1 ] && exit 0
      echo "--- FAIL: TestSecurityAlpha"; exit 1
    fi
    [ "$fixed" -eq 1 ] || { echo "--- FAIL: TestSecurityAlpha"; exit 1; }
    if [ "$sim" = stale ] && grep -rq 'asserts-old' --include='*_test.go' . \
       && ! grep -rq '// updated' --include='*_test.go' .; then
      echo "--- FAIL: TestOldBehaviour"; exit 1
    fi
    exit 0 ;;
esac
exit 0
EOF

# Each stub session recognises its phase by a phrase in its prompt. In retry
# mode the reviewers stay insufficient and each retry session appends one line
# so its commit is non-empty.
cat > "$STUBS/opencode" <<'EOF'
#!/usr/bin/env bash
d="$PWD"; root=""
while [ "$d" != "/" ]; do [ -f "$d/.sim-mode" ] && { root="$d"; break; }; d="$(dirname "$d")"; done
sim=""; [ -n "$root" ] && sim="$(cat "$root/.sim-mode")"
r="$PWD"; [ -d repo ] && r="$PWD/repo"
for last; do :; done
[ -n "$root" ] && printf '%s\n' "$last" >> "$root/.sim-sessions.log"
case "$last" in
  *"You are the test author"*)
    printf 'package pkg\n\nimport "testing"\n\nfunc TestSecurityAlpha(t *testing.T) {}\n' > "$r/pkg/alpha_security_test.go"
    if [ "$sim" = deleter ]; then rm -f "$r/pkg/existing_security_test.go"; fi
    if [ "$sim" = leak ]; then
      printf 'package pkg\n\nimport "testing"\n\n// Sim finding title\nfunc TestSecurityAlpha(t *testing.T) {}\n' > "$r/pkg/alpha_security_test.go"
    fi ;;
  *"did not pass the mechanical red"*)
    printf '// retried\n' >> "$r/pkg/alpha_security_test.go" ;;
  *"You are the implementer"*)
    echo fixed > "$r/pkg/prod.go"
    [ "$sim" = die ] && exit 3
    if [ "$sim" = committer ]; then
      git -C "$r" add -A
      git -C "$r" commit -qm "evil session commit"
    fi ;;
  *"is not finished"*)
    echo "// retry" >> "$r/pkg/prod.go"
    [ "$sim" = stale ] && echo "// updated" >> "$r/pkg/old_test.go" ;;
  *"independent reviewer of the regression tests"*)
    [ "$sim" = reviewer ] && echo tampered >> "$r/pkg/other.go"
    if [ "$sim" = retries ]; then
      echo "one finding" > REVIEW.md
      echo '{"findings":1,"verdict":"insufficient"}' > review-verdict.json
    else
      echo "no findings" > REVIEW.md
      echo '{"findings":0,"verdict":"approved"}' > review-verdict.json
    fi ;;
  *"independent reviewer of one security fix"*)
    if [ "$sim" = retries ] || [ "$sim" = strict ]; then
      echo "one finding" > REVIEW.md
      echo '{"findings":1,"verdict":"insufficient"}' > review-verdict.json
    else
      echo "no findings" > REVIEW.md
      echo '{"findings":0,"verdict":"approved"}' > review-verdict.json
    fi ;;
  *"write the pull request description"*)
    printf '**Issue:** Refs #288\n\n## What\n- fix\n' > PR_BODY.md
    [ "$sim" = prfiles ] && echo tampered >> "$r/pkg/other.go"
    if [ "$sim" = prcommitter ]; then git add PR_BODY.md && git commit -qm "body commit"; fi ;;
esac
exit 0
EOF
chmod +x "$STUBS"/*

# Fail loudly here, not three phases later, if the stubs are not first on the
# PATH the stage will run with: every stub is what keeps it away from the real
# git, gh, network and model. Probed explicitly so the test's own fixture git
# calls keep using the real binary.
for tool in git gh curl bun opencode go; do
  resolved="$(PATH="$STUBS:$PATH" command -v "$tool" 2>/dev/null || true)"
  if [ "$resolved" != "$STUBS/$tool" ]; then
    echo "stub resolution broken: '$tool' resolves to '${resolved:-nothing}', expected '$STUBS/$tool'" >&2
    exit 1
  fi
done

# --- Fixture --------------------------------------------------------------------
# A bare origin, a checkout on main, and an archive clone holding one plan
# that owns pkg/prod.go. The plan's branch is never fetched, so the stage
# starts from main.
SCAN="20260101-000000-sim-ok"
new_fixture() {
  local root=$1 mode=$2
  mkdir -p "$root"
  git init -q --bare "$root/origin.git"
  git -C "$root" clone -q "$root/origin.git" repo 2>/dev/null
  git -C "$root/repo" symbolic-ref HEAD refs/heads/main
  git -C "$root/repo" config user.email "test@example.com"
  git -C "$root/repo" config user.name "test"
  mkdir -p "$root/repo/pkg"
  echo vulnerable > "$root/repo/pkg/prod.go"
  echo other > "$root/repo/pkg/other.go"
  printf 'package pkg\n\nimport "testing"\n\nfunc TestSecurityExisting(t *testing.T) {}\n' \
    > "$root/repo/pkg/existing_security_test.go"
  printf 'package pkg\n\nimport "testing"\n\n// asserts-old\nfunc TestOldBehaviour(t *testing.T) {}\n' \
    > "$root/repo/pkg/old_test.go"
  git -C "$root/repo" add -A
  git -C "$root/repo" commit -qm "chore: base"
  git -C "$root/repo" push -q -u origin HEAD:main
  mkdir -p "$root/archive/scans/$SCAN/mitigation-plans"
  cat > "$root/archive/scans/$SCAN/mitigation-plans/plans.json" <<JSON
[
  {
    "id": "01",
    "priority": 1,
    "branch": "fix/security-$SCAN-plan-01",
    "plan_file": "mitigation-plans/01-origin.md",
    "files": ["pkg/prod.go"],
    "tests": ["TestSecurityAlpha"],
    "issue_line": "Harden the simulated property",
    "review_criteria": "closes the origin, not the symptom"
  }
]
JSON
  mkdir -p "$root/archive/scans/$SCAN/mitigation-plans"
  printf '%s' '[{"id":"vuln-0001","title":"Sim finding title","poc":"curl -H sim-poc http://target"}]' \
    > "$root/archive/scans/$SCAN/vulnerabilities.json"
  git init -q "$root/archive"
  git -C "$root/archive" remote add origin "https://github.com/sim/archive.git"
  git -C "$root/archive" config user.email "test@example.com"
  git -C "$root/archive" config user.name "test"
  : > "$root/pushes.log"
  : > "$root/prs.log"
  : > "$root/.sim-sessions.log"
  printf '%s\n' "$GIT_REAL" > "$root/.git-real"
  printf '%s\n' "$mode" > "$root/.sim-mode"
}

run_stage() {
  local root=$1 rc=0
  (
    cd "$root"
    env \
      PATH="$STUBS:$PATH" GIT_REAL="$GIT_REAL" \
      SIM_PUSH_LOG="$root/pushes.log" SIM_PR_LOG="$root/prs.log" \
      PLAN_ID=01 SCAN_DIR="$SCAN" OVERVIEW_ISSUE=288 WAVE_MAP='{"01":1}' \
      MODEL=sim-model REPO_DIR="$root/repo" ARCHIVE_DIR="$root/archive" \
      LOG_DIR="$root/logs" GITHUB_STEP_SUMMARY="$root/summary.md" \
      GITHUB_REPOSITORY="sim/repo" \
      SECURITY_SCAN_PAT=sim-pat REPO_TOKEN=sim-repo-token ARCHIVE_REPO="sim/archive" \
      SKAINET_TOKEN=sim SKAINET_INTERNAL="http://sim" \
      bash "$STAGE"
  ) >/dev/null 2>&1 || rc=$?
  return "$rc"
}

assert() {
  local desc=$1 want=$2 got=$3
  if [ "$want" = "$got" ]; then echo "ok: $desc"; else fail "$desc: want '$want', got '$got'"; fi
}

# Proof that no surprise remote was ever involved: after a leg has run, the
# fixture's origin is either its local bare repo (a leg that never pushed) or
# the bogus-token github URL the stage sets right before pushing.
assert_fake_origin() {
  local desc=$1 root=$2 url
  url="$(git -C "$root/repo" remote get-url origin)"
  case "$url" in
    "$root/origin.git"|*"github.com/sim/repo.git") echo "ok: $desc" ;;
    *) fail "$desc: unexpected origin '$url'" ;;
  esac
}

# --- Happy path -----------------------------------------------------------------
happy="$TMP/happy"
new_fixture "$happy" happy
if run_stage "$happy"; then echo "ok: happy path exits 0"; else fail "happy path exits 0"; fi
assert_fake_origin "the happy path only ever targets the fixture or the fake URL" "$happy"

subjects="$(git -C "$happy/repo" log --reverse --format=%s main..HEAD | paste -sd'|' -)"
assert "happy path commits tests then fix" \
  "test(security): regression tests for plan 01|fix(security): Harden the simulated property" \
  "$subjects"

test_sha="$(git -C "$happy/repo" log --format='%H %s' main..HEAD | awk '$2=="test(security):"{print $1; exit}')"
fix_sha="$(git -C "$happy/repo" log --format='%H %s' main..HEAD | awk '$2=="fix(security):"{print $1; exit}')"
assert "the test commit touches no production file" "no" \
  "$(git -C "$happy/repo" show --stat --format= "$test_sha" | grep -q 'prod.go' && echo yes || echo no)"
assert "the fix commit does not touch tests" "no" \
  "$(git -C "$happy/repo" show --stat --format= "$fix_sha" | grep -q '_security_test.go' && echo yes || echo no)"
assert "the test commit is attributed to the test writer" "Test Writer Agent" \
  "$(git -C "$happy/repo" log -1 --format=%an "$test_sha")"
assert "the test commit is signed off by the test writer" "Test Writer Agent <test-writer@security-remediate.invalid>" \
  "$(git -C "$happy/repo" log -1 --format=%b "$test_sha" | sed -n 's/^Signed-off-by: //p')"
assert "the fix commit is attributed to the fix writer" "Fix Writer Agent" \
  "$(git -C "$happy/repo" log -1 --format=%an "$fix_sha")"
assert "the pull request body was captured" "yes" \
  "$([ -s "$happy/logs/pr-body.md" ] && echo yes || echo no)"
assert "the happy path creates one pull request" "1" "$(grep -c create "$happy/prs.log" || true)"
assert "the fix branch is pushed once" "1" "$(grep -c "github.com/sim/repo.git" "$happy/pushes.log" || true)"
assert "the archive log is pushed once" "1" "$(grep -c "github.com/sim/archive.git" "$happy/pushes.log" || true)"

# Red and green checks both ran and both passed their gates.
assert "the red check ran" "yes" "$([ -s "$happy/logs/red-check.log" ] && echo yes || echo no)"
assert "the green check ran" "yes" "$([ -s "$happy/logs/green-check.log" ] && echo yes || echo no)"

# --- Retry path -----------------------------------------------------------------
# Both reviewers stay insufficient: one test retry and one fix retry run, then
# the pull request still opens.
retry="$TMP/retry"
new_fixture "$retry" retries
if run_stage "$retry"; then echo "ok: retry path exits 0"; else fail "retry path exits 0"; fi
assert_fake_origin "the retry path only ever targets the fixture or the fake URL" "$retry"

test_commits="$(git -C "$retry/repo" log --format=%s main..HEAD | grep -c 'test(security):' || true)"
fix_commits="$(git -C "$retry/repo" log --format=%s main..HEAD | grep -c 'fix(security):' || true)"
assert "the retry path makes two test commits" "2" "$test_commits"
assert "the retry path makes two fix commits" "2" "$fix_commits"
assert "the retry path still opens the pull request" "yes" \
  "$([ -s "$retry/logs/pr-body.md" ] && echo yes || echo no)"

# --- Not-red path ---------------------------------------------------------------
# The tests pass before the fix: after one retry the leg fails, the branch is
# pushed, and no pull request body exists.
notred="$TMP/notred"
new_fixture "$notred" notred
if run_stage "$notred"; then fail "not-red path fails the leg"; else echo "ok: not-red path fails the leg"; fi
assert_fake_origin "the not-red path only ever targets the fixture or the fake URL" "$notred"
assert "the not-red path pushes the branch" "1" "$(grep -c "github.com/sim/repo.git" "$notred/pushes.log" || true)"
assert "the not-red path pushes no archive log" "0" "$(grep -c "github.com/sim/archive.git" "$notred/pushes.log" || true)"
assert "the not-red path creates no pull request" "0" "$(grep -c create "$notred/prs.log" || true)"

# --- Broken-test path -----------------------------------------------------------
# The tests do not compile: without the go-vet gate the red check would call a
# build failure "red". After one retry the leg fails and the branch is pushed.
broken="$TMP/broken"
new_fixture "$broken" brokenvet
if run_stage "$broken"; then fail "broken-test path fails the leg"; else echo "ok: broken-test path fails the leg"; fi
assert "the broken-test path rejects the non-compiling red check" "yes" "$(grep -q 'does not compile' "$broken/logs/red-check.log" && echo yes || echo no)"
assert "the broken-test path opens no pull request" "no"   "$([ -s "$broken/logs/pr-body.md" ] && echo yes || echo no)"

# --- Session-death path ---------------------------------------------------------
# The fix writer dies mid-phase: its partial work is committed, the branch is
# pushed, no pull request is opened, and the next run can continue.
dead="$TMP/dead"
new_fixture "$dead" die
if run_stage "$dead"; then fail "session-death path fails the leg"; else echo "ok: session-death path fails the leg"; fi
assert "the session-death path pushes the partial branch" "1"   "$(grep -c "github.com/sim/repo.git" "$dead/pushes.log" || true)"
assert "the session-death path opens no pull request" "no"   "$([ -s "$dead/logs/pr-body.md" ] && echo yes || echo no)"
assert "the session-death path saves the partial fix as wip" "1" "$(git -C "$dead/repo" log --format=%s main..HEAD | grep -c '^wip(security):' || true)"

# --- Strict-review path ---------------------------------------------------------
# The fix is green but the review stays insufficient: strict_review fails the
# leg instead of opening the pull request.
strict="$TMP/strict"
new_fixture "$strict" strict
if STRICT_REVIEW=true run_stage "$strict"; then fail "strict-review path fails the leg"; else echo "ok: strict-review path fails the leg"; fi
assert "the strict-review path opens no pull request" "no"   "$([ -s "$strict/logs/pr-body.md" ] && echo yes || echo no)"

# --- PR-writer misbehaviour -----------------------------------------------------
# The description session edits a repository file: the change was never
# reviewed, so the leg fails and nothing is published.
prfiles="$TMP/prfiles"
new_fixture "$prfiles" prfiles
if run_stage "$prfiles"; then fail "PR-writer misbehaviour fails the leg"; else echo "ok: PR-writer misbehaviour fails the leg"; fi
assert "the PR-writer misbehaviour opens no pull request" "no"   "$([ -s "$prfiles/logs/pr-body.md" ] && echo yes || echo no)"

# --- Session-commit path --------------------------------------------------------
# With the sandbox on, the session's own commit cannot happen at all: .git is
# mounted read-only. The runner then commits the session's file writes as
# usual and the leg succeeds.
# The outcome depends on whether the environment running the stage has
# bubblewrap (CI's e2e job does not): with the sandbox the session cannot
# commit at all and the leg succeeds; without it the HEAD-unchanged guard
# discards the commit and fails the leg.
stage_has_bwrap() { PATH="$STUBS:$PATH" command -v bwrap >/dev/null 2>&1; }

committer="$TMP/committer"
new_fixture "$committer" committer
if stage_has_bwrap; then
  if run_stage "$committer"; then echo "ok: the sandboxed session cannot commit and the leg succeeds"; else fail "the sandboxed session cannot commit and the leg succeeds"; fi
  assert "the sandbox blocks the session commit" "0" \
    "$(git -C "$committer/repo" log --format=%s main..HEAD | grep -c 'evil session commit' || true)"
  assert "the sandboxed session still produces one pull request" "1" "$(grep -c create "$committer/prs.log" || true)"
else
  echo "note: no bubblewrap here; sessions run unsandboxed and the HEAD guard is the backstop"
  if run_stage "$committer"; then fail "unsandboxed session-commit fails the leg"; else echo "ok: unsandboxed session-commit fails the leg"; fi
  assert "the session commit is discarded" "0" \
    "$(git -C "$committer/repo" log --format=%s main..HEAD | grep -c 'evil session commit' || true)"
  assert "the session-commit creates no pull request" "0" "$(grep -c create "$committer/prs.log" || true)"
fi

# --- Session-commit path without the sandbox ------------------------------------
# On a host without bubblewrap the mount-level protection is absent, so the
# HEAD-unchanged guard must discard the session's commit and fail the leg.
nobwrap="$TMP/committer-nobwrap"
new_fixture "$nobwrap" committer
if OMAC_SESSION_SANDBOX=off run_stage "$nobwrap"; then fail "unsandboxed session-commit fails the leg"; else echo "ok: unsandboxed session-commit fails the leg"; fi
assert "the unsandboxed session commit is discarded" "0" \
  "$(git -C "$nobwrap/repo" log --format=%s main..HEAD | grep -c 'evil session commit' || true)"
assert "the unsandboxed session-commit creates no pull request" "0" "$(grep -c create "$nobwrap/prs.log" || true)"

# --- Test-deletion path ---------------------------------------------------------
# The test writer deletes an existing security test: rejected, leg fails.
deleter="$TMP/deleter"
new_fixture "$deleter" deleter
if run_stage "$deleter"; then fail "test-deletion path fails the leg"; else echo "ok: test-deletion path fails the leg"; fi
assert "the test-deletion path creates no pull request" "0" "$(grep -c create "$deleter/prs.log" || true)"

# --- Diff-disclosure path -------------------------------------------------------
# The test writer pastes the finding title into a public test: the pre-push
# gate rejects the diff.
leak="$TMP/leak"
new_fixture "$leak" leak
if run_stage "$leak"; then fail "diff-disclosure path fails the leg"; else echo "ok: diff-disclosure path fails the leg"; fi
assert "the diff-disclosure path creates no pull request" "0" "$(grep -c create "$leak/prs.log" || true)"

# --- Stale-test path ------------------------------------------------------------
# The fix breaks a pre-existing normal test. The full-suite green check catches
# it, the retry updates the test, and the pull request opens.
stale="$TMP/stale"
new_fixture "$stale" stale
if run_stage "$stale"; then echo "ok: stale-test path exits 0"; else fail "stale-test path exits 0"; fi
assert "the stale-test path retries once" "2" \
  "$(git -C "$stale/repo" log --format=%s main..HEAD | grep -c '^fix(security):' || true)"
assert "the stale-test path creates one pull request" "1" "$(grep -c create "$stale/prs.log" || true)"
assert "the stale test was corrected" "1" \
  "$(git -C "$stale/repo" show HEAD:pkg/old_test.go | grep -c '// updated' || true)"

# --- Reviewer-tamper path -------------------------------------------------------
# A review session edits the checkout: the change is discarded and the leg fails.
revtamper="$TMP/revtamper"
new_fixture "$revtamper" reviewer
if run_stage "$revtamper"; then fail "reviewer-tamper path fails the leg"; else echo "ok: reviewer-tamper path fails the leg"; fi
assert "the reviewer-tamper path creates no pull request" "0" "$(grep -c create "$revtamper/prs.log" || true)"
assert "the reviewer tamper is discarded" "" \
  "$(git -C "$revtamper/repo" status --porcelain | grep other.go || true)"

# --- PR-writer commit path ------------------------------------------------------
# Without the sandbox, the description session can commit its change; the
# commit must be discarded and the leg must fail.
prcommit="$TMP/prcommit"
new_fixture "$prcommit" prcommitter
if OMAC_SESSION_SANDBOX=off run_stage "$prcommit"; then fail "PR-writer commit path fails the leg"; else echo "ok: PR-writer commit path fails the leg"; fi
assert "the PR-writer commit path creates no pull request" "0" "$(grep -c create "$prcommit/prs.log" || true)"

if [ "$failures" -gt 0 ]; then
  echo "$failures fix-stage test(s) failed" >&2
  exit 1
fi
echo "fix-stage: all cases passed"
