#!/usr/bin/env bash
#
# lib.sh — plumbing shared by the security-remediate stage drivers
# (.github/scripts/security_remediate/*.sh). Sourced, never executed.
#
# Five concerns live here so each stage driver stays small:
#   - probing PAT write access (security-scan.yml's git-receive-pack probe,
#     applied to both repos this pipeline writes to)
#   - git against the private archive repo without ever leaking the PAT into
#     the public job log (git error output embeds the credential URL)
#   - locating the newest scan in the archive and its completeness status
#   - the budget selection: plans.json + PR existence + max_plans + id filter
#   - the opencode session plumbing from scripts/doc-drift.sh: throwaway
#     HOME, gateway config, --auto --pure, wall-clock watchdog
#
# Environment (set by the workflow jobs, or by hand for local runs):
#   GITHUB_REPOSITORY  this repo, owner/name — `gh pr list` target
#   GH_TOKEN           token `gh` authenticates with. The workflow's
#                     read-scoped github.token is enough; the PAT never
#                     reaches gh, let alone an agent session.
#   SECURITY_SCAN_PAT  archive write PAT (probe, clone, push)
#   REPO_TOKEN         this repo's write token (github.token in CI; branch push)
#   ARCHIVE_REPO       private companion repo, owner/name
#   SKAINET_TOKEN      model gateway API key (agent sessions)
#   SKAINET_INTERNAL   model gateway base URL (agent sessions)

# Keep in sync with internal/e2e/versions.go's "opencode" pin, same as
# scripts/doc-drift.sh.
DEFAULT_OPENCODE_VERSION="opencode-ai@1.17.12"
DEFAULT_CONTEXT_LIMIT="100000"
DEFAULT_OUTPUT_LIMIT="32000"

# Commit identities. Every commit this pipeline makes is signed off (-s), and
# the Signed-off-by trailer carries whichever agent wrote the content, so the
# history attributes tests and fixes to their author. The .invalid domain is
# the RFC-reserved placeholder domain — these are not real mailboxes.
# Interface variables for the stage drivers, not used in lib.sh itself.
export TEST_WRITER_NAME="Test Writer Agent"
export TEST_WRITER_EMAIL="test-writer@security-remediate.invalid"
export FIX_WRITER_NAME="Fix Writer Agent"
export FIX_WRITER_EMAIL="fix-writer@security-remediate.invalid"
export RUNNER_NAME="Security Remediate Runner"
export RUNNER_EMAIL="runner@security-remediate.invalid"

# Session budgets. The first test writer and the first fix writer carry the
# work and get the full budget_minutes; the four shorter sessions (two
# reviewers and the two retry writers) share what the leg ceiling leaves once
# the two writers, the slack and the PR writer are accounted for, capped by
# budget_minutes; the PR writer gets a fixed short budget. Everything floors
# at 10 minutes so an undersized budget cannot produce a zero-second session.
LEG_CEILING_MINUTES=350
LEG_SLACK_MINUTES=30
PR_WRITER_MINUTES=10
BUDGET_FLOOR_MINUTES=10

# session_budget writer|short — minutes for one session of that class.
session_budget() {
  local style=$1 budget=${BUDGET_MINUTES:-60} short
  case "$budget" in ''|*[!0-9]*) budget=60 ;; esac
  [ "$budget" -lt "$BUDGET_FLOOR_MINUTES" ] && budget="$BUDGET_FLOOR_MINUTES"
  case "$style" in
    short)
      short=$(( (LEG_CEILING_MINUTES - 2 * budget - LEG_SLACK_MINUTES - PR_WRITER_MINUTES) / 4 ))
      [ "$short" -gt "$budget" ] && short="$budget"
      [ "$short" -lt "$BUDGET_FLOOR_MINUTES" ] && short="$BUDGET_FLOOR_MINUTES"
      echo "$short" ;;
    *) echo "$budget" ;;
  esac
}

# Portable stand-in for GNU coreutils `timeout` (stock macOS has none).
# Same implementation as scripts/doc-drift.sh: background the command, race a
# sleep+kill watchdog against it, return the command's status (143/SIGTERM
# when the watchdog fired).
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

# Append "$1=$2" to $GITHUB_OUTPUT under Actions, print to stdout otherwise,
# so the drivers stay runnable by hand.
emit_output() {
  local name=$1 value=$2
  if [ -n "${GITHUB_OUTPUT:-}" ]; then
    printf '%s=%s\n' "$name" "$value" >> "$GITHUB_OUTPUT"
  else
    printf '%s=%s\n' "$name" "$value"
  fi
}

# Fail fast when a driver is invoked somewhere the toolbox is missing; a
# half-equipped host turns into a confusing failure three steps later.
require_tools() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || {
      echo "::error title=Missing tool::'$tool' is required by this stage but not installed."
      return 1
    }
  done
}

