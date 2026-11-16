---
name: agent-review
description: Use when a pull request needs an in-depth review before merging, when someone asks to review a PR from several angles (code style, documentation, UX consistency, error handling, security, tests, complexity, performance), or when a review dashboard requests a review for a PR.
---

# Agent review

Review one pull request with the reviewers that fit it, verify what they found, and turn it into one PR comment with concrete, numbered suggestions that anyone (or anyone's agent) can act on.

## Inputs

| Input | Default |
|---|---|
| PR number and repository (`owner/name`) | ask if unclear |
| Requester: the GitHub login the comment names as having requested it | the person you work for |
| Round: 1 for a first review, higher for a re-review | 1 |
| Reviewers to add or skip | none: pick them as described in step 3 |
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
2. **Run the tests once**, if the PR changes code. Find the project's test commands (`AGENTS.md`, `CONTRIBUTING.md`, testing docs, `Makefile`, CI config) and run the build, the linters CI runs, and the tests of the packages or modules the PR touches, at the PR head and on the diff base. Write `tests.md` with the commands, and the failures sorted into "new in this PR" and "also failing on the base". Keep it to the touched code if the full suite is slow; say so in `tests.md`. If nothing can run here, write that and why.
3. **Pick the reviewers.** Each reviewer in `reviewers/` answers one question. Pick the ones whose condition the PR meets, from the changed files, the diff and the description:

   | Reviewer | Prefix | Runs when the PR … |
   |---|---|---|
   | `code-style.md` | S | changes code |
   | `error-handling.md` | E | changes code |
   | `tests.md` | T | adds or changes code paths, or tests |
   | `documentation.md` | D | changes docs or comments, or behavior, flags, config, defaults or help texts that docs describe |
   | `ux-consistency.md` | U | changes anything users see: messages, prompts, flags, config keys, endpoints, exit codes, file locations |
   | `security.md` | X | touches a trust boundary: sandboxing, filesystem or network grants, environment filtering, credentials and secrets, approvals and registration, control-plane or API endpoints, parsing data an attacker can write, updates or installs |
   | `complexity.md` | K | adds abstractions, flags, settings, dependencies or new files, or is large compared with the problem it solves |
   | `performance.md` | P | changes code that runs per request, per file, at every start, or in loops over external data |

   A docs-only PR usually gets D and U; a typical code change gets S, E, T and whatever else applies. When unsure, include the reviewer. Apply the requester's additions and removals. Note one line per reviewer you skipped and why; it goes into the comment.
4. **Review.** Each selected reviewer gets its prompt file, `reviewers/report-format.md`, the diff, the PR context, `tests.md`, and read access to the repo at the PR head.
   - If your agent can start subagents, start all selected reviewers at once, on a smaller, faster model if you can choose one (in Claude Code: `model: "sonnet"`).
   - Otherwise do the passes yourself, one after another, keeping each report separate.
5. **Triage.** Follow `triage.md`. This step stays with you, not a subagent.
6. **Write the comment** from `templates/pr-comment.md`.
7. **Post or hand over.** If posting was requested, check first that the PR has no comment of yours for this round yet (search for the marker line and `Round <r>`), then post. Otherwise show the draft.
8. **Fixing** findings is a separate request. Follow `fixing.md`.

## Common mistakes

- Reviewing the whole stack instead of the PR's own change. Check the base branch first.
- Posting reviewer claims that nobody verified. Triage checks each one against the code.
- Reporting test failures that also happen on the base branch as problems of the PR.
- Findings that only make sense with the reports or this conversation at hand. Each finding in the comment must stand on its own.
- Praise, restating the diff, or "consider adding tests" without saying which test.
