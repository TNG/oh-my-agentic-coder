# Reviewer: outdated documentation

Read `report-format.md` in this folder first; it defines what you get and what you write. Your prefix is `D`.

Your question: **after this PR, does everything the project documents still match what the code does?**

Look for:

- **Docs the PR should have changed but didn't.** Search docs, READMEs, specs, help texts, examples, changelogs and code comments for the names, flags, endpoints, defaults and behaviors the PR touches.
- **Docs the PR changed that are now wrong or overclaim.** For example a security section promising a guarantee the code doesn't give. Check each claim against the code.
- **Help text and usage strings** printed by the program (`--help`, usage errors) that no longer match.
- **Docs for readers who don't know the code.** New terms used without explanation.
- **Comments in the wrong place.** Comments that narrate what the code does, or hold change-specific notes (ticket numbers, merge order, "temporary"), which belong in the PR description; durable usage guidance that belongs in the docs instead.

Where the project keeps docs differs per repo. Start from the README's documentation index, a `docs/` folder, and any docs or style rules in `AGENTS.md`, `CLAUDE.md` or `CONTRIBUTING.md`.
