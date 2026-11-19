# PR comment template

Fill in the parts in `<…>`. Leave out a section that would be empty. Style reference: a direct, specific review where each point has evidence and a concrete suggestion, such as the review by JonathanRohland-TNG on https://github.com/TNG/oh-my-agentic-coder/pull/388.

## Layout

```markdown
> 🤖 **Agent-generated review** (Round <r>, head `<short sha>`), requested by @<requester>.
> Findings have IDs (F1, F2, …). To get some addressed, reply e.g. "Please address F2, F5" and point an agent at this comment.

<verdict>. <2–3 sentence summary>

<Round ≥2 only:>
### Earlier findings
| ID | Status |
|---|---|
| F1 | addressed in <sha> |
| F2 | still open |

### Needs a decision
<findings, decision form>

### Directly addressable findings
<findings, directly addressable form>

### Not verified
<findings, not verified form>

### Reviewer reports

_Background only: everything relevant from these reports is already in the findings above. Each item notes where it went (`→ F3`, `merged into …`, `dropped: …`), so there is no need to review them again._

<one line on the reviewers that ran and the ones skipped, with the reason, and one line on the tests, e.g.:>
Reviewers: code style, error handling, tests, security. Skipped: documentation and UX (no docs or user-facing changes), complexity (small, local change), performance (no hot path touched).
Tests: build, vet and the touched packages' tests pass; 2 failures also happen on the base branch.

<one details block per reviewer that ran, in this order and with these titles: "Code style &amp; duplication report", "Error handling report", "Tests report", "Documentation report", "UX consistency report", "Security report", "Complexity report", "Performance report">

<details><summary>Code style &amp; duplication report</summary>

<annotated report>

</details>
<details><summary>Security report</summary>

<annotated report>

</details>
```

## Finding forms

Visible above each `<details>`: the heading and the problem, in 1–2 plain sentences without code blocks. That is enough to decide whether a finding matters. Everything needed to act on it goes inside `<details>`. Each labeled part is its own paragraph; keep the blank line after `<summary>` and before `</details>` so GitHub renders the Markdown inside.

## Finding types

Every finding heading starts with its type: the emoji and the word, e.g. `#### F2. 🐛 Bug: <title>`. Pick the one that best describes the effect on users or maintainers:

| Type | Use for |
|---|---|
| 🐛 Bug | Wrong behavior, crashes, silent failures, half-done state |
| 🔒 Security | Weakens the sandbox, exposes secrets, or bypasses an approval |
| 💬 UX | User-facing texts, flags, prompts, defaults; dead ends and unhelpful errors |
| 📝 Docs | Docs, help texts or code comments that are wrong, missing or unclear |
| 🧪 Tests | Missing or ineffective tests |
| 🧹 Code | Maintainability problems the PR should fix: needless complexity, re-implementing an existing helper, structure that doesn't fit |
| 🏕️ Cleanup | Optional tidy-ups that leave the code cleaner than before (boy scout rule): dead code, stale comments, duplicated tests or fixtures. The PR is fine without them; always 🟡 Minor |
| ⚡ Performance | Measurable slowness or resource use |

## Severity

Right below each heading, before the problem text, give the severity and why, in one sentence. The scale and the rules for dropping findings are in `triage.md`.

```markdown
**Severity:** 🟠 Should fix. Every Desktop user sees this message on each start, and it points them to the wrong setting.
```

The summary starts with the verdict: `✅ Ready to merge` (only 🟡 findings or none), `⚠️ Ready after the 🟠 findings`, or `⛔ Not ready: 🔴 findings`.

## User-Impact

Add this block below the severity line, after a blank line, when the finding changes what someone using omac experiences: a command fails or behaves differently, a message misleads, a documented setup stops working, or the agent can do more than the docs promise. Leave it out when only maintainers would notice, as with missing tests, internal docs, code structure or clean-ups.

```markdown
**User-Impact**
- **Who:** <which users, described by their setup or what they do>
- **What happens:** <what they see or lose, and the workaround if there is one>
- **Reach:** <Everyone | Common setup | Specific setup | Rare>. <why, in one sentence>
```

| Reach | Means |
|---|---|
| Everyone | every user of the feature, in the normal flow |
| Common setup | a setup or step many users of the feature have, e.g. starting `omac serve` from the home folder |
| Specific setup | needs two or more conditions that each occur, but rarely together |
| Rare | needs an unusual environment or timing, e.g. no `HOME` variable, or a race at startup |

Describe users by what they do, not by the code's terms. For example: "people whose `~/.config` is a symlink into a folder they opened as a project in OpenCode Desktop", not "projects that cover a store route".

## Finding, directly addressable form

````markdown
#### F2. <type>: <title> · <new in this PR | already on <base branch>>
**Severity:** <🔴 Blocker | 🟠 Should fix | 🟡 Minor>. <why, in one sentence>

<User-Impact block, only if users are affected (see "User-Impact")>

<what goes wrong, for whom, in plain words>

<details><summary>Details and suggested fix</summary>

**Where:** `<file>:<line>` (`<function>`)<, more locations>

**Evidence:**
```<lang>
<the command and its output, or the few lines of code that show the problem>
```

**Suggested fix:** <the change in one or two sentences; if tests, docs or generated files must change with it, name them in a closing sentence>
```<lang or diff>
<the code change, when a snippet makes it clearer>
```

**Done when:** <test or check that proves it>

</details>
````

## Finding, decision form

Options and suggestion stay visible, since they need the reader's attention.

````markdown
#### F1. <type>: <title> · <new in this PR | already on <base branch>>
**Severity:** <🔴 Blocker | 🟠 Should fix | 🟡 Minor>. <why, in one sentence>

<User-Impact block, only if users are affected (see "User-Impact")>

<what's at stake, in plain words>

**Options:**
- **A. <name>:** <what it means>. <trade-off>
- **B. <name>:** <what it means>. <trade-off>

**Suggested:** <A>, because <reason>.

<details><summary>Details</summary>

**Where:** `<file>:<line>` (`<function>`)

**Evidence:**
```<lang>
<…>
```

</details>
````

## Finding, not verified form

````markdown
#### F8. <type>: <title> · <new in this PR | already on <base branch>>
**Severity:** <🔴 Blocker | 🟠 Should fix | 🟡 Minor>. <why, in one sentence>

<User-Impact block, only if users are affected (see "User-Impact")>

<why it looks like a problem>

<details><summary>How to confirm</summary>

<what would prove or disprove it, and what to do if confirmed>

</details>
````

## Rules

- **Evidence** shows only what was actually read or run. An example scenario is written as prose and labeled as an example, never formatted as command output.
- Inline code only for short identifiers (`ExitIOError`, `--root`). Commands, outputs, multi-line code and anything with arguments go into a fenced block under **Evidence** or **Suggested fix**.
- No praise section and no restating of the diff.
- Mark findings that belong in a follow-up issue as such in **Suggested fix**.
- GitHub limits a comment to 65,536 characters. If the comment is longer, shorten the reports inside `<details>`, longest first: keep each item's heading, triage line and `Suggested fix:`, drop the rest, and end the report with `_(shortened to fit GitHub's comment limit)_`. Never shorten the findings above the reports.
- Before posting, check that the PR has no comment with this marker and the same round. If it has, don't post again; to change it, follow the editing exception in `SKILL.md`.
