# Triage

You have the reports of the reviewers that ran, and the test results (`tests.md`). Your job is to turn them into a short list of findings that are true, matter, and can be acted on by someone who reads only the PR comment.

1. **Read all reports**, `tests.md` and the PR context. A build, lint or test failure that is new in this PR is a finding of its own (🐛 Bug or 🧪 Tests), even if no reviewer mentioned it.
2. **Verify each item** against the code. Open the file, run the test or command where it's cheap. An item you can't confirm is either dropped or kept as "Not verified", never presented as fact.
3. **Merge duplicates.** Two reviewers often find the same thing from different angles; keep one finding and note both sources.
4. **Rate the severity, and drop what doesn't matter.** Rate each finding by its effect on users and maintainers if the PR is merged as is:

   | Severity | Means |
   |---|---|
   | 🔴 Blocker | Merging breaks something users rely on, opens a gap the project's threat model covers, loses data, or makes a documented promise false. |
   | 🟠 Should fix | A real problem with a limited effect: a misleading message or dead end on a common path, risky logic without a test, docs that send users the wrong way. |
   | 🟡 Minor | Polish with a visible benefit: a clearer message on a rare path, a small doc gap, duplicated logic or tests, dead code, a comment or name that no longer matches the code. Fine as a follow-up. |

   Maintainability findings count: duplication, dead code and misleading comments or names make the next change harder or wrong. Rate them 🟡 Minor, or 🟠 Should fix when they are likely to mislead someone into a wrong change.

   Drop a finding instead of rating it when any of these holds:
   - It is a pure style preference (formatting, indentation, wording taste) that doesn't make the code harder to understand or change.
   - You can't name a concrete situation in which it goes wrong (who, when, what they see).
   - The fix would cost more than the problem.
   - It is 🟡 Minor and already exists on the base branch, unless it is a 🏕️ Cleanup in code the PR touches anyway.

   There is no quota: a review with one finding, or none, is a good result. Keep the comment short instead of dropping valid points: if there are more than three 🟡 Minor findings, group related ones into a single finding (for example "🏕️ Cleanup: small clean-ups" with one bullet per item, each naming the file and the fix). From round 2 on, the bar rises: keep a 🟡 Minor finding only if the previous round's fixes introduced it.
5. **Sort each remaining item:**
   - **Directly addressable**: the right fix is clear and doesn't change intended behavior or an interface decision.
   - **Needs a decision**: a reasonable person could choose differently (a default, the name of a new flag, "in this PR or a follow-up").
   - **Not verified**: plausible and worth checking, but you couldn't confirm it.
6. **Assign IDs** `F1`, `F2`, … in reading order: "Needs a decision" first, then "Directly addressable findings", then "Not verified"; within each section by severity, then by importance. IDs never repeat on a PR: in round 2 and later, continue after the highest ID of earlier rounds.
7. **Complete each finding** in the form given by `templates/pr-comment.md`, so it stands alone: the problem in plain words, where, the evidence as a code block, the suggested fix separate from the problem (naming any tests, docs or generated files that must change with it), and how to check it's done. For "Needs a decision", give the options with their trade-off and your suggestion. Give each finding a type from the "Finding types" table in the template, and its severity with a one-sentence reason.
8. **Annotate the reports.** In each reviewer item, put the outcome in front of its confidence line, e.g. `**Triage:** → F3 · **Confidence:** verified`. Outcomes: `→ F3`, `→ merged into F1`, `dropped: <one-line reason>` (for example "dropped: style preference"), or `unverified`. Leave the rest of the report as written, except to escape bare `<…>` text outside code (GitHub hides it).
9. **Round 2 and later:** for each earlier finding, decide whether it's addressed (name the commit), still open, or obsolete.
10. **Write the summary:** start with the verdict (✅ Ready to merge, ⚠️ Ready after the 🟠 findings, or ⛔ Not ready: 🔴 findings), then 2–3 sentences on what matters most, plainly, as the requester would say it.

Then fill in `templates/pr-comment.md`.
