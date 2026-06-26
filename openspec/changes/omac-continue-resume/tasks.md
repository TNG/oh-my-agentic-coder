## 1. Harness session metadata

- [x] 1.1 Add a `HarnessSession` struct + `SessionListKind` enum (`SessionListNone`/`SessionListOpenCodeCLI`/`SessionListClaudeFiles`) and a `Session *HarnessSession` field to `config.Harness` in `internal/config/harness.go` (`ContinueArgs`, `ResumeByIDArgs func(id string) []string`, `ListKind`).
- [x] 1.2 Populate `Session` for the `opencode` descriptor (`ContinueArgs: ["--continue"]`, `ResumeByIDArgs: id -> ["--session", id]`, `ListKind: SessionListOpenCodeCLI`).
- [x] 1.3 Populate `Session` for the `claude-code` descriptor (`ContinueArgs: ["--continue"]`, `ResumeByIDArgs: id -> ["--resume", id]`, `ListKind: SessionListClaudeFiles`).
- [x] 1.4 Add unit tests asserting each registered harness's session metadata (and that a nil `Session`/`SessionListNone` is handled).

## 2. Session listing

- [x] 2.1 Add a session package (extend `internal/opencodestate` or add `internal/sessions`) exposing `List(harness, workdir) ([]Session, error)` with `Session{ID, Title, When}`, dispatching on `ListKind` and returning workdir-filtered, newest-first records.
- [x] 2.2 Implement the `SessionListOpenCodeCLI` backend: run `opencode session list --format json`, parse the array, keep records whose `directory == workdir`.
- [x] 2.3 Implement the `SessionListClaudeFiles` backend: read `~/.claude/projects/<encoded-cwd>/*.jsonl`; id = filename, title = last `aiTitle` record (fallback first user message, then id), time = last record `timestamp` (fallback mtime); confirm membership via each file's embedded `cwd == workdir` (do not trust the decoded dir name).
- [x] 2.4 Make a missing harness/CLI, missing store, or parse failure yield an empty list + a typed "unsupported / unavailable" signal, never a hard error; skip malformed `.jsonl` lines.
- [x] 2.5 Unit-test both backends: opencode JSON parsing + workdir filter (fake command runner); claude `.jsonl` parsing, title/time fallbacks, `cwd` matching, and malformed-line tolerance (temp fixture dir); plus the unsupported-harness path.

## 3. Shared launch pipeline

- [x] 3.1 Extract the body of `runStart` (config → registry reconcile → sidecars → facade → control plane → sandbox exec) into a reusable `runLaunch(env, opts)` in `internal/cli/start.go`, where `opts` carries harness, start flags, and resolved extra inner args.
- [x] 3.2 Re-implement `runStart` as a thin parser that builds `opts` and calls `runLaunch`; confirm existing `start` tests still pass.

## 4. `omac continue`

- [x] 4.1 Add `internal/cli/continue.go` with `runContinue` that parses the optional harness token + the same surface flags as `start`, resolves the harness, and errors clearly if the harness has no `Session.ContinueArgs`.
- [x] 4.2 Build `opts` with `ContinueArgs` appended to inner args (preserving trailing `--` inner args) and call `runLaunch`.
- [x] 4.3 Register `continue` in `commands()` and add it to `printUsage` in `internal/cli/cli.go`.
- [x] 4.4 Add tests: continue appends the harness continue flag and preserves start flags + trailing `--` inner args.

## 5. `omac resume`

- [x] 5.1 Add `internal/cli/resume.go` with `runResume` that parses the optional harness token + flags, resolves the harness, and reports clearly when the harness has no listing strategy (`SessionListNone`).
- [x] 5.2 List + filter sessions to the current workdir, newest first; if none, print "no resumable sessions for this directory" and exit OK without launching.
- [x] 5.3 Render the picker: numbered entries with relative time + title, reusing `style.go`'s `styler`.
- [x] 5.4 Read and validate the selected index from stdin; on empty/EOF/cancel exit OK without launching; on non-TTY stdin print the list + hint and exit without blocking.
- [x] 5.5 On selection, build `opts` with `ResumeByIDArgs(id)` appended and call `runLaunch`.
- [x] 5.6 Register `resume` in `commands()` and add it to `printUsage`.
- [x] 5.7 Add tests: workdir filtering, empty-list message, index parsing/cancel, non-TTY fallback, and that selection appends the correct resume-by-id flag per harness (opencode `--session`, claude `--resume`).

## 6. Docs & verification

- [x] 6.1 Update `README.md` with `omac continue` / `omac resume` usage.
- [x] 6.2 Update the CLI section of `oh-my-agentic-coder.md` to document both subcommands.
- [x] 6.3 Run `go build ./...` and `go test ./...`; manually verify `omac continue` and `omac resume` against a real workdir for **both** harnesses — `omac resume` (opencode) and `omac resume claude` (interactive picker, launch).
