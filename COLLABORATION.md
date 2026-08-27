# Collaboration Guide

## Work is tracked via issues

- Every change starts with a GitHub issue. **No issue, no PR.**
- One issue = one coherent piece of work. Split large efforts into
  multiple issues / small PRs rather than one big one.
- Link related work with `Refs #NN`; close it automatically with
  `Closes #NN` in the PR description.

## Assign yourself before starting

- Assign yourself to the issue the moment you begin work, so nobody
  else picks up the same thing.

## Never push to `main`

- `main` is protected. Never commit or push directly to it.
- Work on a small **feature/fix branch**:
  - `feat/<topic>` for new functionality
  - `fix/<topic>` for bug fixes
  - `docs/<topic>` for documentation
- Keep branches short-lived and focused. One concern per branch.
- At least one approval is required for merging to main.

---

## Issue rules

- **Title:** plain descriptive sentence or noun phrase framing the
  problem (e.g. `WSL: keychain registration fails with unhelpful D-Bus
  error`).
- Cite code with `path/to/file.go:NN` so it is jumpable.
- Use `- [ ]` checkboxes for actionable sub-items.
- Add an `Acceptance criteria` checklist for substantive work.
- Always include the *why* — an issue without motivation is untriageable.
- Labels are helpful and recommended (`bug`, `enhancement`,
  `security`, `documentation`, `agent-created`).

## Pull Request rules

- **Title:** Conventional Commits with scope — e.g.
  `fix(sandbox): protect docker.sock by default` or
  `feat(update): add omac self-update`. Types: `feat`, `fix`, `test`,
  `docs`, `chore`. Scope is a package/area; comma-separate cross-cutting
  scopes (`fix(sandbox,e2e):`).
- **Always link the issue:** `Closes #NN` (auto-closes on merge) or
  `Refs #NN` (reference only). Prefer `Closes` when the PR fully
  resolves the issue.
- **"What" summarizes at a glance** — do NOT restate what is clearly
  readable in the diff.
- **Verification is the most valued section** — show the actual
  commands you ran and their result. No claim of "done" without it.
- Signal agent authorship with a `🤖 Generated with ...` footer or
  `Co-Authored-By:` line when an agent wrote the change.
- Keep the body **concise** — structured bullets, not essays.

