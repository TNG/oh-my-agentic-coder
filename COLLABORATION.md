# Collaboration Guide

This repo is worked on by humans and agents together. The rules below
apply to **everyone** — please follow them.

## 1. Work is tracked via issues

- Every change starts with a GitHub issue. **No issue, no PR.**
- One issue = one coherent piece of work. Split large efforts into
  multiple issues / small PRs rather than one big one.
- Link related work with `Refs #NN`; close it automatically with
  `Closes #NN` in the PR description.

## 2. Assign yourself before starting

- Assign yourself to the issue the moment you begin work, so nobody
  else picks up the same thing.
- If an issue is unassigned and you start on it, assign yourself
  first — even mid-flight.

## 3. Never push to `main`

- `main` is protected. Never commit or push directly to it.
- Work on a small **feature/fix branch**:
  - `feat/<topic>` for new functionality
  - `fix/<topic>` for bug fixes
  - `docs/<topic>` for documentation
- Keep branches short-lived and focused. One concern per branch.

## 4. Reviews

- Open a PR as soon as the work is reviewable; mark it **Draft** if it
  isn't ready.
- **Request reviewers by name** when you want specific eyes on it.
- As a reviewer, finish a review with the **Approve** or **Request
  changes** button — a bare comment does not signal a decision.
- After you (the author) implement review feedback, **re-request
  review** so reviewers know it is ready again.
- **Never merge without at least one approval.**
- **Resolve review comments as you address them.** When you resolve a
  thread, a reply is optional if you implemented the suggestion as
  discussed; if you deviated from the suggestion, reply with a short
  note explaining the deviation *before* resolving.

## 5. Issues & MRs are concise and agent-first

Both issues and PRs are written so a human *or* an agent can pick them
up and act. Keep them **concise** — prefer structured bullets over
prose walls. See the spec below for the expected structure.

The **issue/PR body is the living source of truth.** As understanding
evolves, update the body rather than burying the current state deep in
a comment thread (GitHub preserves the edit history, so nothing is
lost). Use the comment thread for discussion; resolve comments and fold
the outcome back into the body.

---

## Spec: Issue structure

A good issue is self-contained and locatable. Use these sections (omit
a section if it adds nothing):

```
## Context / Summary      Why this issue exists; origin (prior PR/issue refs)
## Problem / What         The gap, with file:line references and evidence
## Suggested fix / Ask    Proposed direction; checklist of sub-items (- [ ])
## Non-goals              Explicitly out of scope — prevents scope creep
## Evidence / Environment repro commands, CI links, logs (when relevant)
```

Rules:
- **Title:** plain descriptive sentence or noun phrase framing the
  problem (e.g. `WSL: keychain registration fails with unhelpful D-Bus
  error`). No conventional-commit prefix on issues.
- Cite code with `path/to/file.go:NN` so it is jumpable.
- Use `- [ ]` checkboxes for actionable sub-items.
- Add an `Acceptance criteria` checklist for substantive work.
- Always include the *why* — an issue without motivation is untriageable.
- Labels are helpful and recommended (`bug`, `enhancement`,
  `security`, `documentation`, `agent-created`).

## Spec: Pull Request structure

PRs mirror the conventions already established in this repo:

```
## What            1-4 bullets of what changed
## Why             Motivation — failing CI run, security gap, related issue
## How             Implementation approach, per bullet with code refs if useful
## Verification    Commands run (go build / go test / ...) with pass status
## Follow-up       Optional next steps or linked issues
```

Rules:
- **Title:** Conventional Commits with scope — e.g.
  `fix(sandbox): protect docker.sock by default` or
  `feat(update): add omac self-update`. Types: `feat`, `fix`, `test`,
  `docs`, `chore`. Scope is a package/area; comma-separate cross-cutting
  scopes (`fix(sandbox,e2e):`).
- **Always link the issue:** `Closes #NN` (auto-closes on merge) or
  `Refs #NN` (reference only). Prefer `Closes` when the PR fully
  resolves the issue.
- **Verification is the most valued section** — show the actual
  commands you ran and their result. No claim of "done" without it.
- Signal agent authorship with a `🤖 Generated with ...` footer or
  `Co-Authored-By:` line when an agent wrote the change.
- Keep the body **concise** — structured bullets, not essays.

---

## Specs

Workflows like the superpowers/plans skills produce spec or plan
artifacts before implementation. We keep it simple:

- **Do not commit spec artifacts.** Keep specs inline in the issue body.
- When a spec encodes an architecture or pattern decision worth
  persisting, fold it into the committed docs (`docs/`, `AGENTS.md`,
  or an ADR/BDR) rather than maintaining a `specs/` tree.

This avoids a parallel "spec for specs" convention and keeps decisions
where they are actually read.

---

*Tracked in #105.*
