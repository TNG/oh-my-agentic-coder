#!/usr/bin/env bash
# Tests for the security-remediate pipeline's pure logic in lib.sh:
#
#   select_plans        the budget state machine — priority order, dropping
#                       plans whose branch has an open or merged pull
#                       request, intersecting with the operator's id filter,
#                       capping at max_plans. A regression here silently
#                       re-runs finished plans or skips deferred ones, and
#                       neither failure is visible in the agent sessions.
#
#   plans_schema_errors the manifest contract every later stage depends on:
#                       generation-labelled branch names, no .github/
#                       ownership, TestSecurity* test names, required text
#                       fields. A regression here lets a malformed manifest
#                       flow into PR creation with finding data attached.
#
#   sanitize_issue_body  the disclosure gate for the public overview issue.
#
#   changed_files_within the ownership guard the stages enforce after every
#                       agent session: staged, unstaged, untracked and
#                       renamed paths must all stay within the allowed
#                       patterns. A regression here lets a session edit
#                       production code or CI from a test-writing prompt.
#
# A stub `gh` stands in for the PR API; the fixtures are synthetic. No
# credentials, no network.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

TMP="$(mktemp -d)"
cleanup() { rm -rf "$TMP"; }
trap cleanup EXIT

# Keep fixture git operations away from the developer's global and system git
# config — hooks, identity, signing, URL rewrites — so a local run touches
# nothing of the user's environment. The fixtures live under mktemp regardless.
: > "$TMP/empty.gitconfig"
export GIT_CONFIG_GLOBAL="$TMP/empty.gitconfig"
export GIT_CONFIG_SYSTEM="$TMP/empty.gitconfig"

failures=0
fail() { printf 'FAIL: %s\n' "$*" >&2; failures=$((failures + 1)); }

expect() {
  local desc=$1 want_sel=$2 want_def=$3 got_sel=$4 got_def=$5
  if [ "$got_sel" = "$want_sel" ] && [ "$got_def" = "$want_def" ]; then
    echo "ok: $desc"
  else
    fail "$desc: want selected '$want_sel' deferred $want_def, got '$got_sel' $got_def"
  fi
}

# --- Fixture manifest and stubbed PR list ------------------------------------
# Sorted by (priority, id) the fixture order is 02, 03, 01, 04; 02 has an
# open PR and 09 a merged one, so the eligible order is 03, 01, 04.
cat > "$TMP/plans.json" <<'EOF'
[
  { "id": "01", "priority": 2, "branch": "fix/security-s1-plan-01" },
  { "id": "02", "priority": 1, "branch": "fix/security-s1-plan-02" },
  { "id": "03", "priority": 1, "branch": "fix/security-s1-plan-03" },
  { "id": "04", "priority": 3, "branch": "fix/security-s1-plan-04" }
]
EOF
mkdir -p "$TMP/bin"
cat > "$TMP/bin/gh" <<'EOF'
#!/usr/bin/env bash
# Stub: real gh applies the caller's --jq filter itself and prints the
# covered headRefNames one per line, so this stub just prints those lines
# (one open PR for plan 02, one merged for a plan that is not in the
# fixture).
printf '%s\n' \
  'fix/security-s1-plan-02' \
  'fix/security-s1-plan-09'
EOF
chmod +x "$TMP/bin/gh"
PATH="$TMP/bin:$PATH"
export GITHUB_REPOSITORY="example/repo"

run_selection() {
  { read -r sel; read -r def; } <<< "$(select_plans "$TMP/plans.json" "$1" "$2")"
}

# --- Cases --------------------------------------------------------------------
run_selection 2 ""
expect "budget takes eligible plans in priority order" "03,01" 1 "$sel" "$def"

run_selection 10 ""
expect "large budget selects every eligible plan" "03,01,04" 0 "$sel" "$def"

run_selection 3 "04"
expect "id filter restricts the selection" "04" 0 "$sel" "$def"

run_selection 2 "03,01,04"
expect "filter keeps deferred arithmetic on the filtered set" "03,01" 1 "$sel" "$def"

run_selection 3 "02"
expect "a PR-covered plan is not selected even when filtered for" "" 0 "$sel" "$def"

