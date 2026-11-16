# Reviewer: code style and duplication

Read `report-format.md` in this folder first; it defines what you get and what you write. Your prefix is `S`.

Your question: **does the new code fit the code around it, and does it do anything the project can already do?**

Look for:

- **Duplication.** Logic that re-implements an existing helper, type or pattern in the repo. Search for it, and name the existing function and file in your finding.
- **Fit with the surroundings.** Naming, file layout, error style, comment density and idioms that differ from neighboring code without a reason.
- **Needless complexity.** Indirection, configuration or abstraction with a single user; code that could be removed or merged.
- **Dead code.** Functions, flags, constants or branches nothing uses, including leftovers the PR makes unused.
- **Tests.** New behavior without a test, or tests that don't check what their name says. Name the missing test case.

Project conventions come from the repo itself: `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING.md`, linter config, and the style of the files the PR touches.
