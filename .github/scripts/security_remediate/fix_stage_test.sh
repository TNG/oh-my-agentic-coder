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
for a in "$@"; do
  if [ "$a" = push ]; then
    printf '%s\n' "$*" >> "$SIM_PUSH_LOG"
    exit 0
  fi
done
exec "$GIT_REAL" "$@"
EOF
cat > "$STUBS/gh" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  *"pr list"*) case "$*" in *length*) echo 0 ;; *) echo "" ;; esac ;;
  *"pr create"*) echo "https://example.invalid/pr/1" ;;
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

# The go stub emulates the one property that matters here: the security tests
# are red until a production file contains "fixed" (SIM_MODE=notred makes them
# green immediately, the "tests do not pin anything" failure).
cat > "$STUBS/go" <<'EOF'
#!/usr/bin/env bash
sub=$1; shift
case "$sub" in
  build|vet) exit 0 ;;
  test)
    grep -rq 'func TestSecurity' --include='*_security_test.go' . || { echo "no tests to run"; exit 0; }
    if [ "${SIM_MODE:-}" = notred ]; then exit 0; fi
    fixed=0
    for f in $(find . -name 'prod*.go' -not -name '*_test.go'); do
      grep -q fixed "$f" && fixed=1
    done
    [ "$fixed" -eq 1 ] && exit 0
    echo "--- FAIL: TestSecurityAlpha"; exit 1 ;;
esac
exit 0
EOF

# Each stub session recognises its phase by a phrase in its prompt. In retry
# mode the reviewers stay insufficient and each retry session appends one line
# so its commit is non-empty.
cat > "$STUBS/opencode" <<'EOF'
#!/usr/bin/env bash
r="$PWD"; [ -d repo ] && r="$PWD/repo"
for last; do :; done
printf '%s\n' "$last" >> "$SIM_SESSION_LOG"
case "$last" in
  *"You are the test author"*)
    printf 'package pkg\n\nimport "testing"\n\nfunc TestSecurityAlpha(t *testing.T) {}\n' > "$r/pkg/alpha_security_test.go" ;;
  *"did not pass the mechanical red"*)
    printf '// retried\n' >> "$r/pkg/alpha_security_test.go" ;;
  *"You are the implementer"*)
    echo fixed > "$r/pkg/prod.go" ;;
  *"is not finished"*)
    echo "// retry" >> "$r/pkg/prod.go" ;;
  *"independent reviewer of the regression tests"*)
    if [ "${SIM_MODE:-}" = retries ]; then
      echo "one finding" > REVIEW.md
      echo '{"findings":1,"verdict":"insufficient"}' > review-verdict.json
    else
      echo "no findings" > REVIEW.md
      echo '{"findings":0,"verdict":"approved"}' > review-verdict.json
    fi ;;
  *"independent reviewer of one security fix"*)
    if [ "${SIM_MODE:-}" = retries ]; then
      echo "one finding" > REVIEW.md
      echo '{"findings":1,"verdict":"insufficient"}' > review-verdict.json
    else
      echo "no findings" > REVIEW.md
      echo '{"findings":0,"verdict":"approved"}' > review-verdict.json
    fi ;;
  *"write the pull request description"*)
    printf '**Issue:** Refs #288\n\n## What\n- fix\n' > PR_BODY.md ;;
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
  git init -q "$root/archive"
  git -C "$root/archive" config user.email "test@example.com"
  git -C "$root/archive" config user.name "test"
  : > "$root/pushes.log"
  : > "$root/sessions.log"
  printf '%s\n' "$mode" > "$root/mode"
}

run_stage() {
  local root=$1 rc=0
  (
    cd "$root"
    env \
      PATH="$STUBS:$PATH" GIT_REAL="$GIT_REAL" \
      SIM_MODE="$(cat "$root/mode")" \
      SIM_PUSH_LOG="$root/pushes.log" SIM_SESSION_LOG="$root/sessions.log" \
      PLAN_ID=01 SCAN_DIR="$SCAN" OVERVIEW_ISSUE=288 WAVE_MAP='{"01":1}' \
      MODEL=sim-model REPO_DIR="$root/repo" ARCHIVE_DIR="$root/archive" \
      LOG_DIR="$root/logs" GITHUB_STEP_SUMMARY="$root/summary.md" \
      GITHUB_REPOSITORY="sim/repo" \
      SECURITY_SCAN_PAT=sim-pat ARCHIVE_REPO="sim/archive" \
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
assert "the branch is pushed once" "1" "$(grep -c -- '-u origin' "$happy/pushes.log" || true)"

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
assert "the not-red path pushes the branch" "1" "$(grep -c -- '-u origin' "$notred/pushes.log" || true)"
assert "the not-red path opens no pull request" "no" \
  "$([ -s "$notred/logs/pr-body.md" ] && echo yes || echo no)"

if [ "$failures" -gt 0 ]; then
  echo "$failures fix-stage test(s) failed" >&2
  exit 1
fi
echo "fix-stage: all cases passed"