# --- plans_schema_errors -------------------------------------------------------
# Fixtures are jq transforms of one valid plan, so each case exercises
# exactly one rule of the schema.
SCAN="20260913-080000-weekly-ok"
cat > "$TMP/valid.json" <<'EOF'
[
  {
    "id": "01",
    "priority": 1,
    "branch": "fix/security-20260913-080000-weekly-ok-plan-01",
    "plan_file": "mitigation-plans/01-origin-slug.md",
    "files": ["internal/netproxy/foo.go"],
    "tests": ["TestSecurityFoo"],
    "issue_line": "one sanitized sentence",
    "review_criteria": "closes the origin, not the symptom"
  }
]
EOF

expect_clean() {
  local desc=$1 fixture=$2 out
  out="$(plans_schema_errors "$fixture" "$SCAN")"
  if [ -z "$out" ]; then
    echo "ok: $desc"
  else
    fail "$desc: expected no errors, got: $out"
  fi
}

expect_schema_error() {
  local desc=$1 fixture=$2 want=$3 out
  out="$(plans_schema_errors "$fixture" "$SCAN")"
  case "$out" in
    *"$want"*) echo "ok: $desc" ;;
    *) fail "$desc: expected an error containing '$want', got: $out" ;;
  esac
}

broken() { jq "$1" "$TMP/valid.json" > "$TMP/broken.json"; }

expect_clean "a valid manifest passes" "$TMP/valid.json"

echo '{"a": 1}' > "$TMP/broken.json"
expect_schema_error "an object instead of an array is rejected" "$TMP/broken.json" "top level is not a JSON array"

echo '[]' > "$TMP/broken.json"
expect_schema_error "an empty manifest is rejected" "$TMP/broken.json" "no plans"

echo 'not json' > "$TMP/broken.json"
expect_schema_error "invalid JSON is rejected" "$TMP/broken.json" "not valid JSON"

broken '.[0].id = "1"'
expect_schema_error "a one-digit id is rejected" "$TMP/broken.json" "id must be a two-digit string"

broken '.[0].priority = "1"'
expect_schema_error "a string priority is rejected" "$TMP/broken.json" "priority must be a number"

broken '.[0].branch = "fix/security-wrong-generation-plan-01"'
expect_schema_error "a branch without the generation label is rejected" "$TMP/broken.json" "branch must be fix/security"

broken '.[0].files = [".github/workflows/ci.yml"]'
expect_schema_error ".github/ ownership is rejected" "$TMP/broken.json" "never own .github/"

broken '.[0].tests = ["TestFoo"]'
expect_schema_error "a test name outside the security suite is rejected" "$TMP/broken.json" "start with TestSecurity"

broken 'del(.[0].issue_line)'
expect_schema_error "a missing issue line is rejected" "$TMP/broken.json" "issue_line must be a non-empty string"

broken '. + [.[0]]'
expect_schema_error "duplicate plan ids are rejected" "$TMP/broken.json" "duplicate plan ids"

broken '.[0].branch = "fix/security-20260913-080000-weekly-ok-plan-01 extra"'
expect_schema_error "a branch with characters git rejects is rejected" "$TMP/broken.json" "characters git rejects"

broken '.[0].files = ["/etc/passwd"]'
expect_schema_error "an absolute owned path is rejected" "$TMP/broken.json" "repo-relative paths"

broken '.[0].files = ["../outside.go"]'
expect_schema_error "a parent-relative owned path is rejected" "$TMP/broken.json" "repo-relative paths"

broken '.[0].issue_line = "harden the key\u0301 material"'
expect_schema_error "a non-ASCII issue line is rejected" "$TMP/broken.json" "plain ASCII"

# --- sanitize_issue_body -------------------------------------------------------
# The public overview issue must not carry finding text or the vulnerable
# file paths; the body is assembled mechanically, so the gate can also be
# purely mechanical.
cat > "$TMP/vulns.json" <<'EOF'
[
  {
    "id": "VULN-0001",
    "title": "Proxy trusts a spoofable upstream header value",
    "severity": "high",
    "description": "An attacker can send X-Internal-Auth and be trusted",
    "impact": "credential exposure",
    "poc": "curl -H 'X-Internal-Auth: yes' http://target/whoami",
    "endpoint": "/__omac__/whoami (header auto-trust)",
    "technical_analysis": "The admission check reads the header before validating the upstream peer",
    "remediation_steps": "Validate the peer address before trusting the header",
    "code_locations": [
      {
        "file": "internal/netproxy/admission.go",
        "start_line": 42,
        "end_line": 44,
        "snippet": "if req.Header.Get(\"X-Internal-Auth\") != \"\" { return true }",
        "label": "Admission trusts the header unconditionally"
      }
    ]
  }
]
EOF
# Reuse valid.json's plan: it owns internal/netproxy/foo.go.
cat > "$TMP/issue-clean.md" <<'EOF'
- [ ] Network egress admission only trusts the configured upstream boundary
EOF

