---
name: agent-review
description: Use when a pull request needs an in-depth review before merging, when someone asks to review a PR from several angles (code style, documentation, UX consistency, error handling), or when a review dashboard requests a review for a PR.
---

# Agent review

Review one pull request from four angles, verify what the reviewers found, and turn it into one PR comment with concrete, numbered suggestions that anyone (or anyone's agent) can act on.

## Inputs

| Input | Default |
|---|---|
| PR number and repository (`owner/name`) | ask if unclear |
| Requester: the GitHub login the comment names as having requested it | the person you work for |
| Round: 1 for a first review, higher for a re-review | 1 |
| Post the comment? | no: show the draft and ask |

## GitHub access

You need to: read a PR (metadata, description, files, commits, comments, reviews, inline review comments), read linked issues, and post **one top-level PR comment**. Use whatever the environment offers, for example:

- `gh pr view <N> -R <repo> --json title,body,baseRefName,headRefName,headRefOid,commits,files,comments,reviews,closingIssuesReferences` and `gh api repos/<repo>/pulls/<N>/comments`
- a sandbox broker such as the omac `omac-gh` skill (same `gh` arguments via its `/exec` endpoint)
- a GitHub MCP server

Never approve, merge, close, label or push. The only write is the one comment, and only if posting was requested.

**One exception for editing:** you may replace your own review comment of the **same round** (for example after the requester asked for changes to it), but only if it is the latest comment of the logged-in account on the PR. Check that first, then use `gh pr comment <N> -R <repo> --edit-last --body <text>`; it replaces exactly that comment. If the account has commented on the PR since the review, don't edit; ask instead. Never edit anyone else's comment, and never delete.

## Steps

1. **Prepare.**
   - Fetch the PR head: `git fetch origin +refs/pull/<N>/head:refs/remotes/pr/<N>` and the base branch.
   - Pick the diff base. If the PR's base branch is the head branch of another open PR, it is stacked: diff against that PR (`pr/<M>`). If that parent PR is gone or merged, use the default branch.
   - Save `git diff <base>...pr/<N>` and note the head SHA you reviewed.
   - Collect the description, linked issues, all comments and reviews. For round ≥2, find your earlier review comments on the PR and their `F` findings.
2. **Review.** Run the four reviewers in `reviewers/`: `code-style.md`, `documentation.md`, `ux-consistency.md`, `error-handling.md`. Each gets its prompt file, `reviewers/report-format.md`, the diff, the PR context, and read access to the repo at the PR head.
   - If your agent can start subagents, start all four at once, on a smaller, faster model if you can choose one (in Claude Code: `model: "sonnet"`).
   - Otherwise do the four passes yourself, one after another, keeping each report separate.
3. **Triage.** Follow `triage.md`. This step stays with you, not a subagent.
4. **Write the comment** from `templates/pr-comment.md`.
5. **Post or hand over.** If posting was requested, check first that the PR has no comment of yours for this round yet (search for the marker line and `Round <r>`), then post. Otherwise show the draft.
6. **Fixing** findings is a separate request. Follow `fixing.md`.

## Common mistakes

- Reviewing the whole stack instead of the PR's own change. Check the base branch first.
- Posting reviewer claims that nobody verified. Triage checks each one against the code.
- Findings that only make sense with the reports or this conversation at hand. Each finding in the comment must stand on its own.
- Praise, restating the diff, or "consider adding tests" without saying which test.
