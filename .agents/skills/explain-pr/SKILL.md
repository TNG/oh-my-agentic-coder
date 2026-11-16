---
name: explain-pr
description: Use when a pull request is complex enough that a reviewer needs to understand the feature from the ground up before reviewing it, or when someone asks for an explainer, deep dive or walkthrough page for a PR.
---

# Explain a PR

Write a page that explains one pull request from the ground up: what it changes, where that fits in the codebase, the usage scenarios that matter, the viable approaches, and how to review it. In Claude Code the page is published as a Claude Artifact; in other agents it is a standalone HTML file.

## Inputs

| Input | Default |
|---|---|
| PR number and repository (`owner/name`) | ask if unclear |
| Work directory for the page's files | a scratch or temp directory outside the repository, e.g. `<scratch>/explain-pr/<N>/` |

## GitHub access

You need to read the PR (description, files, commits, comments, reviews), its linked issues, and related open PRs. This skill never writes to GitHub. Use what the environment offers, for example:

- `gh pr view <N> -R <repo> --json title,body,baseRefName,headRefName,headRefOid,files,commits,comments,reviews,closingIssuesReferences`
- a sandbox broker such as the omac `omac-gh` skill (same `gh` arguments via its `/exec` endpoint)
- a GitHub MCP server

## Steps

1. **Fetch the code.** `git fetch origin +refs/pull/<N>/head:refs/remotes/pr/<N>` plus the base branch. If the PR's base branch is the head branch of another open PR, it is stacked: diff against that PR's head, not the default branch.
2. **Read.** Go through the sources in the "Where to look" table of `template/content.md`.
3. **Write** `meta.json` and `content.html` in the work directory, following `template/content.md`.
4. **Build.**
   - In Claude Code: `python3 scripts/build.py <work-dir>` (paths relative to this skill's directory).
   - In any other agent: `python3 scripts/build.py <work-dir> --standalone`.

   It writes `<work-dir>/explainer.html` and must print `"problems": []`. Fix and rebuild until it does.
5. **Deliver.**
   - In Claude Code: publish `<work-dir>/explainer.html` with the Artifact tool (copy it into the session's scratchpad first if the tool only accepts paths there). Pass a one-word `icon` such as `book` and a one-sentence `description`. To update an explainer that already exists, publish to its existing URL so the link stays the same. Give the user the link.
   - Elsewhere: copy the file to a name like `pr-<N>-explainer.html` where the user can open it, and give them the path. It needs network access to load Mermaid for the diagrams.

## When the user asks for changes

**Always ask first** whether the change is for this one explainer only, or for the template so that every future explainer gets it. Then edit the matching files:

| Scope | Edit |
|---|---|
| This explainer only | its `meta.json` / `content.html`, then rebuild and republish to the same URL |
| Look of every explainer (colors, layout, typography) | `template/page.html` |
| Structure or contents of every explainer (sections, what goes in them, building blocks) | `template/content.md`, and `scripts/build.py` if the checks must change |

After a template change, offer to rebuild the explainers that already exist so they match.

Example: the user says "the code in the title is hard to read". Ask: "Fix it in the template, so all explainers get it, or only on this page?"

## Common mistakes

- Diffing a stacked PR against the default branch, which mixes in the parent PR's changes.
- Claims about behavior that nobody read in the code or ran. Say on the page what wasn't verified.
- A "context" section that restates the diff. It should explain how this part of the system works today, for someone who has never touched it.
- Adding colors to Mermaid diagrams, which then break in dark mode.