sanitize_ok() {
  local desc=$1 body=$2
  if sanitize_issue_body "$body" "$TMP/vulns.json" "$TMP/valid.json" >/dev/null 2>&1; then
    echo "ok: $desc"
  else
    fail "$desc: sanitizer rejected a clean body"
  fi
}

sanitize_rejects() {
  local desc=$1 body=$2
  if sanitize_issue_body "$body" "$TMP/vulns.json" "$TMP/valid.json" >/dev/null 2>&1; then
    fail "$desc: sanitizer accepted a leaking body"
  else
    echo "ok: $desc"
  fi
}

sanitize_ok "a clean body passes" "$TMP/issue-clean.md"

# Verbatim finding strings, the way a leak actually happens: the assembler
# copies text out of the scan instead of paraphrasing it.
printf '%s\n' 'the gap lets an attacker can send X-Internal-Auth and be trusted via the proxy' > "$TMP/issue-leak.md"
sanitize_rejects "a description leak is rejected" "$TMP/issue-leak.md"

printf '%s\n' 'the fix ensures the proxy trusts a spoofable upstream header value no longer' > "$TMP/issue-leak.md"
sanitize_rejects "a title leak is rejected" "$TMP/issue-leak.md"

printf '%s\n' "repro: curl -H 'X-Internal-Auth: yes' http://target/whoami still works" > "$TMP/issue-leak.md"
sanitize_rejects "a PoC leak is rejected" "$TMP/issue-leak.md"

printf '%s\n' 'hardens the code in internal/netproxy/foo.go' > "$TMP/issue-leak.md"
sanitize_rejects "a vulnerable file path is rejected" "$TMP/issue-leak.md"

# The scanner's richer fields (code_locations, endpoint, technical_analysis)
# are exactly the schema drift the schema-agnostic forbidden set exists for:
# a new field must not become a leak channel just because no field-name rule
# mentions it.
printf '%s\n' 'the admission check in internal/netproxy/admission.go must change' > "$TMP/issue-leak.md"
sanitize_rejects "a code_locations file path is rejected" "$TMP/issue-leak.md"

printf '%s\n' 'fix the snippet if req.Header.Get("X-Internal-Auth") != "" { return true }' > "$TMP/issue-leak.md"
sanitize_rejects "a code_locations snippet is rejected" "$TMP/issue-leak.md"

printf '%s\n' 'hardens /__omac__/whoami (header auto-trust) so it validates the peer' > "$TMP/issue-leak.md"
sanitize_rejects "an endpoint leak is rejected" "$TMP/issue-leak.md"

printf '%s\n' 'changes how the admission check reads the header before validating the upstream peer' > "$TMP/issue-leak.md"
sanitize_rejects "a technical_analysis leak is rejected" "$TMP/issue-leak.md"

# --- changed_files_within ------------------------------------------------------
# Fixture repo with the fix leg's guard shape: the plan's owned production
# file and any *_security_test.go file are allowed; each case creates exactly
# one violation.
GUARD="$TMP/guard-patterns"
cat > "$GUARD" <<'EOF'
^pkg/prod\.go$
_security_test\.go$
EOF

g="$TMP/guard-repo"
git init -q "$g"
git -C "$g" config user.email "test@example.com"
git -C "$g" config user.name "test"
mkdir -p "$g/pkg"
echo x > "$g/pkg/prod.go"
echo x > "$g/pkg/other.go"
echo x > "$g/pkg/thing_security_test.go"
git -C "$g" add -A
git -C "$g" commit -qm base

guard_clean() {
  local desc=$1 out
  if out="$(changed_files_within "$g" "$GUARD")"; then
    echo "ok: $desc"
  else
    fail "$desc: guard reported '$out', expected it to allow the change"
  fi
}

