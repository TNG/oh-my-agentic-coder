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
#   SECURITY_SCAN_PAT  the write PAT (probe, archive clone/push)
#   ARCHIVE_REPO       private companion repo, owner/name
#   SKAINET_TOKEN      model gateway API key (agent sessions)
#   SKAINET_INTERNAL   model gateway base URL (agent sessions)

# Keep in sync with internal/e2e/versions.go's "opencode" pin, same as
# scripts/doc-drift.sh.
DEFAULT_OPENCODE_VERSION="opencode-ai@1.17.12"
DEFAULT_CONTEXT_LIMIT="100000"
DEFAULT_OUTPUT_LIMIT="32000"

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
  local err status=0
  err=$(git -C "$dir" "$@" 2>&1) || status=$?
  if [ "$status" -ne 0 ]; then
    printf '%s\n' "$err" | sed "s/${SECURITY_SCAN_PAT}/REDACTED-PAT/g" >&2
    return "$status"
  fi
  [ -z "$err" ] || printf '%s\n' "$err" >&2
}

# Shallow-clone the private archive repo and give the clone a commit identity,
# so stages only ever add content and push.
clone_archive() {
  local dest=$1 repo=$2 pat=$3
  archive_git . clone --quiet --depth 1 \
    "https://x-access-token:${pat}@github.com/${repo}.git" "$dest" || {
    echo "::error title=Archive clone failed::Could not clone the private archive repo (details above are redacted — they embed the credential URL)."
    return 1
  }
  git -C "$dest" config user.name "security-remediate-bot"
  git -C "$dest" config user.email "actions@users.noreply.github.com"
}

# Commit and push whatever the archive clone now contains. Nothing to commit
# is success: re-dispatching a stage that already delivered is a no-op, which
# is how the pipeline's idempotence is meant to work.
push_archive() {
  local dir=$1 message=$2
  git -C "$dir" add .
  if git -C "$dir" diff --cached --quiet; then
    echo "archive: nothing to push"
    return 0
  fi
  git -C "$dir" commit --quiet -m "$message"
  archive_git "$dir" push --quiet origin HEAD || {
    echo "::error title=Archive push failed::Pushing the archive repo failed after the preflight confirmed write access — check for a protected default branch or a token revoked mid-run."
    return 1
  }
  echo "archive: pushed"
}

