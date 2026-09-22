#!/usr/bin/env bash
#
# issue-stage.sh — Job 3 of the security-remediate workflow: the sanitized
# overview issue.
#
# Creates or updates ONE ongoing issue, modeled on the manual #288: a
# checklist with one sentence per plan (the sanitized issue_line from
# plans.json), plus a deferred count. No agent session runs here — the body
# is assembled mechanically, because the text that goes public must be the
# output of code, not of another model turn. The sanitizer rejects any body
# containing finding text or vulnerable file paths before it is posted.
#
# Idempotent: an open issue carrying the body marker is updated in place
# (merged plans keep their ticked checkboxes), a first run creates it.
#
# Usage (from the job workspace, checkout under repo/):
#   bash repo/.github/scripts/security_remediate/issue-stage.sh
#
# Environment:
#   SCAN_DIR            name of the scan directory inside scans/ (required)
#   SELECTED_IDS        plans this run dispatches, comma ids (for the in-flight
#                       bucket: the legs have not pushed their branches yet)
#   REPO_DIR            this repo's checkout, for the branch lookup (default ./repo)
#   ARCHIVE_DIR         archive clone, created if missing (default ./archive)
#   GH_TOKEN            github.token with issues:write (issue + labels)
#   ARCHIVE_REPO, SECURITY_SCAN_PAT   (clone)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

SCAN_DIR="${SCAN_DIR:-}"
SELECTED_IDS="${SELECTED_IDS:-}"
REPO_DIR="${REPO_DIR:-$PWD/repo}"
ARCHIVE_DIR="${ARCHIVE_DIR:-$PWD/archive}"

require_tools jq gh git

MARKER="<!-- security-remediation: overview -->"
ISSUE_TITLE="Security hardening"

if [ -z "$SCAN_DIR" ]; then
  echo "::error title=No scan::SCAN_DIR is not set — the preflight stage names the scan."
  exit 1
fi
if [ -z "${SECURITY_SCAN_PAT:-}" ] || [ -z "${ARCHIVE_REPO:-}" ]; then
  echo "::error title=Missing secrets::SECURITY_SCAN_PAT or SECURITY_ARCHIVE_REPO is not set."
  exit 1
fi

[ -d "$ARCHIVE_DIR" ] || clone_archive "$ARCHIVE_DIR" "$ARCHIVE_REPO" "$SECURITY_SCAN_PAT"

plans_json="$ARCHIVE_DIR/scans/$SCAN_DIR/mitigation-plans/plans.json"
vulns_json="$ARCHIVE_DIR/scans/$SCAN_DIR/vulnerabilities.json"
if [ ! -f "$plans_json" ]; then
  echo "::error title=No manifest::plans.json is missing for scan '$SCAN_DIR'. The plan stage must run before this one."
  exit 1
fi
[ -f "$vulns_json" ] || {
  echo "::error title=No findings file::Scan '$SCAN_DIR' has no vulnerabilities.json. Refusing to assemble a public issue without the disclosure gate's source data."
  exit 1
}

# --- Find the existing overview issue -----------------------------------------
# Match by the security + agent-created labels and the body marker, so the
# manual #288 (same topic, no marker) is never touched and re-runs update
# instead of duplicating.
existing_number=""
while IFS=$'\t' read -r number body; do
  if [ -n "$number" ] && printf '%s' "$body" | grep -Fq "$MARKER"; then
    existing_number="$number"
    break
  fi
done < <(gh issue list -R "$GITHUB_REPOSITORY" --label security --label agent-created --state open \
          --json number,body --jq '.[] | [.number, .body] | @tsv' 2>/dev/null || true)

# --- Assemble the body --------------------------------------------------------
# The body is rebuilt on every run from the manifest plus the live PR states,
# so the status counts always match the workstream list (the old "Further
# steps" count was read as additional work). Ticks derive from merged PRs.
plan_count="$(jq 'length' "$plans_json")"
{ read -r merged_line; read -r open_line; read -r _unstarted_line; } <<< "$(plan_pr_states "$plans_json")"
merged_ids="${merged_line#merged:}"
open_ids="${open_line#open:}"

# In flight = dispatched before (its branch exists) or by this very run (its
# leg has not pushed yet, but it is selected), and no pull request yet. The
# run's selection is the piece finalize cannot know, which is why the issue
# stage writes this bucket at dispatch time.
dispatched_ids="$(dispatched_bucket \
  "$(branch_dispatched_ids "$REPO_DIR" "$SCAN_DIR")" "$SELECTED_IDS" "$merged_ids" "$open_ids")"

body_file="$(mktemp)"
trap 'rm -f "$body_file"' EXIT
overview_body "$plans_json" "$merged_ids" "$open_ids" "$dispatched_ids" \
  "$vulns_json" "${SEVERITY_THRESHOLD:-high}" > "$body_file"

# --- Sanitizer gate, then post ------------------------------------------------
if ! sanitize_issue_body "$body_file" "$vulns_json" "$plans_json"; then
  echo "::error title=Issue body rejected by the sanitizer::The assembled overview issue body contains material from the scan findings. Nothing was posted. Compare plans.json's issue_line entries in the private archive repo against the finding texts to find the offender."
  exit 1
fi

if [ -n "$existing_number" ]; then
  gh issue edit -R "$GITHUB_REPOSITORY" "$existing_number" --body-file "$body_file" --title "$ISSUE_TITLE" >/dev/null
  issue_number="$existing_number"
  echo "updated overview issue #$issue_number"
else
  gh label create -R "$GITHUB_REPOSITORY" security --force >/dev/null 2>&1 || true
  gh label create -R "$GITHUB_REPOSITORY" agent-created --force >/dev/null 2>&1 || true
  create_status=0
  create_out="$(gh issue create -R "$GITHUB_REPOSITORY" --title "$ISSUE_TITLE" \
    --body-file "$body_file" --label security --label agent-created 2>&1)" || create_status=$?
  issue_number="$(printf '%s' "$create_out" | grep -oE 'issues/[0-9]+' | head -n1 | grep -oE '[0-9]+')"
  if [ "$create_status" -ne 0 ] || [ -z "$issue_number" ]; then
    echo "::error title=Issue creation failed::gh exited $create_status. Details: $(printf '%s' "$create_out" | head -n1)"
    exit 1
  fi
  echo "created overview issue #$issue_number"
fi

emit_output overview_issue "$issue_number"
{
  echo "## Security remediation: overview issue"
  echo ""
  echo "Issue #${issue_number} carries ${plan_count} workstreams (merged $(id_count "$merged_ids"), in review $(id_count "$open_ids"), in flight $(id_count "$dispatched_ids"), not yet dispatched $(( plan_count - $(id_count "$merged_ids") - $(id_count "$open_ids") - $(id_count "$dispatched_ids") )))."
} >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