guard_violation() {
  local desc=$1 want=$2 out
  if out="$(changed_files_within "$g" "$GUARD")"; then
    fail "$desc: guard allowed the change, expected it to report '$want'"
  else
    case "$out" in
      *"$want"*) echo "ok: $desc" ;;
      *) fail "$desc: expected '$want', got '$out'" ;;
    esac
  fi
}

guard_clean "a clean tree passes"

echo y >> "$g/pkg/prod.go"
echo y >> "$g/pkg/thing_security_test.go"
echo y > "$g/pkg/fresh_security_test.go"
guard_clean "the owned file and security test files pass"

echo y >> "$g/pkg/other.go"
guard_violation "a file outside the plan's owned set is reported" "pkg/other.go"
git -C "$g" checkout -q -- pkg/other.go

echo y > "$g/pkg/rogue.go"
guard_violation "an untracked file outside the patterns is reported" "pkg/rogue.go"
rm "$g/pkg/rogue.go"

git -C "$g" mv pkg/other.go pkg/other_renamed.go
guard_violation "a renamed production file is reported by its new path" "other_renamed.go"
git -C "$g" mv pkg/other_renamed.go pkg/other.go

echo y > "$g/pkg/name with space.go"
guard_violation "a quoted untracked path is unquoted before matching" "pkg/name with space.go"
rm "$g/pkg/name with space.go"

# --- assign_waves ---------------------------------------------------------------
# Greedy coloring: plans sharing an owned file must land in different waves,
# each plan gets the lowest free wave, the chain caps at 4 and reports the
# rest as overflow. Fixture: 01 owns a.go+b.go, 02 shares a.go, 03 is
# disjoint, 04 shares b.go, 05 is disjoint; 11-15 all share shared.go.
cat > "$TMP/waves.json" <<'EOF'
[
  { "id": "01", "priority": 1, "files": ["a.go", "b.go"] },
  { "id": "02", "priority": 2, "files": ["a.go"] },
  { "id": "03", "priority": 3, "files": ["c.go"] },
  { "id": "04", "priority": 4, "files": ["b.go", "d.go"] },
  { "id": "05", "priority": 5, "files": ["e.go"] },
  { "id": "11", "priority": 6, "files": ["shared.go"] },
  { "id": "12", "priority": 7, "files": ["shared.go"] },
  { "id": "13", "priority": 8, "files": ["shared.go"] },
  { "id": "14", "priority": 9, "files": ["shared.go"] },
  { "id": "15", "priority": 10, "files": ["shared.go"] }
]
EOF

expect_waves() {
  local desc=$1 ids=$2 want=$3 got
  got="$(assign_waves "$TMP/waves.json" "$ids" | jq -c '.waves')"
  if [ "$got" = "$want" ]; then
    echo "ok: $desc"
  else
    fail "$desc: want waves $want, got $got"
  fi
}

expect_waves "conflicting plans get different waves, disjoint plans share wave 1" \
  "01,02,03,04,05" \
  '{"1":["01","03","05"],"2":["02","04"],"3":[],"4":[]}'

expect_waves "a five-deep conflict chain fills the waves and overflows" \
  "11,12,13,14,15" \
  '{"1":["11"],"2":["12"],"3":["13"],"4":["14"]}'

overflow="$(assign_waves "$TMP/waves.json" "11,12,13,14,15" | jq -c '.overflow')"
if [ "$overflow" = '["15"]' ]; then
  echo "ok: over-cap plans are reported as overflow"
else
  fail "over-cap plans are reported as overflow: got $overflow"
fi

wave_map="$(assign_waves "$TMP/waves.json" "01,02,03" | jq -c '.map')"
if [ "$wave_map" = '{"01":1,"02":2,"03":1}' ]; then
  echo "ok: the wave map carries per-plan wave numbers"
else
  fail "the wave map carries per-plan wave numbers: got $wave_map"
fi

expect_waves "an empty selection colors nothing" \
  "" \
  '{"1":[],"2":[],"3":[],"4":[]}'

