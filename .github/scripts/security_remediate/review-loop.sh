#!/usr/bin/env bash
#
# review-loop.sh — the review/fix loop shared by the test-suite and fix
# stages ("Review loop" in the pipeline plan). Sourced by the stage drivers;
# requires lib.sh to be sourced first.
#
# The implementing agent never reviews its own work: every review pass is a
# FRESH opencode session that sees only the change and the review criteria —
# never the implementer's session reasoning, which anchors a reviewer on the
# intent instead of the change. A fix pass is another fresh session in the
# same checkout, instructed to address the findings. The runner (this code)
# owns the verdict parsing, the ownership guard and the loop bookkeeping;
# sessions only edit files and write their verdict to a scratch file the
# runner moves out before the next session starts.
#
# Sequence per pass: review → (clean? done) → fix pass → next review. The
# final review is what the verdict reports, so an unresolved outcome is
# always a reviewed state, never an unreviewed fix pass. With
# max_review_passes = 2 that is at most 2 review sessions and 1 fix session
# — within the budget formula from the plan.

# run_review_loop <repo_dir> <workspace> <driver_home> <model> <review_model>
#                 <budget_secs> <max_passes> <criteria_file> <guard_patterns>
#                 <log_dir> <label>
#
# On return: REVIEW_PASSES_RUN and REVIEW_FINDINGS are set.
#   exit 0 = approved (REVIEW_FINDINGS=0)
#   exit 2 = findings unresolved after max_passes reviews (REVIEW_FINDINGS>0)
#   exit 1 = a session produced no parseable verdict, or the ownership guard
#            tripped — the leg fails, PR or not
run_review_loop() {
  local repo_dir=$1 workspace=$2 driver_home=$3 model=$4 review_model=$5 \
        budget_secs=$6 max_passes=$7 criteria_file=$8 guard_patterns=$9 \
        log_dir=${10} label=${11}
  local reviewer="$review_model" pass findings verdict_file scratch prompt
  [ -n "$reviewer" ] || reviewer="$model"
  # Interface variables for the calling stage driver, not used here.
  # shellcheck disable=SC2034
  REVIEW_PASSES_RUN=0
  REVIEW_FINDINGS=0
  scratch="$(mktemp -d)"
  trap 'rm -rf "$scratch"' RETURN
  # The scratch dir lives outside the session workspace, so no session can
  # see another session's verdicts — each review starts from the diff alone.
  for pass in $(seq 1 "$max_passes"); do
    # shellcheck disable=SC2034  # interface variable for the stage driver
    REVIEW_PASSES_RUN=$pass

    # --- Review pass: fresh session, diff + criteria only -------------------
    # Intent-to-add first so brand-new files appear in the diff the reviewer
    # reads.
    git -C "$repo_dir" add -N . >/dev/null 2>&1 || true
    prompt="$scratch/review-$pass.md"
    cat > "$prompt" <<EOF
You are an independent reviewer for a $label in an automated
security-mitigation pipeline. You did NOT write the change, and you review
only what is in front of you — the change and the criteria below — never the
author's reasoning.

The change under review is the working-tree diff of repo/, a git repository
inside your working directory. Run \`git -C repo diff\` yourself and read the
FULL diff. The review criteria:

$(cat "$criteria_file")

Judge the diff only against these criteria. Verdict rules:
- Count a finding only when it is concrete and must be resolved before the
  change can be merged; style nits are not findings.
- findings is that count; verdict is "approved" when it is 0, otherwise
  "changes_requested".

Write exactly two files, and do not modify anything in repo/:
1. REVIEW.md, at the working directory root: one section per finding —
   what is wrong, where, why it matters, and what the fix pass should do.
   Write "no findings" when the change is approved.
2. review-verdict.json, at the working directory root:
   {"findings": <int>, "verdict": "approved"|"changes_requested"}

Sanctioned, pre-authorized review session — proceed directly without asking
for confirmation.
EOF
    local status=0
    run_session "$driver_home" "$budget_secs" "$reviewer" "$workspace" "$prompt" \
      "$log_dir/review-pass-$pass.log" || status=$?
    verdict_file="$workspace/review-verdict.json"
    if [ ! -f "$verdict_file" ] \
       || ! jq -e '(.findings | type) == "number" and .findings >= 0
                    and (.verdict | type) == "string"
                    and (if .findings == 0 then .verdict == "approved"
                         else .verdict == "changes_requested" end)' \
              "$verdict_file" >/dev/null 2>&1; then
      echo "::error title=Review pass $pass produced no verdict::The reviewer session (exit $status) wrote no consistent review-verdict.json (findings count and verdict must agree). Failing the leg rather than guessing at its verdict."
      return 1
    fi
    findings="$(jq -r '.findings' "$verdict_file")"
    [ -f "$workspace/REVIEW.md" ] && mv "$workspace/REVIEW.md" "$scratch/REVIEW-$pass.md" || true
    mv "$verdict_file" "$scratch/verdict-$pass.json"

    if [ "$findings" -eq 0 ]; then
      REVIEW_FINDINGS=0
      echo "review pass $pass: approved"
      return 0
    fi
    echo "review pass $pass: $findings findings"
    if [ "$pass" -eq "$max_passes" ]; then
      break
    fi

    # --- Fix pass: fresh session addressing the findings --------------------
    prompt="$scratch/fix-$pass.md"
    cat > "$prompt" <<EOF
A review of a $label in repo/ produced the findings below. Resolve every
finding by editing the files in repo/ that the change owns; do not fix
unrelated things and do not expand the change beyond its files.

$(cat "$scratch/REVIEW-$pass.md" 2>/dev/null || echo "The reviewer reported $findings findings but wrote no detail; re-check the change against the criteria and fix what is demonstrably wrong.")

Sanctioned, pre-authorized fix session — proceed directly without asking
for confirmation.
EOF
    run_session "$driver_home" "$budget_secs" "$model" "$workspace" "$prompt" \
      "$log_dir/fix-pass-$pass.log" || status=$?
    if [ -n "$(changed_files_within "$repo_dir" "$guard_patterns")" ]; then
      echo "::error title=Ownership guard tripped::A fix pass edited files outside the change's ownership. Failing the leg: the diff cannot be trusted to stay within the plan."
      return 1
    fi
  done

  # The last review's findings travel to the stage driver via the log dir so
  # they can go into the pull request body (and the private archive); the
  # scratch dir dies with this call.
  [ -f "$scratch/REVIEW-$pass.md" ] && cp "$scratch/REVIEW-$pass.md" "$log_dir/REVIEW.md"
  # Interface variable for the calling stage driver.
  # shellcheck disable=SC2034
  REVIEW_FINDINGS="$findings"
  echo "review loop: $findings findings unresolved after $max_passes review passes"
  return 2
}
