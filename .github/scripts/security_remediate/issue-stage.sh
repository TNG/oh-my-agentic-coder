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
#   DEFERRED_COUNT      deferred plans, for the "Further steps" count
#   ARCHIVE_DIR         archive clone, created if missing (default ./archive)
#   GH_TOKEN            github.token with issues:write (issue + labels)
#   ARCHIVE_REPO, SECURITY_SCAN_PAT   (clone)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

SCAN_DIR="${SCAN_DIR:-}"
DEFERRED_COUNT="${DEFERRED_COUNT:-0}"
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
existing_body=""
while IFS=$'\t' read -r number body; do
  if [ -n "$number" ] && printf '%s' "$body" | grep -Fq "$MARKER"; then
    existing_number="$number"
    existing_body="$body"
    break
  fi
done < <(gh issue list -R "$GITHUB_REPOSITORY" --label security --label agent-created --state open \
          --json number,body --jq '.[] | [.number, .body] | @tsv' 2>/dev/null || true)

# --- Assemble the body --------------------------------------------------------
plan_count="$(jq 'length' "$plans_json")"
body_file="$(mktemp)"
trap 'rm -f "$body_file"' EXIT
{
  echo "$MARKER"
  echo ""
  echo "The security remediation pipeline tracks its workstreams here. Each item is"
  echo "one origin-batched fix plan, executed by an automated pipeline whose pull"
  echo "requests reference this issue. Detailed plans live in the private companion"
  echo "repo; this issue stays sanitized by design."
  echo ""
  echo "## Workstreams"
  jq -r 'sort_by(.priority, .id) | .[] | .issue_line' "$plans_json" | while IFS= read -r line; do
    state=' '
    if [ -n "$existing_body" ] && printf '%s' "$existing_body" | grep -Fq -- "- [x] $line"; then
      state='x'
    fi
    echo "- [$state] $line"
  done
  if [ "$DEFERRED_COUNT" -gt 0 ]; then
    echo ""
    echo "## Further steps"
    echo ""
    echo "${DEFERRED_COUNT} further workstreams are scheduled for follow-up runs of this pipeline; re-running it picks them up in priority order."
  fi
  echo ""
  echo "## Notes"
  echo ""
  echo "- Merging stays a human action: every pull request needs at least one approval (\`COLLABORATION.md\`)."
} > "$body_file"

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
  echo "Issue #${issue_number} carries ${plan_count} workstreams; ${DEFERRED_COUNT} deferred."
} >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
