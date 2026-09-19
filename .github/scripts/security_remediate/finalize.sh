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
#   GH_TOKEN      github.token with issues:write (edit and close)
#   plus the lib.sh variables (ARCHIVE_REPO, SECURITY_SCAN_PAT)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

ARCHIVE_DIR="${ARCHIVE_DIR:-$PWD/archive}"
REPO_DIR="${REPO_DIR:-$PWD/repo}"

require_tools jq gh git curl

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
{ read -r merged_line; read -r open_line; read -r unstarted_line; } <<< "$(plan_pr_states "$plans_json")"
merged_ids="${merged_line#merged:}"
open_ids="${open_line#open:}"
unstarted_ids="${unstarted_line#unstarted:}"
merged_count="$(id_count "$merged_ids")"
# Finalize does not dispatch, so it derives in-flight purely from the branches
# on origin; by the time it runs daily, a dispatched leg has pushed one.
dispatched_ids="$(dispatched_bucket \
  "$(branch_dispatched_ids "$REPO_DIR" "$scan_dir")" "" "$merged_ids" "$open_ids")"

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

# --- Overview issue: refresh status and ticks, close when all merged ------------
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
  # The body is rebuilt with the shared generator, so the status counts and
  # the ticks always match the manifest; the edit is skipped when nothing
  # changed, keeping the daily run a true no-op.
  body_file="$(mktemp)"
  overview_body "$plans_json" "$merged_ids" "$open_ids" "$dispatched_ids" > "$body_file"
  if [ "$(cat "$body_file")" = "$issue_body" ]; then
    echo "overview issue #${issue_number} already up to date"
  else
    gh issue edit -R "$GITHUB_REPOSITORY" "$issue_number" --body-file "$body_file" >/dev/null
    echo "refreshed overview issue #${issue_number} (${merged_count}/${plan_count} merged)"
  fi
  rm -f "$body_file"

  if [ "$merged_count" -eq "$plan_count" ] && [ "$plan_count" -gt 0 ]; then
    gh issue close -R "$GITHUB_REPOSITORY" "$issue_number" --comment "All ${plan_count} planned workstreams merged. The finalize workflow will stay quiet until the next scan." >/dev/null
    echo "all plans merged — closed overview issue #${issue_number}"
  fi
else
  echo "no open overview issue — report only"
fi

printf 'Security finalize: %s/%s merged, %s open.\n' \
  "$merged_count" "$plan_count" "$(id_count "$open_ids")" \
  >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