# --- last_test_commit / missing_test_names / commit_as ---------------------------
# The fix leg's phase detection and presence check, plus the signed-commit
# identity: a bug here either skips the red proof or attributes work to the
# wrong agent.
h="$TMP/history-repo"
git init -q "$h"
git -C "$h" config user.email "test@example.com"
git -C "$h" config user.name "test"
echo base > "$h/file.txt"
git -C "$h" add -A
git -C "$h" commit -qm "chore: base"
base_sha="$(git -C "$h" rev-parse HEAD)"
echo fix1 > "$h/file.txt"
git -C "$h" commit -qam "fix(security): early fix"
echo tests > "$h/file.txt"
git -C "$h" commit -qam "test(security): the tests"
echo more > "$h/file.txt"
git -C "$h" commit -qam "test(security): extend the tests"
last_want="$(git -C "$h" rev-parse HEAD)"
echo fix2 > "$h/file.txt"
git -C "$h" commit -qam "fix(security): the fix"

if [ "$(last_test_commit "$h" "$base_sha")" = "$last_want" ]; then
  echo "ok: last_test_commit finds the newest test commit after the base"
else
  fail "last_test_commit finds the newest test commit: got $(last_test_commit "$h" "$base_sha")"
fi
if [ -z "$(last_test_commit "$h" "$(git -C "$h" rev-parse HEAD)")" ]; then
  echo "ok: last_test_commit is empty when the range has no test commit"
else
  fail "last_test_commit should be empty for a range without test commits"
fi

mkdir -p "$h/pkg"
printf 'package pkg\n\nfunc TestSecurityAlpha(t *testing.T) {}\n' > "$h/pkg/alpha_security_test.go"
printf 'package pkg\n\nfunc TestSecurityBetaBar(t *testing.T) {}\n' > "$h/pkg/beta_security_test.go"
printf 'TestSecurityAlpha\nTestSecurityBeta\n' > "$TMP/names.txt"
if [ "$(missing_test_names "$h" "$TMP/names.txt" | tr -d '\n')" = "TestSecurityBeta" ]; then
  echo "ok: a prefix name does not count for a longer test name"
else
  fail "missing_test_names: got '$(missing_test_names "$h" "$TMP/names.txt" | tr -d '\n')'"
fi

echo signed > "$h/extra.txt"
git -C "$h" add extra.txt
commit_as "$h" "$TEST_WRITER_NAME" "$TEST_WRITER_EMAIL" "test(security): signed"
if git -C "$h" log -1 --format='%an <%ae>%n%b' | grep -qF "Test Writer Agent <test-writer@security-remediate.invalid>" \
   && git -C "$h" log -1 --format='%b' | grep -qF "Signed-off-by: Test Writer Agent <test-writer@security-remediate.invalid>"; then
  echo "ok: commit_as attributes and signs off with the agent identity"
else
  fail "commit_as: $(git -C "$h" log -1 --format='%an <%ae> | %b')"
fi

# --- run_review_session verdict contract -----------------------------------------
# A missing or internally inconsistent verdict must fail hard; a consistent one
# sets the interface variables. run_session is overridden: no CLI, no network.
ws="$TMP/verdict-ws"
mkdir -p "$ws"
run_session() { :; }

expect_verdict_rejected() {
  local desc=$1 payload=$2
  rm -f "$ws/review-verdict.json"
  [ -n "$payload" ] && printf '%s' "$payload" > "$ws/review-verdict.json"
  if run_review_session "" 1 m "$ws" /dev/null /dev/null "" >/dev/null 2>&1; then
    fail "$desc: run_review_session accepted the verdict"
  else
    echo "ok: $desc"
  fi
}

expect_verdict_rejected "a missing verdict is rejected" ""
expect_verdict_rejected "an inconsistent verdict is rejected" '{"findings":1,"verdict":"approved"}'
expect_verdict_rejected "a negative findings count is rejected" '{"findings":-1,"verdict":"approved"}'

printf '%s' '{"findings":2,"verdict":"insufficient"}' > "$ws/review-verdict.json"
if run_review_session "" 1 m "$ws" /dev/null /dev/null "" >/dev/null 2>&1 \
   && [ "$REVIEW_FINDINGS" = 2 ] && [ "$REVIEW_VERDICT" = insufficient ]; then
  echo "ok: a consistent verdict sets the interface variables"
else
  fail "a consistent verdict sets the interface variables (got ${REVIEW_FINDINGS:-?}/${REVIEW_VERDICT:-?})"
fi

# --- sanitize_diff: the narrowed string set --------------------------------------
# A finding title or PoC text in the diff is rejected, but the public code the
# scanner recorded as a snippet is not: a fix diff legitimately touches it.
printf '%s\n' '+// Proxy trusts a spoofable upstream header value' > "$TMP/diff-gate.txt"
if sanitize_diff "$TMP/diff-gate.txt" "$TMP/vulns.json" >/dev/null 2>&1; then
  fail "a finding title in the diff is rejected"
