## Context

omac launches an inner harness (opencode/claude) inside a sandbox via `omac start`. The launch pipeline in `runStart` (`internal/cli/start.go`) does registry reconciliation, spawns sidecars + facade + control plane, builds the sandbox argv, and execs. Inner args after `--` are passed verbatim, so `omac start opencode -- --continue` already works mechanically — but it is undiscoverable and offers no session selection.

The two shipped harnesses expose sessions very differently:

- **OpenCode** (v1.17+) keeps sessions in `~/.local/share/opencode/opencode.db` (no per-session JSON), but `opencode session list --format json` emits `{id, title, updated, created, projectId, directory}` records, and `opencode --continue` / `opencode --session <id>` re-enter sessions.
- **Claude Code** has **no JSON session-list CLI**. It persists each session as `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl` (session id = filename UUID). A title is carried by an `ai-title` record (`{"type":"ai-title","aiTitle":"…"}`); each line also carries `cwd` and ISO `timestamp` fields. It re-enters sessions via `claude --continue` / `claude --resume <id>`.

The harness model in `internal/config/harness.go` already isolates all harness-specific knowledge as data on the `Harness` descriptor, so the listing difference is expressed as a per-harness strategy selector, not branching in the command code.

## Goals / Non-Goals

**Goals:**
- `omac continue` and `omac resume` as first-class subcommands that reuse the existing start pipeline unchanged in spirit.
- Resume lists only the current workdir's sessions, newest first, in an interactive numbered picker.
- Keep it harness-data-driven (opencode + claude), not opencode-hardcoded.

**Non-Goals:**
- Provenance coloring (omac-run vs. direct). Excluded — see the proposal's Out of Scope: omac has no authoritative signal short of a harness plugin, and the snapshot-diff alternative is a misattributing heuristic. Deferred to a plugin-based follow-up.
- A full-screen arrow-key TUI picker. A numbered prompt is sufficient and dependency-free.
- Session management beyond launch (no delete/rename/export — opencode already has those).
- Multi-directory `omac serve` integration (these are `start`-pipeline commands).

## Decisions

### 1. Extract a shared `runLaunch` from `runStart`

Refactor `runStart` so the post-parse pipeline (config load → registry reconcile → sidecars → facade → sandbox exec) lives in a `runLaunch(env, opts)` helper, where `opts` carries the parsed harness, start flags, and the resolved inner args. `runStart`, `runContinue`, and `runResume` all parse their own surface flags, decide the extra inner flags, then call `runLaunch`. This avoids duplicating the ~250-line pipeline and keeps a single source of truth for launch behavior.

Alternative considered: have `continue`/`resume` shell out to `omac start` by re-invoking the dispatcher. Rejected — it loses type-safe arg passing and doubles process setup.

### 2. Harness session metadata on the descriptor

Add an optional `Session *HarnessSession` field to `config.Harness`:

```
type HarnessSession struct {
    ContinueArgs   []string                  // e.g. ["--continue"]
    ResumeByIDArgs func(id string) []string   // opencode: ["--session", id]; claude: ["--resume", id]
    ListKind       SessionListKind           // SessionListOpenCodeCLI | SessionListClaudeFiles | SessionListNone
}
```

opencode sets `ListKind = SessionListOpenCodeCLI`; claude-code sets `ResumeByIDArgs` to `["--resume", id]` and `ListKind = SessionListClaudeFiles`. A nil `Session` (or `SessionListNone`) means resume reports the harness doesn't support listing. The descriptor stays pure data (an enum, not a func or I/O) so `config` keeps no filesystem/CLI dependency — matching the existing `ServerLaunch`/`InnerCmd` pattern. The actual listing logic lives in the session package (Decision 3), keyed on `ListKind`.

### 3. Per-harness session listing in a dedicated package

A new package (extend `internal/opencodestate` or add `internal/sessions`) exposes `List(harness, workdir) ([]Session, error)` returning workdir-filtered, newest-first records (`{ID, Title, When}`). It switches on the harness's `ListKind`, and runs **outside** the sandbox (omac orchestration, like the existing `opencodestate` reads):

- **`SessionListOpenCodeCLI`** — run `opencode session list --format json`, parse the JSON array, keep records whose `directory == workdir`. No SQLite driver / `sqlite3` binary dependency; correct across opencode storage-format changes.
- **`SessionListClaudeFiles`** — locate the project dir under `~/.claude/projects/`. The encoded name is derived from the workdir (Claude replaces path separators and `.` with `-`), but because that encoding is lossy/ambiguous for paths containing literal `-`/`.`, omac treats each candidate file's embedded `cwd` as the source of truth: a session belongs to the workdir iff its `cwd == workdir`. For each `*.jsonl`: id = filename sans extension; title = the last `ai-title` record's `aiTitle` (fallback: first user message text, truncated; final fallback: the id); timestamp = the last record's `timestamp` (fallback: file mtime). Reading is best-effort and line-oriented — a malformed line is skipped, never fatal.

Both backends yield the same `Session` shape, so the picker and launch code are harness-agnostic.

### 4. Picker UI: numbered prompt reusing `style.go`

Render a numbered list (index, title, relative time) then read a line from stdin and parse the index, reusing the existing `styler` (which already disables color for non-TTY/`NO_COLOR`). TTY detection uses `golang.org/x/term` as `style.go` already does. Non-TTY stdin prints the list + hint and exits without blocking. Empty input / EOF / Ctrl-C = cancel (exit OK, no launch).

## Risks / Trade-offs

- **`opencode session list` latency/availability** → Adds a subprocess call to opencode resume. Mitigation: only resume pays it, interactively; a missing harness/CLI yields an empty list, not an error.
- **Claude `.jsonl` format drift** → The session store is an internal Claude Code format that could change (`ai-title` shape, line schema). Mitigation: parse defensively (skip unknown/malformed lines; layered title fallbacks; mtime fallback for time); listing failure degrades to "no resumable sessions", never a crash.
- **Claude project-dir encoding is ambiguous** → Reverse-decoding `<encoded-cwd>` is lossy for paths with literal `-`/`.`. Mitigation: never trust the decoded name — match on each file's embedded `cwd` field as the authoritative directory.
- **`runStart` refactor regressions** → Extracting `runLaunch` touches a large, well-tested function. Mitigation: keep `runStart`'s observable behavior identical and rely on the existing `start` test suite.

## Migration Plan

Purely additive: new subcommands and one new nil-safe `Harness.Session` field. No existing behavior changes; no state on disk; rollback is reverting the commit.

## Open Questions

- Should `omac resume` accept a non-interactive `--session <id>` / `-n <index>` shortcut for scripting? (Not required by the proposal; easy to add later.)