# Scan dirs are named "<label>-<reason>-<status>" and the label is always
# UTC %Y%m%d-%H%M%S (security-scan.yml mints it that way), so lexical order
# is chronological order and `sort -r` puts the newest first.
newest_scan_dir() {
  local archive_dir=$1
  ls -1 "${archive_dir}/scans" 2>/dev/null | sort -r | head -1 || true
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
  gh pr list -R "$GITHUB_REPOSITORY" --state all --limit 1000 \
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
# permission), test names are TestSecurity* because they run behind the vuln
# build tag. Prints one "<id>: <problem>" line per violation; no output means
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
          (if (($e.files // []) | type) != "array" or ($e.files // [] | length) == 0 then "\($id): files must be a non-empty array" else empty end),
          (if (($e.files // []) | any(. as $x | ($x | type) != "string")) then "\($id): files entries must be strings" else empty end),
          (if (($e.files // []) | map(select(type == "string")) | any(startswith(".github/"))) then "\($id): plans never own .github/ files — CI-config findings are manual follow-up" else empty end),
          (if (($e.tests // []) | type) != "array" or ($e.tests // [] | length) == 0 then "\($id): tests must be a non-empty array" else empty end),
          (if (($e.tests // []) | any(. as $x | ($x | type) != "string")) then "\($id): tests entries must be strings" else empty end),
          (if (($e.tests // []) | map(select(type == "string")) | any(test("^TestSecurity") | not)) then "\($id): test names must start with TestSecurity" else empty end),
          (if badstr($e.issue_line) then "\($id): issue_line must be a non-empty string" else empty end),
          (if badstr($e.review_criteria) then "\($id): review_criteria must be a non-empty string" else empty end),
          (if badstr($e.plan_file) then "\($id): plan_file must be a non-empty string" else empty end)
        ])
       + (if ([ .[] | (.id // "") ] | length) > ([ .[] | (.id // "") ] | unique | length) then ["-: duplicate plan ids"] else [] end)
     end) | .[]
  ' "$plans_json"
}

# Strings that must never appear verbatim in the public overview issue: every
# finding's title, description and impact, any PoC/exploit-style field the
# scanner happens to emit, and the plans' owned file paths (they point
# straight at the vulnerable code). Used by the sanitizer below; short
# values are skipped there to keep false positives down.
issue_body_forbidden_strings() {
  local vulns_json=$1 plans_json=$2
  {
    jq -r '.[] | (.title // empty), (.description // empty), (.impact // empty)' \
      "$vulns_json" 2>/dev/null || true
    jq -r '.[] | to_entries[]
            | select((.key | test("poc|exploit|proof"; "i")) and (.value | type == "string"))
            | .value' "$vulns_json" 2>/dev/null || true
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
sanitize_issue_body() {
  local body_file=$1 vulns_json=$2 plans_json=$3 s
  while IFS= read -r s; do
    # Trim the ends only — multi-word finding text must keep its interior
    # whitespace to stay findable in the body.
    s="${s#"${s%%[![:space:]]*}"}"
    s="${s%"${s##*[![:space:]]}"}"
    [ "${#s}" -ge 8 ] || continue
    if grep -Fqi -- "$s" "$body_file"; then
      echo "issue body contains a string from the scan findings or the vulnerable file paths (string withheld)"
      return 1
    fi
  done < <(issue_body_forbidden_strings "$vulns_json" "$plans_json")
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
  local repo=$1 patterns=$2 path
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
      echo "$path"
      return 1
    fi
  done < <(git -C "$repo" status --porcelain)
}

# Install the pinned opencode CLI the sessions run with.
install_opencode() {
  bun install -g "${E2E_VERSION_OPENCODE:-$DEFAULT_OPENCODE_VERSION}"
}

# Fresh HOME for a session's opencode CLI so it never picks up the runner's
# real omac/opencode state, holding only the generated gateway config — the
# same isolation scripts/doc-drift.sh applies. Prints the HOME path.
session_home() {
  local work=$1 model=$2 driver_home
  driver_home="$work/driver-home"
  mkdir -p "$driver_home/.local/share/opencode" "$driver_home/.config/opencode"
  cat > "$driver_home/.local/share/opencode/auth.json" <<EOF
{"model": {"type": "api", "key": "$SKAINET_TOKEN"}}
EOF
  chmod 600 "$driver_home/.local/share/opencode/auth.json"
  cat > "$driver_home/.config/opencode/opencode.json" <<EOF
{
  "share": "disabled",
  "provider": {
    "model": {
      "name": "Model",
      "npm": "@ai-sdk/openai-compatible",
      "options": { "baseURL": "$SKAINET_INTERNAL" },
      "models": {
        "$MODEL": { "name": "$MODEL", "limit": { "context": ${E2E_CONTEXT_LIMIT:-$DEFAULT_CONTEXT_LIMIT}, "output": ${E2E_OUTPUT_LIMIT:-$DEFAULT_OUTPUT_LIMIT} } }
      }
    }
  }
}
EOF
  echo "$driver_home"
}

# Run one headless opencode session. The environment is narrowed to exactly
# what the session needs: the throwaway HOME and XDG dirs, the gateway key
# (the session must call the gateway; that is the only credential it gets),
# and PATH. The PAT, GH_TOKEN and ARCHIVE_REPO stay with the runner shell.
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
  (
    cd "$workdir" || exit 1
    HOME="$driver_home" \
    XDG_CONFIG_HOME="$driver_home/.config" \
    XDG_DATA_HOME="$driver_home/.local/share" \
    XDG_STATE_HOME="$driver_home/.local/state" \
    SKAINET_TOKEN="$SKAINET_TOKEN" \
    PATH="$PATH" \
    run_with_timeout "$secs" \
      opencode run --print-logs --pure --auto -m "model/$model" "$prompt" \
      > "$transcript" 2>&1
  )
}
