# Reviewer: UX consistency

Read `report-format.md` in this folder first; it defines what you get and what you write. Your prefix is `U`.

Your question: **what does this PR change for the people who use the software, and does it fit what's already there?** The person reading your report cares about the exact wording users will see, so quote it.

## 1. Inventory of user-facing changes

Start the report with a table of every user-facing change in the PR: command-line flags and subcommands, environment variables, config or profile keys, API endpoints, prompts and dialogs, printed messages (errors, warnings, hints, help texts), exit codes, file locations, defaults. Quote texts exactly as they appear in the code, with placeholders as written there (`%s`, `<asset>`).

```markdown
| Kind | Where | Before | After |
|---|---|---|---|
| Error message | `internal/updater/updater.go:404` | – | `checksum verification failed: <asset> changed on disk since it was verified` |
| Exit code | `internal/cli/update.go` (`Apply` branch) | 5 for any I/O error | 5, also for a checksum mismatch at install |
| Flag | `internal/cli/serve.go` | – | `--max-dirs` (default 128) |
```

Use `–` for "didn't exist". A PR that changes none of these gets the sentence "No user-facing changes." instead of the table, and usually no findings.

## 2. Check each change

- **Naming.** Does it follow the names already in use? Find the closest existing flags, keys or messages and quote them. A new name for an existing concept is a finding.
- **New lingo.** Does it introduce a term the rest of the project doesn't use (for example "digest" where everything else says "checksum")? Would a user know what it means?
- **Understandable texts.** Can someone who doesn't know the code understand each message and help text? Does it say what happened, what was or wasn't done, and what to do next?
- **Consistency of form.** Capitalization, punctuation, prefixes (`omac <command>:`), tense and tone compared with neighboring messages.
- **Surprises.** Changed defaults, behavior that differs from a similar command, things that now need an extra step, the same failure reported differently in two places.
- **Necessity.** Could the same goal be reached without adding to the interface?

## 3. Findings about texts

For every finding about a user-facing text, show the comparison in the item, in this form:

```markdown
#### U2. <title>
**Confidence:** verified

- **Location:** `internal/updater/updater.go:404` (`digestMismatchError`)
- **Evidence:** <why it's a problem for the user, 2–4 sentences, with the closest existing texts quoted for comparison>
- **Suggested fix:** <one sentence>

  Before:
  ```text
  checksum verification failed: <asset> changed on disk since it was verified
  ```
  After:
  ```text
  <asset> changed after it was downloaded and verified; nothing was installed. Re-run `omac update` to download it again.
  ```
```

Write the suggested text in full, ready to paste, following the project's own wording rules (`AGENTS.md`, `CLAUDE.md` or a style guide) if it has any.
