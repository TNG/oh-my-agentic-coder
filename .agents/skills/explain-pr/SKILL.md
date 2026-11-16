---
name: explain-pr
description: Use when a pull request is complex enough that a reviewer needs to understand the feature from the ground up before reviewing it, or when someone asks for an explainer, deep dive or walkthrough page for a PR.
---

# Explain a PR

Write a page that explains one pull request from the ground up: what it changes, where that fits in the codebase, the usage scenarios that matter, the viable approaches, and how to review it. The page is one HTML file filled in from `template/page.html`: in Claude Code it is published as a Claude Artifact, in other agents the user opens the file in a browser.

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
3. **Write.** Copy `template/page.html` (relative to this skill's directory) to `<work-dir>/pr-<N>-explainer.html` and replace every `{{…}}` placeholder, following `template/content.md`.
4. **Check** what you wrote:
   - The five sections are there in order (`what`, `context`, `scenarios`, `approaches`, `review`), and the table of contents has one entry per section with the same id and heading.
   - Text taken from the PR (titles, descriptions, code, comments) has `&`, `<` and `>` escaped.
   - You added no `<script>`, `<style>`, `<link>`, `<iframe>` or `<img>`, no `on…=` attributes and no `javascript:` links.
   - Every tag you opened is closed.
   - No `{{` is left in the file.
5. **Deliver.**
   - In Claude Code: publish the file with the Artifact tool (copy it into the session's scratchpad first if the tool only accepts paths there). Pass a one-word `icon` such as `book` and a one-sentence `description`. To update an explainer that already exists, publish to its existing URL so the link stays the same. Give the user the link.
   - Elsewhere: put the file where the user can open it and give them the path.

   The page loads Mermaid from cdn.jsdelivr.net, so diagrams need network access; without it, the reader sees the diagram's source.

## When the user asks for changes

- If the request is about this page, edit the page and republish it to the same URL.
- If it is clearly about all future explainers, edit `template/page.html` (look) or `template/content.md` (structure and contents), and offer to update the explainers that already exist.
- Ask only when it's unclear which of the two the user means.

## Common mistakes

- Diffing a stacked PR against the default branch, which mixes in the parent PR's changes.
- Claims about behavior that nobody read in the code or ran. Say on the page what wasn't verified.
- A "context" section that restates the diff. It should explain how this part of the system works today, for someone who has never touched it.
- Adding colors to Mermaid diagrams, which then break in dark mode.