# GitHub authorizes the ref advertisement, so asking for git-receive-pack is
# a read-only probe of WRITE access: 200 writable, 403 valid but read-only,
# 401 expired/malformed, 404 invisible. Same probe as security-scan.yml's
# preflight, factored out because remediation writes to two repos.
#
# $1 = repo (owner/name), $2 = PAT, $3 = what to call it in error messages.
probe_write_access() {
  local repo=$1 pat=$2 what=$3 code
  code=$(curl -sS -o /dev/null -w '%{http_code}' \
    -u "x-access-token:${pat}" \
    "https://github.com/${repo}.git/info/refs?service=git-receive-pack" || echo 000)
  case "$code" in
    200)
      echo "${what} is writable"
      ;;
    403)
      echo "::error title=${what} is read-only::The PAT can read ${repo} but not write it. Grant Contents: read and write on ${repo} (a fine-grained PAT must also list the repo explicitly), then re-run."
      return 1
      ;;
    401)
      echo "::error title=${what} credential rejected::The PAT was refused by GitHub (401) — it is expired or malformed. Rotate it and re-run."
      return 1
      ;;
    404)
      echo "::error title=${what} not reachable::The PAT cannot see ${repo} (404). Either the repo name is wrong or the token has no access to it."
      return 1
      ;;
    *)
      echo "::error title=${what} probe inconclusive::HTTP ${code}. Not spending an agent session on a credential that may not deliver its results."
      return 1
      ;;
  esac
}

# git's error output embeds the credential URL, so it never reaches the job
# log unredacted: capture, strip the PAT, print the remainder on failure only.
archive_git() {
  local dir=$1; shift
  local err status=0 s
  local -a redact sedargs
  redact=("s/${SECURITY_SCAN_PAT}/REDACTED-TOKEN/g")
  # A failed push prints the credential URL; both tokens can appear now (the
  # archive PAT and the repo's github.token), so redact either. One -e per
  # pattern: a bare `sed "${redact[@]}"` treats the second pattern as a file.
  if [ -n "${REPO_TOKEN:-}" ] && [ "$REPO_TOKEN" != "$SECURITY_SCAN_PAT" ]; then
    redact+=("s/${REPO_TOKEN}/REDACTED-TOKEN/g")
  fi
  for s in "${redact[@]}"; do sedargs+=(-e "$s"); done
  err=$(git -C "$dir" "$@" 2>&1) || status=$?
  if [ "$status" -ne 0 ]; then
    printf '%s\n' "$err" | sed "${sedargs[@]}" >&2
    return "$status"
  fi
  [ -z "$err" ] || printf '%s\n' "$err" >&2
}

# The ownership guard reads `git status --porcelain`, which never shows .git
# internals. A session can therefore plant hook files, set core.hooksPath or
# core.fsmonitor, or an url.*.insteadOf rewrite there, and a later runner
# commit/push would execute or follow them with the PAT in scope. Every
# clone/checkout is stripped of hooks up front, and assert_git_untampered
# verifies the directory is still exactly as the runner left it before the
# runner commits or pushes from it.
harden_git_dir() {
  rm -rf "$1/.git/hooks"
  mkdir -p "$1/.git/hooks"
}

assert_git_untampered() {
  local dir=$1 expected_origin=$2 what=$3 problem=""
  # -L: a symlinked hooks dir would hide planted hooks from `find -type f`.
  if [ -L "$dir/.git/hooks" ] || [ -n "$(find "$dir/.git/hooks" -type f 2>/dev/null)" ]; then
    problem="hook files were planted"
  fi
  [ -z "$(git -C "$dir" config --local --get core.hooksPath 2>/dev/null || true)" ] \
    || problem="${problem:+$problem; }core.hooksPath was set"
  [ -z "$(git -C "$dir" config --local --get core.fsmonitor 2>/dev/null || true)" ] \
    || problem="${problem:+$problem; }core.fsmonitor was set"
  [ -z "$(git -C "$dir" config --local --get-regexp '^url\..*\.insteadof$' 2>/dev/null || true)" ] \
    || problem="${problem:+$problem; }an url rewrite was planted"
  if [ "$(git -C "$dir" remote get-url origin 2>/dev/null || true)" != "$expected_origin" ]; then
    problem="${problem:+$problem; }origin was moved"
  fi
  if [ -n "$problem" ]; then
    echo "::error title=${what} git directory tampered::${problem}. The runner refuses to commit or push from a directory a session could write to; nothing was pushed."
    return 1
  fi
  return 0
}

# Shallow-clone the private archive repo and give the clone a commit identity,
# so stages only ever add content and push. The token is only needed to clone:
# leaving it in .git/config would put the PAT on disk inside every session's
# workspace, so the origin URL is scrubbed immediately and pushes re-supply
# the credential per invocation, from the runner shell only.
clone_archive() {
  local dest=$1 repo=$2 pat=$3
  archive_git . clone --quiet --depth 1 \
    "https://x-access-token:${pat}@github.com/${repo}.git" "$dest" || {
    echo "::error title=Archive clone failed::Could not clone the private archive repo (details above are redacted — they embed the credential URL)."
    return 1
  }
  git -C "$dest" remote set-url origin "https://github.com/${repo}.git"
  harden_git_dir "$dest"
  git -C "$dest" config user.name "$RUNNER_NAME"
  git -C "$dest" config user.email "$RUNNER_EMAIL"
}

