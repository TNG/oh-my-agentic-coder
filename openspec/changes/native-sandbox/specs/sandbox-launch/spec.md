# sandbox-launch

Integration of the built-in sandbox with the omac launcher (`omac start` / `omac serve`).

## ADDED Requirements

### Requirement: Built-in launcher profile is the default
The launcher SHALL provide a compiled-in launch profile named `builtin` that runs the inner harness under the built-in sandbox by re-executing the current omac binary:

```
{{self}} sandbox run --profile default --allow-file {{socket}} --read {{socket_dir}} {{tmpdir_flags}} --open-port {{tcp_port}} -- {{inner_cmd}} {{inner_args}}
```

`{{self}}` SHALL expand to the absolute path of the running omac executable. `builtin` SHALL be the default launch profile. The existing template mechanism and placeholders SHALL be preserved so users can configure external sandboxes (including the retained, non-default `nono` template and `no-sandbox-debug`).

#### Scenario: Default start uses built-in sandbox
- **WHEN** `omac start opencode` is run with no launcher configuration
- **THEN** the harness is launched via `omac sandbox run` with the bridge socket file, socket directory, tmpdir, and facade TCP port granted

#### Scenario: User overrides launcher
- **WHEN** the omac config selects the `nono` launch profile
- **THEN** the external `nono run ...` template is used unchanged

### Requirement: Bridge connectivity inside the sandbox
Under the default `builtin` profile, the inner harness MUST be able to reach the omac facade over both transports: the Unix domain socket (`bridge.sock`, granted via `--allow-file` and explicitly allowed in the macOS Seatbelt profile despite the network deny) and the loopback TCP port (granted via `--open-port`). All `OMAC_*` environment variables exported by the launcher MUST be visible to the inner harness (the default profile's env filtering passes `OMAC_*`).

#### Scenario: Unix socket transport on macOS
- **WHEN** the harness connects to `OMAC_SOCKET` from inside the sandbox on macOS with network filtering active
- **THEN** the connection succeeds

#### Scenario: TCP transport
- **WHEN** the harness sends a request to `OMAC_BASE` (`http://127.0.0.1:<tcp_port>/...`) from inside the sandbox
- **THEN** the request reaches the facade and the response streams back (including SSE without buffering)

### Requirement: Default sandbox profile content
The compiled-in `default` sandbox profile SHALL provide the equivalent of today's `tng-sandbox.json`: readwrite workdir; `filesystem.allow` for the harness state/cache paths (`~/.local/share/opencode`, `~/.local/state/opencode`, `~/.claude`, `~/.cache`, `~/Library/Caches`, `~/go`, `~/.rustup`, `~/.cargo`); `filesystem.read` for config paths (`~/.config/opencode`, `~/.opencode/bin`, `~/.nvm`, `~/.gitconfig`, `~/.gitignore_global`, `~/.claude.json`); `network.listen_port: [4097]`; `network.allow_tcp_connect: [22]`; prompt enabled with 60 s timeout and `on_unavailable: deny`; `environment.allow_vars` unset (pass-through minus blocklist).

#### Scenario: Harness state persists
- **WHEN** the inner harness writes session state under `~/.local/share/opencode`
- **THEN** the write succeeds and the data is visible on the host after exit

### Requirement: Doctor checks for sandbox prerequisites
`omac doctor` SHALL verify the platform prerequisites of the built-in sandbox and report actionable failures: on Linux, that `bwrap` is installed and the kernel supports Landlock ABI ≥ 4; on macOS, that `sandbox-exec` is present; on both, whether an interactive dialog backend is available (warning only).

#### Scenario: Missing bwrap reported
- **WHEN** `omac doctor` runs on a Linux host without bubblewrap
- **THEN** the report flags the missing dependency with an install hint

#### Scenario: Headless host warning
- **WHEN** no dialog backend is available
- **THEN** doctor warns that network prompts will fall back to the `on_unavailable` policy

### Requirement: OpenCode Desktop folder grants
`omac serve` SHALL accept a `--for-opencode-desktop` flag. When set, omac SHALL read the project worktrees recorded in the local OpenCode state (the `project` records under `~/.local/share/opencode/` — JSON storage files and/or the `opencode.db` SQLite `project.worktree` column) and grant each existing worktree directory read+write in the sandbox, in addition to the profile's grants. The pseudo-project with worktree `/` MUST be ignored. The list of granted worktrees SHALL be logged at startup.

#### Scenario: Desktop projects granted
- **WHEN** `omac serve --for-opencode-desktop` starts and the OpenCode state lists worktrees `/a` and `/b` (both existing) plus the global `/` record
- **THEN** the sandboxed harness can read and write `/a` and `/b`, and `/` is not granted

#### Scenario: Stale worktree skipped
- **WHEN** a recorded worktree no longer exists on disk
- **THEN** it is skipped with a notice and the launch proceeds

#### Scenario: Newly opened folder outside grants
- **WHEN** the desktop opens a folder that was not in the OpenCode state at launch time
- **THEN** the folder is not accessible in the running sandbox (kernel sandboxes cannot grow); omac surfaces a log line advising a restart (or learn mode)

### Requirement: Learn mode
`omac start` and `omac serve` SHALL accept a `--learn` flag. In learn mode the sandbox SHALL NOT restrict filesystem access (network filtering and env filtering remain active), and omac SHALL record every directory the inner process opens outside the already-granted set. At session end omac SHALL present the recorded folders and ask the user whether to append them to the active profile's `filesystem.allow` list; on confirmation the profile file is rewritten pretty-printed with the additions.

Folder recording SHALL aggregate to a sensible granularity (project/directory level, deduplicated, ancestors-collapse-descendants) and SHALL exclude paths already granted, baseline system paths, temp dirs, and protected paths (protected paths are never offered for allowlisting).

#### Scenario: Learn mode records and offers folders
- **WHEN** a learn-mode session reads files under `/Users/u/newproject` (not granted in the profile) and then exits
- **THEN** omac asks whether to add `/Users/u/newproject` to the profile, and on "yes" the profile's `filesystem.allow` gains that entry, pretty-printed

#### Scenario: Learn mode declines
- **WHEN** the user answers "no" at session end
- **THEN** the profile file is unchanged

#### Scenario: Protected paths never offered
- **WHEN** a learn-mode session touches `~/.ssh`
- **THEN** `~/.ssh` does not appear in the offered folder list

### Requirement: Human-readable output formatting
All JSON files omac writes (profiles, pages files, scaffolded defaults) SHALL be pretty-printed (2-space indent, trailing newline). Sandbox log lines (`~/.local/state/omac/sandbox.log` and stderr diagnostics) SHALL carry a timestamp and a level/category prefix in aligned columns so the log is scannable.

#### Scenario: Scaffolded profile is pretty-printed
- **WHEN** `default.json` is auto-created on first start
- **THEN** the file is indented JSON with a trailing newline, suitable for hand-editing

#### Scenario: Log line format
- **WHEN** the proxy denies a host
- **THEN** the log line contains a timestamp, a level, the `net` category, and the decision (e.g. `2026-06-11 10:42:01 INFO  net   DENY tracker.example:443 (deny_domain)`)
