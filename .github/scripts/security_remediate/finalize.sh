#!/usr/bin/env bash
#
# finalize.sh — the security-finalize workflow's single job.
#
# The remediation workflow's triggers do not re-fire, so this runs on a daily
# cron (plus manual dispatch) and babysits what the remediation runs left
# behind: it checks the fix and test-suite pull requests' merge states, ticks
# the overview issue's checkboxes for merged plans, appends a status section
# to the private archive, and closes the overview issue once every plan of
# the current generation has merged. When nothing changed it is a cheap
# no-op.
#
# Only counts and plan ids reach the public job log; the per-plan detail
# goes to the private archive.
#
# Usage (from the job workspace, checkout under repo/):
#   bash repo/.github/scripts/security_remediate/finalize.sh
#
# Environment:
#   ARCHIVE_DIR   archive clone, created if missing (default ./archive)
#   GH_TOKEN      the write PAT (issue edit and close)
#   plus the lib.sh variables (ARCHIVE_REPO, SECURITY_SCAN_PAT)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

ARCHIVE_DIR="${ARCHIVE_DIR:-$PWD/archive}"

require_tools jq gh git curl

# Number of comma-separated ids in $1 (empty input is zero).
id_count() {
  [ -z "$1" ] && { echo 0; return; }
  printf '%s' "$1" | awk -F, '{print NF}'
}

if [ -z "${SECURITY_SCAN_PAT:-}" ] || [ -z "${ARCHIVE_REPO:-}" ]; then
  echo "::error title=Missing secrets::SECURITY_SCAN_PAT or SECURITY_ARCHIVE_REPO is not set."
  exit 1
fi

[ -d "$ARCHIVE_DIR" ] || clone_archive "$ARCHIVE_DIR" "$ARCHIVE_REPO" "$SECURITY_SCAN_PAT"

# The generation to finalize: the newest scan that has a manifest. Older,
# unmerged generations keep their pull requests — a human merges or closes
# them; this stage only reports.
scan_dir=""
while IFS= read -r d; do
  if [ -f "$ARCHIVE_DIR/scans/$d/mitigation-plans/plans.json" ]; then
    scan_dir="$d"
    break
  fi
done < <(ls -1 "$ARCHIVE_DIR/scans" 2>/dev/null | LC_ALL=C sort -r)
if [ -z "$scan_dir" ]; then
  echo "no scan with a manifest in the archive — nothing to finalize"
  exit 0
fi
scan_abs="$ARCHIVE_DIR/scans/$scan_dir"
plans_json="$scan_abs/mitigation-plans/plans.json"
# The manifest feeds the issue ticks and the all-merged close; a tampered one
# must not be trusted (an empty issue_line would tick every line).
schema_errors="$(plans_schema_errors "$plans_json" "$scan_dir")"
if [ -n "$schema_errors" ]; then
  echo "::error title=Manifest invalid::plans.json for '$scan_dir' fails its schema; refusing to tick or close the overview issue."
  exit 1
fi
plan_count="$(jq 'length' "$plans_json")"

# --- Pull request states --------------------------------------------------------
merged_ids=""
open_ids=""
unstarted_ids=""
while IFS= read -r entry; do
  id="$(printf '%s' "$entry" | jq -r '.id')"
  branch="$(printf '%s' "$entry" | jq -r '.branch')"
  pr_state="$(gh pr list -R "$GITHUB_REPOSITORY" --head "$branch" --state all \
    --limit 1 --json state --jq '.[0].state' 2>/dev/null || true)"
  case "$pr_state" in
    MERGED) merged_ids="${merged_ids}${id}," ;;
    OPEN)   open_ids="${open_ids}${id}," ;;
    *)      unstarted_ids="${unstarted_ids}${id}," ;;
  esac
done < <(jq -c '.[]' "$plans_json")
merged_ids="${merged_ids%,}"
open_ids="${open_ids%,}"
unstarted_ids="${unstarted_ids%,}"
merged_count="$(id_count "$merged_ids")"

# The no-op fingerprint: when the state line matches the last recorded one,
# nothing happened since the previous finalize run and the day is done.
status_file="$scan_abs/remediation/status.md"
fingerprint="merged: ${merged_ids:-none} | open: ${open_ids:-none} | unstarted: ${unstarted_ids:-none}"
last_state=""
[ -f "$status_file" ] && last_state="$(grep -F '<!-- state:' "$status_file" | tail -n1 || true)"
if [ "$last_state" = "<!-- state: $fingerprint -->" ]; then
  echo "nothing changed since the last finalize run (${merged_count}/${plan_count} merged)"
  printf 'Security finalize: idle, %s/%s merged.\n' "$merged_count" "$plan_count" >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
  exit 0
fi

# --- Private report -------------------------------------------------------------
mkdir -p "$scan_abs/remediation"
{
  echo "## $(date -u '+%F %H:%M UTC') — remediation status for ${scan_dir}"
  echo ""
  echo "- merged (${merged_count}): ${merged_ids:-none}"
  echo "- open pull requests: ${open_ids:-none}"
  echo "- not started (deferred or leg failed): ${unstarted_ids:-none}"
  echo ""
  echo "<!-- state: $fingerprint -->"
} >> "$status_file"
push_archive "$ARCHIVE_DIR" "remediation status: ${scan_dir} ($(date -u +%F))" \
  "scans/$scan_dir/remediation/status.md"

# --- Overview issue: tick merged plans, close when all merged -------------------
MARKER="<!-- security-remediation: overview -->"
issue_body=""
issue_number=""
while IFS=$'\t' read -r number body; do
  if [ -n "$number" ] && printf '%s' "$body" | grep -Fq "$MARKER"; then
    issue_number="$number"
    issue_body="$body"
    break
  fi
done < <(gh issue list -R "$GITHUB_REPOSITORY" --label security --label agent-created \
          --state open --json number,body --jq '.[] | [.number, .body] | @tsv' 2>/dev/null || true)

if [ -n "$issue_number" ]; then
  # One flip per merged plan: the body's unticked line becomes ticked. Matched
  # literally at line start, so no plan text is interpreted as a regex.
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    issue_body="$(printf '%s\n' "$issue_body" | awk -v t="- [ ] $line" -v r="- [x] $line" \
      '{ if (index($0, t) == 1) print r; else print }')"
  done < <(jq -r --arg ids "$merged_ids" \
           '($ids | split(",") | map(select(length > 0))) as $m
            | map(select(.id as $i | ($m | index($i))))
            | .[] | select(.issue_line != null and .issue_line != "") | .issue_line' "$plans_json")
  body_file="$(mktemp)"
  printf '%s\n' "$issue_body" > "$body_file"
  gh issue edit "$issue_number" --body-file "$body_file" >/dev/null
  rm -f "$body_file"
  echo "ticked ${merged_count} merged plan(s) in overview issue #${issue_number}"

  if [ "$merged_count" -eq "$plan_count" ] && [ "$plan_count" -gt 0 ]; then
    gh issue close "$issue_number" --comment "All ${plan_count} planned workstreams merged. The finalize workflow will stay quiet until the next scan." >/dev/null
    echo "all plans merged — closed overview issue #${issue_number}"
  fi
else
  echo "no open overview issue — report only"
fi

printf 'Security finalize: %s/%s merged, %s open.\n' \
  "$merged_count" "$plan_count" "$(id_count "$open_ids")" \
  >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