# Commit and push ONLY the paths the runner produced ($3...). A session can
# write anywhere in the archive clone; the runner stages nothing else, so
# planted files and edits outside the intended paths never leave the runner.
# Nothing to commit is success: re-dispatching a stage that already delivered
# is a no-op, which is how the pipeline's idempotence works. Commits skip
# hooks, the .git directory is verified untouched, and the credential is
# supplied per invocation rather than living on disk.
push_archive() {
  local dir=$1 message=$2 p; shift 2
  assert_git_untampered "$dir" "https://github.com/${ARCHIVE_REPO}.git" "Archive repo" || return 1
  for p in "$@"; do
    [ -e "$dir/$p" ] && git -C "$dir" add -A -- "$p"
  done
  if git -C "$dir" diff --cached --quiet; then
    echo "archive: nothing to push"
    return 0
  fi
  git -C "$dir" commit -s --no-verify --quiet -m "$message"
  # Parallel wave legs share the archive clone, so the loser of a concurrent
  # push gets a non-fast-forward rejection. The paths are disjoint per stage,
  # so rebasing onto the new tip is conflict-free — retry before failing.
  local attempt
  for attempt in 1 2 3; do
    if archive_git "$dir" push --quiet --no-verify \
         "https://x-access-token:${SECURITY_SCAN_PAT}@github.com/${ARCHIVE_REPO}.git" HEAD; then
      echo "archive: pushed"
      return 0
    fi
    if [ "$attempt" -lt 3 ]; then
      echo "archive: push rejected (likely a parallel leg) — rebasing onto the new tip and retrying ($attempt/3)" >&2
      # Fetch by explicit credentialed URL: origin is the clean URL, and the
      # archive is private, so an unauthenticated fetch would fail and the
      # rebase would never happen.
      archive_git "$dir" fetch --quiet \
        "https://x-access-token:${SECURITY_SCAN_PAT}@github.com/${ARCHIVE_REPO}.git" HEAD >&2 || true
      archive_git "$dir" rebase --quiet FETCH_HEAD >&2 \
        || archive_git "$dir" rebase --abort >&2 || true
    fi
  done
  echo "::error title=Archive push failed::Pushing the archive repo failed after retries — check for a protected default branch or a token revoked mid-run."
  return 1
}

# Scan dirs are named "<label>-<reason>-<status>" and the label is always
# UTC %Y%m%d-%H%M%S (security-scan.yml mints it that way), so lexical order
# is chronological order and `sort -r` puts the newest first.
newest_scan_dir() {
  local archive_dir=$1
  ls -1 "${archive_dir}/scans" 2>/dev/null | LC_ALL=C sort -r | head -1 || true
}

# The status is the trailing segment of the scan dir name, so the suffix —
# not the middle — is authoritative even when the reason contains dashes.
scan_dir_status() {
  case "$1" in
    *-ok)      echo ok ;;
    *-PARTIAL) echo PARTIAL ;;
    *-FAILED)  echo FAILED ;;
    *)         echo unknown ;;
  esac
}

# Branches that already have an open or merged pull request. A closed-
# without-merge PR means the leg failed and the plan stays eligible.
pr_covered_branches() {
  gh pr list -R "$GITHUB_REPOSITORY" --state all --limit 5000 \
    --json headRefName,state,mergedAt \
    --jq '.[] | select(.state == "OPEN" or .mergedAt != null) | .headRefName'
}

# The budget selection from "Budget control" in the pipeline plan:
#   1. drop plans whose branch has an open or merged PR (done or in flight,
#      consuming no budget),
#   2. intersect with the operator's id filter when one is given,
#   3. take the first max_plans in (priority, id) order.
# Prints two lines: the selected ids comma-joined (possibly empty), then the
# deferred count (eligible minus selected, where eligible is the list after
# steps 1 and 2). Two lines, not "ids count" on one: an empty selection would
# otherwise shift into the count field on read.
#
# $1 = plans.json path, $2 = max_plans (number), $3 = filter ("" or comma ids)
select_plans() {
  local plans_json=$1 max=$2 filter=$3 covered
  covered="$(pr_covered_branches || true)"
  jq -nr --slurpfile plans "$plans_json" \
       --arg covered "$covered" --arg filter "$filter" --argjson max "$max" '
    ($plans[0] | sort_by(.priority, .id)) as $sorted
    | ($covered | split("\n") | map(select(length > 0))) as $c
    | ($filter | if length > 0 then split(",") else [] end) as $f
    | ($sorted
       | map(select(.branch as $b | ($c | index($b)) | not))
       | (if ($f | length) > 0 then map(select(.id as $i | ($f | index($i)))) else . end)
      ) as $eligible
    | ($eligible | .[0:$max]) as $selected
    | ($selected | map(.id) | join(",")),
      (($eligible | length) - ($selected | length))
  '
}

