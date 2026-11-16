# Fixing findings

Use this when someone asks to address findings from an agent review comment, e.g. "Please address F2, F5". You need only the PR and that comment; the original reviewer's reports or conversation are not required.

1. **Read the current state.** Fetch the PR's latest comments and commits. If a requested finding was already fixed, or someone said they're working on it, skip it and say so.
2. **Check you can deliver.** Find the PR's head branch and repository. If it comes from a fork that doesn't allow edits from maintainers, the fix can't go onto the PR branch: say so before doing the work, and ask whether to prepare a separate branch instead.
3. **Work in a separate working copy** based on the current PR head, on a local branch `review-fixes/<N>`, e.g. `git worktree add <dir> -b review-fixes/<N> pr/<N>`.
4. **One commit per finding.** Implement what the finding's "Suggested fix" says, including the tests, docs or generated files it names. A "Needs a decision" finding is only implemented when the request states the decision. Commit message: a normal message for the project, ending with `(F<n>)`, and a body line linking the review comment. If the project requires a DCO sign-off (a DCO check on its PRs, or its contributing docs ask for it), commit with `git commit -s` as the person who will push, e.g. `Signed-off-by: Jane Doe <jane@example.com>`.
5. **Check each fix** with the finding's "Done when" and the project's usual tests for the touched code. Report failures; don't hide them.
6. **Catch up with the PR.** Fetch the PR head again. If it moved, rebase `review-fixes/<N>` onto it. On conflicts, stop, abort the rebase, and report.
7. **Don't push.** Report the branch, the commits, the check results, and the command to push to the PR branch, e.g. `git push <remote> review-fixes/<N>:<pr-head-branch>`. Pushing is left to a person.
