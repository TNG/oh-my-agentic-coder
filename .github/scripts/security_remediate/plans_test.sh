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
    "poc": "curl -H 'X-Internal-Auth: yes' http://target/whoami"
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

# --- changed_files_within ------------------------------------------------------
# Fixture repo with one allowed pair (a _test.go file and the pin file) and
# one production file, so each case creates exactly one violation.
GUARD="$TMP/guard-patterns"
cat > "$GUARD" <<'EOF'
_test\.go$
^scripts/security-suite-expected-failures\.txt$
EOF

g="$TMP/guard-repo"
git init -q "$g"
git -C "$g" config user.email "test@example.com"
git -C "$g" config user.name "test"
mkdir -p "$g/pkg" "$g/scripts"
echo x > "$g/pkg/old_test.go"
echo x > "$g/pkg/prod.go"
echo x > "$g/scripts/security-suite-expected-failures.txt"
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

echo y >> "$g/pkg/old_test.go"
echo y > "$g/pkg/fresh_test.go"
echo y >> "$g/scripts/security-suite-expected-failures.txt"
guard_clean "modified and new test files and the pin file pass"

echo y >> "$g/pkg/prod.go"
guard_violation "a modified production file is reported" "pkg/prod.go"
git -C "$g" checkout -q -- pkg/prod.go

echo y > "$g/pkg/rogue.go"
guard_violation "an untracked file outside the patterns is reported" "pkg/rogue.go"
rm "$g/pkg/rogue.go"

git -C "$g" mv pkg/prod.go pkg/prod_renamed.go
guard_violation "a renamed production file is reported by its new path" "prod_renamed.go"
git -C "$g" mv pkg/prod_renamed.go pkg/prod.go

echo y > "$g/pkg/name with space.go"
guard_violation "a quoted untracked path is unquoted before matching" "pkg/name with space.go"
rm "$g/pkg/name with space.go"

if [ "$failures" -gt 0 ]; then
  echo "$failures plans test(s) failed" >&2
  exit 1
fi
echo "plans: all cases passed"