# The manifest schema every later stage depends on: branch names carry the
# generation label, owned files never include .github/ (CI-config findings
# are manual follow-up, which keeps the PAT free of the workflows
# permission), test names are TestSecurity* because the promoted security
# suite names them that way. Prints one "<id>: <problem>" line per violation; no output means
# the manifest is schema-valid. Path existence is the caller's job — jq
# cannot stat.
plans_schema_errors() {
  local plans_json=$1 scan_dir=$2
  if ! jq -e . "$plans_json" >/dev/null 2>&1; then
    echo "-: plans.json is missing or not valid JSON"
    return 0
  fi
  jq -r --arg scan "$scan_dir" '
    def badstr($v): ($v // "") | type != "string" or length == 0;
    (if type != "array" then ["-: top level is not a JSON array"]
     elif length == 0 then ["-: no plans"]
     else
       ([ .[] | . as $e | ($e.id // "-") as $id |
          (if (($e.id // "") | test("^[0-9]{2}$") | not) then "\($id): id must be a two-digit string (e.g. 01)" else empty end),
          (if (($e.priority // 0) | type) != "number" or ($e.priority // 0) < 1 then "\($id): priority must be a number >= 1" else empty end),
          (if (($e.branch // "") | type) != "string" or ($e.branch // "") != ("fix/security-\($scan)-plan-\($e.id // "?")") then "\($id): branch must be fix/security-\($scan)-plan-<id>" else empty end),
          (if (($e.branch // "") | type) == "string" and (($e.branch // "") | test("^[A-Za-z0-9._/-]+$") | not) then "\($id): branch contains characters git rejects (the scan dir name may contain spaces or parentheses)" else empty end),
          (if (($e.files // []) | type) != "array" or ($e.files // [] | length) == 0 then "\($id): files must be a non-empty array" else empty end),
          (if (($e.files // []) | any(. as $x | ($x | type) != "string")) then "\($id): files entries must be strings" else empty end),
          (if (($e.files // []) | map(select(type == "string")) | any(startswith(".github/"))) then "\($id): plans never own .github/ files — CI-config findings are manual follow-up" else empty end),
          (if (($e.files // []) | map(select(type == "string")) | any(startswith("/") or startswith("../") or contains("/../"))) then "\($id): files must be repo-relative paths" else empty end),
          (if (($e.tests // []) | type) != "array" or ($e.tests // [] | length) == 0 then "\($id): tests must be a non-empty array" else empty end),
          (if (($e.tests // []) | any(. as $x | ($x | type) != "string")) then "\($id): tests entries must be strings" else empty end),
          (if (($e.tests // []) | map(select(type == "string")) | any(test("^TestSecurity") | not)) then "\($id): test names must start with TestSecurity" else empty end),
          (if badstr($e.issue_line) then "\($id): issue_line must be a non-empty string" else empty end),
          (if (($e.issue_line // "") | type) == "string" and (($e.issue_line // "") | test("[^ -~]")) then "\($id): issue_line must be a single line of plain ASCII" else empty end),
          (if badstr($e.review_criteria) then "\($id): review_criteria must be a non-empty string" else empty end),
          (if badstr($e.plan_file) then "\($id): plan_file must be a non-empty string" else empty end)
        ])
       + (if ([ .[] | (.id // "") ] | length) > ([ .[] | (.id // "") ] | unique | length) then ["-: duplicate plan ids"] else [] end)
     end) | .[]
  ' "$plans_json"
}

# Strings that must never appear verbatim in the public overview issue: every
# string the scanner emitted for a finding (titles, descriptions, impacts,
# PoCs, code locations with their snippets, endpoints, technical analyses —
# whatever the schema grows next) plus the plans' owned file paths (they
# point straight at the vulnerable code). Deliberately schema-agnostic:
# strix has added fields between generations (code_locations, endpoint) and
# an allowlist of field names silently stops covering what it has not seen.
# Used by the sanitizer below; short values are skipped there to keep false
# positives down.
issue_body_forbidden_strings() {
  local vulns_json=$1 plans_json=$2
  {
    jq -r '[.. | strings] | .[]' "$vulns_json" 2>/dev/null || true
    jq -r '.[].files[]?' "$plans_json" 2>/dev/null || true
  }
}

# The disclosure gate for the public overview issue: the assembled body is
# checked against every forbidden string before anything is posted. Case
# and whitespace insensitive, line-by-line: a multi-line finding text is
# checked per line, so a leak is caught even in fragments. Prints nothing
# and returns 0 when clean; prints a withholding reason and returns 1 on a
# match — never echo the matched string, this output can reach the public
# job log.
#
# The same per-line check guards the pushed diff, with a narrower string set
# and a higher length floor (see diff_forbidden_strings).
sanitize_lines() {
  local body_file=$1 min_len=${2:-8} s
  while IFS= read -r s; do
    # Trim the ends only — multi-word finding text must keep its interior
    # whitespace to stay findable in the body.
    s="${s#"${s%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    [ "${#s}" -ge "$min_len" ] || continue
    if grep -Fqi -- "$s" "$body_file"; then
      echo "text contains a string from the scan findings (string withheld)"
      return 1
    fi
  done
}

sanitize_issue_body() {
  sanitize_lines "$1" < <(issue_body_forbidden_strings "$2" "$3")
}

# Strings the pushed diff must not contain: the finding TITLES and the PoC's
# prose fields, but not code locations, snippets or poc_script_code. A fix
# diff and the tests it adds are Go code, and every scanner PoC block is
# ordinary Go/HTTP test scaffolding: matching those line-by-line fired on
# boilerplate (`t.Setenv(...)`, error checks) in both test and production
# files. The prose fields are the exploit-specific strings that must not be
# pasted into a public test or comment.
diff_forbidden_strings() {
  local vulns_json=$1
  {
    jq -r '.[] | (.title // empty)' "$vulns_json" 2>/dev/null || true
    jq -r '.[] | to_entries[]
            | select((.key | test("poc|exploit|proof"; "i"))
                     and (.key | test("(^|[_-])(code|script)([_-]|$)"; "i") | not)
                     and (.value | type == "string"))
            | .value' "$vulns_json" 2>/dev/null || true
  }
}

# The diff's length floor is higher than the issue body's: prose sentences are
# long, so a 20-character minimum keeps short boilerplate from ever matching.
sanitize_diff() {
  sanitize_lines "$1" 20 < <(diff_forbidden_strings "$2")
}

# The forbidden strings that actually appear in $1, one per line — for the
# private repair prompt. Finding text, so this output must never reach a
# public surface. Always returns 0; callers judge by the output.
diff_disclosure_hits() {
  local diff_file=$1 vulns_json=$2 s
  while IFS= read -r s; do
    s="${s#"${s%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    [ "${#s}" -ge 20 ] || continue
    grep -Fqi -- "$s" "$diff_file" && printf '%s\n' "$s"
  done < <(diff_forbidden_strings "$vulns_json")
  return 0
}

# Number of comma-separated ids in $1 (empty input is zero).
id_count() {
  [ -z "$1" ] && { echo 0; return; }
  printf '%s' "$1" | awk -F, '{print NF}'
}

# The per-plan pull-request state, the single source of truth for "executed":
# a plan is done when its PR is merged, in flight when the PR is open, and
# still queued otherwise (no PR — a leg that failed pushed its branch but no
# PR, so it stays queued and is retried on a later run).
# Prints three lines: "merged:<ids>", "open:<ids>", "unstarted:<ids>".
plan_pr_states() {
  local plans_json=$1 merged="" open="" unstarted="" entry id branch state
  while IFS= read -r entry; do
    id="$(printf '%s' "$entry" | jq -r '.id')"
    branch="$(printf '%s' "$entry" | jq -r '.branch')"
    state="$(gh pr list -R "$GITHUB_REPOSITORY" --head "$branch" --state all \
      --limit 1 --json state --jq '.[0].state' 2>/dev/null || true)"
    case "$state" in
      MERGED) merged="$merged$id," ;;
      OPEN)   open="$open$id," ;;
      *)      unstarted="$unstarted$id," ;;
    esac
  done < <(jq -c '.[]' "$plans_json")
  printf 'merged:%s\nopen:%s\nunstarted:%s\n' "${merged%,}" "${open%,}" "${unstarted%,}"
}

# The overview issue body, shared by the issue stage (which creates or
# rewrites it) and the finalize workflow (which refreshes it daily), so the
# two writers cannot drift apart. Ticks derive from the merged-id list, which
# is why no state is carried in the old body.
# Plan ids whose generation branch already exists on origin: a leg pushes its
# branch on success (a pull request follows) or on failure (a wip push), so a
# branch means "dispatched at least once". One ls-remote covers the whole
# generation, which is why no state has to be carried between runs.
branch_dispatched_ids() {
  local repo=$1 scan_dir=$2 prefix="refs/heads/fix/security-${scan_dir}-plan-"
  git -C "$repo" ls-remote --heads origin "${prefix}*" 2>/dev/null \
    | awk -v p="$prefix" '{ i = index($2, p); if (i) print substr($2, i + length(p)) }' \
    | sort -u | paste -sd, -
}

# The "in flight" bucket: dispatched at least once but no pull request yet.
# $1 branch-derived ids, $2 extra ids (this run's selection), $3 merged ids,
# $4 open ids. Disjoint from merged/open by construction, so the four status
# buckets sum to the total.
dispatched_bucket() {
  local id out=""
  for id in $(printf '%s,%s' "$1" "$2" | tr ',' ' '); do
    [ -n "$id" ] || continue
    case ",$3,$4," in *",$id,"*) continue ;; esac
    case ",$out," in *",$id,"*) continue ;; esac
    out="$out$id,"
  done
  printf '%s' "${out%,}"
}

# Prints the body; $1 plans.json, $2/$3/$4 the merged/open/in-flight id lists.
overview_body() {
  local plans_json=$1 merged=$2 open=$3 dispatched=$4
  local total merged_n open_n dispatched_n not_dispatched_n
  total="$(jq 'length' "$plans_json")"
  merged_n="$(id_count "$merged")"
  open_n="$(id_count "$open")"
  dispatched_n="$(id_count "$dispatched")"
  not_dispatched_n=$(( total - merged_n - open_n - dispatched_n ))

  printf '%s\n' '<!-- security-remediation: overview -->'
  printf '\n'
  printf '%s\n' '> **Note:** this issue is maintained automatically by the security'
  printf '%s\n' '> remediation pipeline. It is rewritten by the pipeline on every run and'
  printf '%s\n' '> updated by the daily finalize workflow, so please do not edit it by hand.'
  printf '\n'
  printf '%s\n' 'The security remediation pipeline tracks its workstreams here. Each item is'
  printf '%s\n' 'one origin-batched fix plan, executed by an automated pipeline whose pull'
  printf '%s\n' 'requests reference this issue. Detailed plans live in the private companion'
  printf '%s\n' 'repo; this issue stays sanitized by design.'
  printf '\n'
  printf '%s\n' '## Status'
  printf '\n'
  printf -- '- Workstreams: %s total — %s merged, %s in review, %s in flight, %s not yet dispatched.\n' \
    "$total" "$merged_n" "$open_n" "$dispatched_n" "$not_dispatched_n"
  printf '\n'
  printf '%s\n' '## Workstreams'
  printf '\n'
  while IFS=$'\t' read -r id line; do
    local state=' '
    case ",$merged," in *",$id,"*) state='x' ;; esac
    printf -- '- [%s] %s\n' "$state" "$line"
  done < <(jq -r 'sort_by(.priority, .id) | .[] | "\(.id)\t\(.issue_line)"' "$plans_json")
  printf '\n'
  printf '%s\n' '## Notes'
  printf '\n'
  printf '%s\n' '- Merging stays a human action: every pull request needs at least one approval (COLLABORATION.md).'
  printf '%s\n' '- Each run starts up to max_plans not-yet-dispatched workstreams in priority order; one whose leg fails stays queued and is retried on a later run.'
}

# Wave assignment for the fix stage ("Job 5" in the pipeline plan): greedy
# coloring over the selected plans. Plans are placed in (priority, id) order;
# each gets the lowest wave in which it shares no owned file with a plan
# already placed there, capped at 4 waves — enough for the default
# max_plans of 3 and one conflict chain on top. Over-cap plans are reported
# as overflow: they stay eligible (no PR) and the next run picks them up.
#
# Prints one JSON object:
#   {"waves": {"1": ["01","03"], "2": ["02"]},
#    "map": {"01": 1, "02": 2, "03": 1},
#    "overflow": ["04"]}
assign_waves() {
  local plans_json=$1 ids=$2
  jq -cn --slurpfile plans "$plans_json" --arg ids "$ids" '
    ($ids | split(",") | map(select(length > 0))) as $want
    | ($plans[0] | sort_by(.priority, .id)
                   | map(select(.id as $i | ($want | index($i))))) as $sel
    | reduce $sel[] as $p (
        {placed: [], overflow: []};
        ([ .placed[]
           | select(. as $a | any($p.files[]; . as $f | ($a.files | index($f)) != null))
           | .wave ]) as $taken
        | ([range(1; 5)] - $taken | min) as $w
        | if $w == null then .overflow += [$p.id]
          else .placed += [{id: $p.id, wave: $w, files: $p.files}]
          end)
    | { placed: [.placed[] | select(true)] } as $placed
    | { waves: (reduce range(1; 5) as $w ({};
                 . + {($w | tostring): [$placed.placed[] | select(.wave == $w) | .id]}))
      , map: ($placed.placed | map({key: .id, value: .wave}) | from_entries)
      , overflow: .overflow }
  '
}

# Ownership guard, the mechanical enforcement of the conflict matrix: every
# changed path in the checkout (staged, unstaged or untracked) must match one
# of the grep -E patterns in $2, or the guard prints the offending path and
# returns 1. Violations are paths in THIS public repo, so printing them is
# fine; the sensitive set is the allowed paths from the plan, and those are
# never printed.
changed_files_within() {
  local repo=$1 patterns=$2 path violations=0
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    path="${path:3}"             # porcelain v1: XY<TAB>path
    case "$path" in
      *" -> "*) path="${path#* -> }" ;;  # renames: R  orig -> path
    esac
    path="${path#\"}"
    path="${path%\"}"
    [ -n "$path" ] || continue
    if ! printf '%s\n' "$path" | grep -Eqf "$patterns"; then
      # Print every violation, not only the first: the stage names them in the
      # failure message so an under-specified plan is diagnosable at a glance.
      echo "$path"
      violations=1
    fi
  done < <(git -C "$repo" status --porcelain)
  return "$violations"
}

# Paths changed across a commit range ($2, e.g. base..HEAD), one per line —
# the cumulative view a re-entered branch needs, where the working tree alone
# would only show the latest session's edits.
changed_paths_between() {
  git -C "$1" diff --name-only "$2"
}

# Undo every edit the ownership guard reports for a hard-leashed session,
# given the guard's printed list: tracked modifications and deletions are
# restored from HEAD, staged additions and untracked files are deleted, and
# a staged rename is moved back. The removed diff is written to $3 first —
# untracked files are captured via intent-to-add before they are deleted and
# a rename's source path joins the pathspec so the pair stays visible — so
# the fix session under the soft leash can judge what was taken away and
# reapply what it needs.
revert_offending_edits() {
  local repo=$1 paths=$2 report=$3 path old xy
  local -a specs=()
  # A pathspec limited to one side of a staged rename (old -> new) reports the
  # new path as a plain add, so renames are resolved against the full index
  # diff; the capture needs both sides in its pathspec anyway.
  rename_source() {
    git -C "$repo" diff --cached -M --name-status \
      | awk -F'\t' -v new="$path" '$3 == new { print $2; exit }'
  }
  : > "$report"
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    if [ "$(git -C "$repo" status --porcelain -- "$path" | head -n1 | cut -c1-2)" = "??" ]; then
      # Intent-to-add: otherwise the capture below shows nothing for it.
      git -C "$repo" add -N -- "$path" 2>/dev/null || true
    fi
    old="$(rename_source)"
    [ -n "$old" ] && specs+=("$old")
    specs+=("$path")
  done <<< "$paths"
  git -C "$repo" diff HEAD -M -- "${specs[@]}" >> "$report"
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    old="$(rename_source)"
    if [ -n "$old" ]; then
      git -C "$repo" mv -f "$path" "$old"
      continue
    fi
    xy="$(git -C "$repo" status --porcelain -- "$path" | head -n1 | cut -c1-2)"
    case "$xy" in
      'A'*)
        git -C "$repo" rm -f -q -- "$path" ;;
      'M '*|' M'*|'MM'*|'D '*|' D'*)
        git -C "$repo" checkout -q HEAD -- "$path" ;;
      *)
        # Plain untracked or intent-to-add: drop the index entry with the file.
        rm -f "$repo/$path"
        git -C "$repo" rm -q --cached -- "$path" 2>/dev/null || true ;;
    esac
  done <<< "$paths"
}

# Install the pinned opencode CLI the sessions run with.
install_opencode() {
  bun install -g "${E2E_VERSION_OPENCODE:-$DEFAULT_OPENCODE_VERSION}"
}

# Fresh HOME for a session's opencode CLI so it never picks up the runner's
# real omac/opencode state, holding only the generated gateway config — the
# same isolation scripts/doc-drift.sh applies. $2 is the implementing model,
# $3 an optional reviewer model: both must be registered, because a review
# session runs against the same driver home but may use a different model.
# Prints the HOME path.
session_home() {
  local work=$1 model=$2 reviewer=${3:-} driver_home models
  driver_home="$work/driver-home"
  mkdir -p "$driver_home/.local/share/opencode" "$driver_home/.config/opencode"
  cat > "$driver_home/.local/share/opencode/auth.json" <<EOF
{"model": {"type": "api", "key": "$SKAINET_TOKEN"}}
EOF
  chmod 600 "$driver_home/.local/share/opencode/auth.json"
  models="\"$model\": { \"name\": \"$model\", \"limit\": { \"context\": ${E2E_CONTEXT_LIMIT:-$DEFAULT_CONTEXT_LIMIT}, \"output\": ${E2E_OUTPUT_LIMIT:-$DEFAULT_OUTPUT_LIMIT} } }"
  if [ -n "$reviewer" ] && [ "$reviewer" != "$model" ]; then
    models="$models,
        \"$reviewer\": { \"name\": \"$reviewer\", \"limit\": { \"context\": ${E2E_CONTEXT_LIMIT:-$DEFAULT_CONTEXT_LIMIT}, \"output\": ${E2E_OUTPUT_LIMIT:-$DEFAULT_OUTPUT_LIMIT} } }"
  fi
  cat > "$driver_home/.config/opencode/opencode.json" <<EOF
{
  "share": "disabled",
  "provider": {
    "model": {
      "name": "Model",
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "$SKAINET_INTERNAL" },
      "models": {
        $models
      }
    }
  }
}
EOF
  echo "$driver_home"
}

# Run a command with only PATH, HOME and the Go toolchain/cache variables in
# scope. The mechanical checks execute code the pipeline does not trust (the
# scanner's PoC-derived tests), so the job credentials — SECURITY_SCAN_PAT,
# GH_TOKEN, ARCHIVE_REPO — must not be in their environment.
scrubbed() {
  local -a envp=("PATH=$PATH" "HOME=${HOME:-/tmp}")
  local v
  for v in TMPDIR GOPATH GOCACHE GOMODCACHE GOPROXY GOFLAGS GOTOOLCHAIN GOOS GOARCH CGO_ENABLED; do
    [ -n "${!v:-}" ] && envp+=("$v=${!v}")
  done
  env -i "${envp[@]}" "$@"
}

# Args for the session's write-isolation sandbox. The whole filesystem is
# mounted read-only, /tmp and the session's working directory are writable,
# and every .git directory in reach is mounted read-only, so a session cannot
# tamper with hooks or config at the mount level (the runner-side
# assert_git_untampered stays as the backstop). Network is deliberately not
# unshared — the session must reach the model gateway. Ceiling: reads are not
# restricted, so a session can still read the archive; this bounds writes and
# tampering, not visibility.
session_sandbox_args() {
  local workdir=$1 g
  local args=(bwrap --die-with-parent --unshare-pid --unshare-ipc --unshare-uts
              --ro-bind / / --bind /tmp /tmp --dev /dev --proc /proc
              --bind "$workdir" "$workdir")
  for g in "$workdir/.git" "$workdir/repo/.git" "$workdir/archive/.git"; do
    [ -d "$g" ] && args+=(--ro-bind "$g" "$g")
  done
  # CI and supply-chain surfaces are read-only for every session — a fix may
  # edit production files, but never the workflows or the module graph. The
  # runner-side denylist is the backstop where bubblewrap is unavailable.
  # These binds come after the workdir bind so they win over its writability.
  for g in "$workdir/.github" "$workdir/repo/.github" \
           "$workdir/go.mod" "$workdir/go.sum" "$workdir/go.work" "$workdir/go.work.sum" \
           "$workdir/repo/go.mod" "$workdir/repo/go.sum" "$workdir/repo/go.work" "$workdir/repo/go.work.sum"; do
    [ -e "$g" ] && args+=(--ro-bind "$g" "$g")
  done
  printf '%s\n' "${args[@]}"
}

# Run one headless opencode session. The session gets an explicit allowlist,
# not the job environment: env -i is what keeps SECURITY_SCAN_PAT, GH_TOKEN
# and ARCHIVE_REPO out of the session and out of everything it spawns (git,
# go test, a prompt-injected curl). Only the throwaway HOME/XDG dirs, the
# gateway key, PATH and the Go caches pass through. Where bubblewrap is
# available the session also runs inside the write-isolation sandbox above;
# locally (or on a runner without the AppArmor grant) it degrades to an
# unwrapped session with a warning.
#
# --pure: no external plugins — the checkout's own .opencode/ plugins expect
# an omac control plane that does not exist here and would hang the run.
# --auto: non-interactive; writes outside the workdir are auto-rejected.
#
# $1 driver home, $2 timeout secs, $3 model, $4 workdir, $5 prompt file,
# $6 transcript file. Returns the session's exit status.
run_session() {
  local driver_home=$1 secs=$2 model=$3 workdir=$4 prompt_file=$5 transcript=$6 prompt
  # Read the prompt back from a file (avoids bash 3.2's
  # heredoc-in-$() apostrophe bug, same as doc-drift.sh).
  prompt="$(cat "$prompt_file")"
  local -a envp=(
    "HOME=$driver_home"
    "XDG_CONFIG_HOME=$driver_home/.config"
    "XDG_DATA_HOME=$driver_home/.local/share"
    "XDG_STATE_HOME=$driver_home/.local/state"
    "SKAINET_TOKEN=$SKAINET_TOKEN"
    "PATH=$PATH"
  )
  local v
  for v in TMPDIR GOPATH GOCACHE GOMODCACHE GOPROXY GOFLAGS GOTOOLCHAIN GOOS GOARCH CGO_ENABLED; do
    [ -n "${!v:-}" ] && envp+=("$v=${!v}")
  done
  local -a sandbox=()
  if [ "${OMAC_SESSION_SANDBOX:-}" = off ]; then
    # Test/repair seam only, never set by a workflow: a session cannot reach
    # the runner's environment to turn its own sandbox off.
    echo "::warning title=Session sandbox disabled::OMAC_SESSION_SANDBOX=off — a session will run without the write-isolation sandbox." >&2
  elif command -v bwrap >/dev/null 2>&1; then
    while IFS= read -r v; do sandbox+=("$v"); done < <(session_sandbox_args "$workdir")
  else
    echo "::warning title=No bubblewrap::Sessions run without the write-isolation sandbox here; CI installs bubblewrap." >&2
  fi
  (
    cd "$workdir" || exit 1
    # ${sandbox[@]+...} keeps an empty array safe under `set -u` on bash 3.2.
    run_with_timeout "$secs" env -i "${envp[@]}" ${sandbox[@]+"${sandbox[@]}"} \
      opencode run --print-logs --pure --auto -m "model/$model" "$prompt" \
      > "$transcript" 2>&1
  )
}

# Commit whatever is staged with the given identity, signed off. The runner
# shell owns commits (agents only write files), so the agent identity is a
# parameter, not repository config the session could tamper with. --no-verify
# skips any hook that appeared since hardening.
commit_as() {
  local dir=$1 name=$2 email=$3 message=$4
  git -C "$dir" config user.name "$name"
  git -C "$dir" config user.email "$email"
  git -C "$dir" commit -s --no-verify --quiet -m "$message"
}

# Security test files deleted by the commit(s) in $2 (default: the phase
# commit just made), one path per line.
deleted_security_tests() {
  local repo=$1 range=${2:-HEAD~1..HEAD} path
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    case "$path" in *_security_test.go) echo "$path" ;; esac
  done < <(git -C "$repo" diff --diff-filter=D --name-only "$range")
}

# Names from $2 (one per line) that have no "func <name>(" in any *_test.go
# file under $1. Prints the missing ones; no output means all present. The
# name is a Go identifier, so embedding it in the regex is safe.
missing_test_names() {
  local repo=$1 names_file=$2 name missing=""
  while IFS= read -r name; do
    [ -n "$name" ] || continue
    grep -rqE "^func ${name}\(" "$repo" --include='*_test.go' || missing="$missing$name
"
  done < "$names_file"
  printf '%s' "$missing"
}

# SHA of the newest commit since $2 whose subject marks it as a test commit
# ("test(security):" — the runner's prefix; plain "test:" tolerated). The
# tree at that commit is base plus tests only, which is what the mechanical
# red check runs against. Empty output when there is no such commit.
last_test_commit() {
  git -C "$1" log --format='%H%x09%s' "$2..HEAD" \
    | awk -F'\t' '$2 ~ /^test\(security\):|^test:/ { print $1; exit }'
}

# One reviewer session: runs the prompt, then parses the verdict the session
# wrote to <workspace>/review-verdict.json. A missing or internally
# inconsistent verdict is a hard failure — the runner never guesses at a
# verdict. REVIEW.md is moved to $7 (a log-dir path) and review-verdict.json
# removed, so the next session cannot anchor on the previous review.
# Sets REVIEW_FINDINGS and REVIEW_VERDICT. Returns 1 on an unusable verdict.
run_review_session() {
  local driver_home=$1 secs=$2 model=$3 workspace=$4 prompt_file=$5 log_file=$6 keep=$7
  local status=0
  run_session "$driver_home" "$secs" "$model" "$workspace" "$prompt_file" "$log_file" || status=$?
  local verdict_file="$workspace/review-verdict.json"
  if [ ! -f "$verdict_file" ] \
     || ! jq -e '(.findings | type) == "number" and .findings >= 0
                  and (.verdict | type) == "string"
                  and (if .findings == 0 then .verdict == "approved"
                       else .verdict == "insufficient" end)' \
           "$verdict_file" >/dev/null 2>&1; then
    echo "::error title=Reviewer produced no verdict::The reviewer session (exit $status) wrote no consistent review-verdict.json (findings count and verdict must agree). Failing the leg rather than guessing at its verdict."
    return 1
  fi
  # Interface variables for the stage driver (exported so shellcheck sees
  # them as consumed outside this scope).
  local findings verdict
  findings="$(jq -r '.findings' "$verdict_file")"
  verdict="$(jq -r '.verdict' "$verdict_file")"
  export REVIEW_FINDINGS="$findings"
  export REVIEW_VERDICT="$verdict"
  rm -f "$verdict_file"
  if [ -f "$workspace/REVIEW.md" ]; then
    [ -n "$keep" ] && cp "$workspace/REVIEW.md" "$keep"
    rm -f "$workspace/REVIEW.md"
  fi
  return 0
}
