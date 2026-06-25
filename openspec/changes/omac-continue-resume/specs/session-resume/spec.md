## ADDED Requirements

### Requirement: Resume subcommand lists workdir sessions

omac SHALL provide an `omac resume [harness] [flags]` subcommand that lists recent sessions scoped to the current workdir and lets the user select one to launch inside omac. Sessions SHALL be obtained using the resolved harness's listing strategy and filtered to those whose recorded directory equals the current workdir. The list SHALL be ordered most-recently-updated first. Each listed session SHALL carry an id, a human-readable title, and a timestamp.

#### Scenario: Only the current workdir's sessions are shown

- **WHEN** the user runs `omac resume` in workdir `/a` and the harness has sessions for `/a` and `/b`
- **THEN** the picker SHALL list only the sessions whose directory is `/a`

#### Scenario: OpenCode sessions are listed via its CLI

- **WHEN** the resolved harness is opencode
- **THEN** omac SHALL obtain sessions by parsing `opencode session list --format json` and matching each record's `directory` against the workdir

#### Scenario: Claude Code sessions are listed from its session store

- **WHEN** the resolved harness is claude-code
- **THEN** omac SHALL obtain sessions by reading the per-project session files under `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`, deriving the id from the filename, the title from the session's `aiTitle` record (falling back to the first user message), and the timestamp from the latest record (falling back to file mtime), and confirming the directory via each file's embedded `cwd`

#### Scenario: No sessions for this workdir

- **WHEN** the user runs `omac resume` and there are no sessions for the current workdir
- **THEN** omac SHALL print a message stating there are no resumable sessions for this directory and SHALL exit without launching the harness

#### Scenario: Harness without a listing strategy

- **WHEN** the resolved harness declares no session-listing strategy
- **THEN** omac SHALL print that resume listing is unsupported for that harness and suggest `omac continue`, and SHALL NOT error obscurely

### Requirement: Interactive picker

When stdin and stdout are a TTY, `omac resume` SHALL present an interactive picker of the listed sessions, each entry showing at least an index, the session title, and a relative timestamp. Selecting an entry SHALL launch that session inside omac using the existing `omac start` pipeline with the harness's "resume specific session" inner flag (opencode `--session <id>`, claude-code `--resume <id>`). Cancelling the picker SHALL exit without launching.

#### Scenario: Selecting an opencode session launches it in omac

- **WHEN** the resolved harness is opencode and the user selects entry N for session `ses_X`
- **THEN** omac SHALL run the start pipeline for opencode and SHALL append `--session ses_X` to the inner command

#### Scenario: Selecting a Claude Code session launches it in omac

- **WHEN** the resolved harness is claude-code and the user selects entry N for session `<uuid>`
- **THEN** omac SHALL run the start pipeline for claude-code and SHALL append `--resume <uuid>` to the inner command

#### Scenario: Cancelling the picker

- **WHEN** the user cancels the picker (e.g. Ctrl-C or empty selection)
- **THEN** omac SHALL exit without launching any session and SHALL return a non-error (cancelled) exit code

#### Scenario: Non-interactive stdin

- **WHEN** stdin is not a TTY
- **THEN** omac SHALL print the numbered session list and a hint that selection requires an interactive terminal, and SHALL NOT block waiting for input
