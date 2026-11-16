# Reviewer report format

You are one of several reviewers looking at the same pull request, each from a different angle. Your report is read by the person who triages all reports. It is also posted, with a triage note per item, in a collapsed section of the PR comment, so write it for a reader who knows the project but not this PR.

## What you get

- Your angle's prompt (the file that sent you here).
- The PR's own diff, the PR description, linked issues, existing comments and reviews.
- The results of the project's tests, run at the PR head and on the base branch (`tests.md`), if they could be run.
- Read access to the repository at the PR head.

## Rules

- Read-only. No commits, no branch changes, no posting anything anywhere.
- Look beyond the diff when your angle needs it: callers, existing helpers, docs, tests.
- Don't take the PR description's claims for granted. For each change, look for the input, state or order of events that would break it, and report only what you can show.
- Run cheap checks (a single test, `grep`, a build) when they confirm or refute a finding. Say what you ran. The full test suite has already been run for you; use `tests.md` instead of running it again.
- Report what this PR changes or should have changed. Name it when a problem already exists on the base branch.
- Rank findings by how much they matter to someone using or maintaining the project.

## Report

Write Markdown in exactly this shape. It is rendered on GitHub, so keep the blank lines and the `####` headings (bigger headings dominate the comment).

```markdown
<one or two sentences of context, if needed; otherwise leave out>

#### <P>1. <one-line title>
**Confidence:** verified | plausible | unverified

- **Location:** `<file>:<line>` (`<function>`)
- **Evidence:** <what you saw or ran, and what it shows>
- **Suggested fix:** <the concrete change, including files that must follow>

#### <P>2. …
```

Make each item understandable without opening the code:

- **Evidence** explains the problem in 2–4 sentences: what happens, in which situation, who notices, and why it matters. Quote the relevant code, text or command output (a short fenced block is fine inside the list item, indented by two spaces). Say what you ran or read to confirm it.
- **Suggested fix** is concrete enough to implement: name the function or file to change, and show the new code or text when it's short.

`<P>` is your angle's prefix: `S` code style, `E` error handling, `D` documentation, `U` UX consistency, `X` security, `T` tests, `K` complexity, `P` performance.

- `verified`: you checked it in the code or by running something.
- `plausible`: it follows from what you read, but you didn't check it directly.
- `unverified`: a suspicion worth a second look.

Wrap code names, paths, commands and placeholders such as `<asset>` in backticks; GitHub hides bare `<…>` text as HTML.

If you found nothing worth reporting, the whole report is the line `No findings.`
