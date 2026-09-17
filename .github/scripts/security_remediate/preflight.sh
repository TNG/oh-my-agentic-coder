#!/usr/bin/env bash
#
# preflight.sh — Job 1 of the security-remediate workflow: gate the run.
#
# Decides within the first seconds whether there is anything to remediate and
# whether the credentials can deliver the results, BEFORE any agent session is
# spent. Emits (via $GITHUB_OUTPUT): scan_dir, vuln_count, plan_needed,
# selected_ids, deferred_count, no_op, and a counts-only step summary.
#
# A run is a no-op when the newest scan is clean, partial or failed (partial
# findings are stale data: batching and budget selection on them would
# mis-prioritize), or when every plan already has an open or merged pull
# request — pull request existence is the pipeline's single source of truth
# for "executed".
#
# Usage (from the job workspace, checkout under repo/):
#   bash repo/.github/scripts/security_remediate/preflight.sh
#
# Environment:
#   SECURITY_SCAN_PAT        write PAT (required)
#   ARCHIVE_REPO             private companion repo (required)
#   GITHUB_REPOSITORY        this repo
#   GH_TOKEN                  for `gh pr list` (read-scoped is enough)
#   MAX_PLANS                budget, plans selected per run (default 3)
#   PLANS_FILTER             restrict to these plan ids, comma-separated
#   ARCHIVE_DIR              where to clone (default: ./archive)

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
. "$HERE/lib.sh"

MAX_PLANS="${MAX_PLANS:-3}"
PLANS_FILTER="${PLANS_FILTER:-}"
ARCHIVE_DIR="${ARCHIVE_DIR:-$PWD/archive}"

require_tools jq gh git curl

if [ -z "${SECURITY_SCAN_PAT:-}" ] || [ -z "${ARCHIVE_REPO:-}" ]; then
  echo "::error title=Missing secrets::SECURITY_SCAN_PAT or SECURITY_ARCHIVE_REPO is not set. The remediation pipeline can neither read the private archive repo nor write its results, so it would burn agent sessions on undeliverable work."
  exit 1
fi
case "$MAX_PLANS" in
  ''|*[!0-9]*)
    echo "::error::max_plans must be a whole number, got '$MAX_PLANS'" >&2
    exit 2 ;;
esac

# Both repos get the write probe: the archive repo for plan/report pushes,
# this repo for the branches, issues and pull requests the later stages open.
# A PAT that cannot deliver is caught here, in the first ten seconds.
probe_write_access "$ARCHIVE_REPO" "$SECURITY_SCAN_PAT" "Archive repo"
probe_write_access "$GITHUB_REPOSITORY" "$SECURITY_SCAN_PAT" "This repo"

clone_archive "$ARCHIVE_DIR" "$ARCHIVE_REPO" "$SECURITY_SCAN_PAT"

no_op_summary() {
  # $1 = one-line reason. Counts and scan-dir names only — never finding data.
  {
    echo "## Security remediation: no-op"
    echo ""
    echo "$1"
  } >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
}

# Absent outputs read as empty strings in later jobs' expressions, so every
# exit path emits the full set.
emit_all() {
  emit_output scan_dir "$1"
  emit_output vuln_count "$2"
  emit_output plan_needed "$3"
  emit_output selected_ids "$4"
  emit_output deferred_count "$5"
  emit_output no_op "$6"
}

scan_dir="$(newest_scan_dir "$ARCHIVE_DIR")"
if [ -z "$scan_dir" ]; then
  emit_all "" 0 false "" 0 true
  no_op_summary "The private archive repo contains no scans, so there is nothing to remediate."
  exit 0
fi

status="$(scan_dir_status "$scan_dir")"
if [ "$status" != ok ]; then
  emit_all "$scan_dir" 0 false "" 0 true
  no_op_summary "Newest scan \`$scan_dir\` is \`$status\`, not \`ok\` — ${status} scans are not remediated (their findings are ${status}-scan data), so this run waits for the next complete scan."
  exit 0
fi

vulns_json="$ARCHIVE_DIR/scans/$scan_dir/vulnerabilities.json"
if [ ! -f "$vulns_json" ]; then
  # A completed scan with no findings file is a clean result, not an anomaly
  # (security-scan.yml accepts rc=0-without-file as clean, #194).
  emit_all "$scan_dir" 0 false "" 0 true
  no_op_summary "Newest scan \`$scan_dir\` recorded no findings — nothing to remediate."
  exit 0
fi
vuln_count="$(jq 'length' "$vulns_json")"
if [ "$vuln_count" -eq 0 ]; then
  emit_all "$scan_dir" 0 false "" 0 true
  no_op_summary "Newest scan \`$scan_dir\` recorded 0 findings — nothing to remediate."
  exit 0
fi

plans_json="$ARCHIVE_DIR/scans/$scan_dir/mitigation-plans/plans.json"
if [ -f "$plans_json" ]; then
  plan_needed=false
  { read -r selected_ids; read -r deferred_count; } <<< "$(select_plans "$plans_json" "$MAX_PLANS" "$PLANS_FILTER")"
  if [ -z "$selected_ids" ] && [ "$deferred_count" -eq 0 ]; then
    no_op=true
    no_op_summary "Newest scan \`$scan_dir\` has ${vuln_count} findings; every plan already has an open or merged pull request, so this run has nothing left to execute."
  else
    no_op=false
  fi
else
  plan_needed=true
  selected_ids=""
  deferred_count=0
  no_op=false
fi

emit_all "$scan_dir" "$vuln_count" "$plan_needed" "$selected_ids" "$deferred_count" "$no_op"

if [ "$no_op" != true ]; then
  {
    echo "## Security remediation"
    echo ""
    echo "Newest scan: \`$scan_dir\` — ${vuln_count} findings. Plans: $([ "$plan_needed" = true ] && echo "none yet, planning stage will run" || echo "present, ${selected_ids:-none} selected for this run, ${deferred_count} deferred")."
  } >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
fi
