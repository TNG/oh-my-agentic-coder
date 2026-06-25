## Why

Re-entering work in omac is clumsy today. To pick up where you left off you must launch the harness and navigate its own session UI, and there is no omac-level way to continue the last session or choose from recent ones. omac already launches sessions inside its sandbox via `omac start`, so two thin subcommands — `omac continue` and `omac resume` — close the gap by reusing that launch pipeline (sandbox + sidecars + facade) while letting the user re-enter prior work in one step.

## What Changes

- Add `omac continue [harness] [flags] [-- inner args...]`: re-launch the **last session for the current workdir** inside omac. It runs the full `omac start` pipeline and appends the harness's "continue last session" inner flag (opencode/claude: `--continue`).
- Add `omac resume [harness] [flags]`: list the **recent sessions for the current workdir**, present an interactive numbered picker (title + relative time), and launch the selected session inside omac (appending the harness's "resume specific session" inner flag — opencode `--session <id>`, claude `--resume <id>`). Non-interactive stdin falls back to printing the list with a hint.
- Support **both shipped harnesses** with working continue *and* resume. The two harnesses expose sessions differently, so omac uses a per-harness listing strategy: opencode via its `session list --format json` CLI; Claude Code by reading its session store (`~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`, title from the `aiTitle` record, directory confirmed via each file's embedded `cwd`).
- Register both subcommands in the dispatcher and top-level usage; add per-harness session metadata (continue flag, resume-by-id flag, and which listing strategy to use) to the harness registry so the commands are not opencode-hardcoded.

## Capabilities

### New Capabilities
- `session-continue`: The `omac continue` command — continue the most recent session for the current workdir inside the omac sandbox, delegating to the existing start pipeline with a harness-specific "continue" inner flag.
- `session-resume`: The `omac resume` command — list recent sessions scoped to the current workdir, render an interactive numbered picker, and launch the chosen session inside omac.

### Modified Capabilities
<!-- inner-harness is defined in the not-yet-archived support-claude-code-harness change; this change adds session metadata to that registry but does not alter its existing requirements, so no delta spec is needed here. -->

## Impact

- **Code (Go):** new `internal/cli/continue.go` and `internal/cli/resume.go`; `internal/cli/cli.go` (register `continue`/`resume`, usage text); refactor `internal/cli/start.go` so its launch pipeline is callable with caller-supplied extra inner args (extract the body of `runStart` into a shared `runLaunch`); a new session-listing package (or extension of `internal/opencodestate`) with two backends — opencode (parse `opencode session list --format json`) and Claude Code (read `~/.claude/projects/<encoded-cwd>/*.jsonl`) — both returning workdir-filtered, newest-first records.
- **Harness registry (`internal/config/harness.go`):** add per-harness session metadata (continue-flag argv, resume-by-id flag builder, and a listing-strategy selector). opencode and claude both populated; a nil session block means continue/resume report the harness doesn't support them.
- **Interactive UI:** reuse `internal/cli/style.go` color helpers and terminal detection for the numbered picker; respect non-TTY by degrading gracefully.
- **Docs:** `README.md` (new subcommands + usage), `oh-my-agentic-coder.md` CLI section.
- **Backward compatibility:** purely additive — new subcommands only; existing commands, layouts, and harness behavior are unchanged. Omitting the harness token defaults to `opencode` as elsewhere.

## Out of Scope / Future Work

- **Provenance coloring** (showing which sessions were previously run through omac vs. opened directly): deliberately excluded. omac has no authoritative signal for this — it would require either a best-effort session-list snapshot-diff (a heuristic that can misattribute) or, done properly, an opencode-plugin/bridge that tags sessions from inside the harness. Defer to a follow-up that takes the plugin-based approach if the hint proves wanted.