else
  echo "ok: a finding title in the diff is rejected"
fi

printf '%s\n' '+if req.Header.Get("X-Internal-Auth") != "" { return true }' > "$TMP/diff-gate.txt"
if sanitize_diff "$TMP/diff-gate.txt" "$TMP/vulns.json" >/dev/null 2>&1; then
  echo "ok: a public code snippet in the diff is allowed"
else
  fail "a public code snippet in the diff is allowed"
fi

# --- assert_git_untampered: symlinked hooks --------------------------------------
gd="$TMP/hookrepo"
git init -q "$gd"
git -C "$gd" remote add origin "https://example.invalid/repo.git"
rm -rf "$gd/.git/hooks"
ln -s "$TMP" "$gd/.git/hooks"
if assert_git_untampered "$gd" "https://example.invalid/repo.git" "test" >/dev/null 2>&1; then
  fail "a symlinked hooks directory is detected"
else
  echo "ok: a symlinked hooks directory is detected"
fi
harden_git_dir "$gd"
if assert_git_untampered "$gd" "https://example.invalid/repo.git" "test" >/dev/null 2>&1; then
  echo "ok: a hardened git dir passes the tamper check"
else
  fail "a hardened git dir passes the tamper check"
fi

# --- session_budget -------------------------------------------------------------
# Writers get budget_minutes; the four short sessions split what the leg
# ceiling leaves after the writers, the slack and the PR writer, capped by the
# writer budget and floored at ten minutes. The arithmetic was corrected twice
# once, so pin it.
export BUDGET_MINUTES=60
b_writer="$(session_budget writer)"; b_short="$(session_budget short)"
if [ "$b_writer" = 60 ] && [ "$b_short" = 47 ]; then
  echo "ok: default budgets are writers 60, short 47 (350-120-30-10 over four)"
else
  fail "default session budgets: writer=$b_writer short=$b_short"
fi
export BUDGET_MINUTES=10
b_short="$(session_budget short)"
if [ "$(session_budget writer)" = 10 ] && [ "$b_short" = 10 ]; then
  echo "ok: the ten-minute floor holds for an undersized budget"
else
  fail "undersized-budget floor: writer=$(session_budget writer) short=$b_short"
fi
export BUDGET_MINUTES=100
b_short="$(session_budget short)"
if [ "$(session_budget writer)" = 100 ] && [ "$b_short" = 27 ]; then
  echo "ok: a large writer budget still caps the short sessions at the ceiling maths"
else
  fail "large-budget short cap: writer=$(session_budget writer) short=$b_short"
fi
unset BUDGET_MINUTES

# --- session_sandbox_args -------------------------------------------------------
# The sandbox mounts the whole filesystem read-only, keeps /tmp and the
# session workdir writable, and pins every .git in reach read-only.
sb="$TMP/sb"
mkdir -p "$sb/repo/.git" "$sb/archive/.git"
sb_args="$(session_sandbox_args "$sb" | paste -sd' ' -)"
case "$sb_args" in
  *"--ro-bind / / --bind /tmp /tmp"*"--bind $sb $sb"*"--ro-bind $sb/repo/.git $sb/repo/.git"*"--ro-bind $sb/archive/.git $sb/archive/.git"*)
    echo "ok: the session sandbox makes the fs read-only, the workdir writable and .git read-only" ;;
  *) fail "session_sandbox_args: $sb_args" ;;
esac

# --- session_home ---------------------------------------------------------------
# The generated gateway config must be valid JSON and register both the
# implementer and a distinct reviewer model.
sh="$TMP/sh"
if SKAINET_TOKEN=sim SKAINET_INTERNAL="http://sim" session_home "$sh" "model-a" "model-b" >/dev/null \
   && jq -e '.provider.model.models["model-a"] and .provider.model.models["model-b"]' \
        "$sh/driver-home/.config/opencode/opencode.json" >/dev/null 2>&1; then
  echo "ok: session_home registers both models in valid JSON"
else
  fail "session_home registers both models in valid JSON"
fi

if [ "$failures" -gt 0 ]; then
  echo "$failures plans test(s) failed" >&2
  exit 1
fi
echo "plans: all cases passed"
