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
The compiled-in `default` sandbox profile SHALL provide the equivalent of today's `tng-sandbox.json` for harness state paths — readwrite workdir; `filesystem.allow` for the harness state/cache paths (`~/.local/share/opencode`, `~/.local/state/opencode`, `~/.local/share/claude`, `~/.claude`) — but SHALL NOT broad-grant the host cache roots (`~/.cache`, `~/Library/Caches`) or the whole tool homes (`~/go`, `~/.cargo`). Toolchain runtime paths (`~/.cargo/bin`, `~/.rustup`, `~/go/bin`, `~/.nvm`, `~/.bun/bin`) SHALL be granted read-only so installed compilers stay runnable; `~/.rustup` is not a writable or cache grant. `~/.cargo/config`, `~/.cargo/credentials`, and the rest of `~/go` / `~/.cargo` SHALL NOT be reachable. The selected cache scope leaf (`~/.cache/omac/<sha256(scope)>` for persistent mode, or the per-launch temp cache for `--ephemeral-cache`) is granted read+write at launch time by the launcher (`--allow <scope>`), not by the profile. `filesystem.read` for config paths (`~/.config/agents/skills`, `~/.agents/skills`, `~/.gitconfig`, `~/.gitignore_global`); `network.listen_port: [4097]`; `network.allow_tcp_connect: [22]`; prompt enabled with 60 s timeout and `on_unavailable: deny`; `environment.allow_vars` unset (pass-through minus blocklist).

#### Scenario: Harness state persists
- **WHEN** the inner harness writes session state under `~/.local/share/opencode`
- **THEN** the write succeeds and the data is visible on the host after exit

#### Scenario: Host cache roots are denied
- **WHEN** the inner harness tries to write under `~/.cache` or `~/Library/Caches`
- **THEN** the write is denied; only the selected cache scope leaf (`OMAC_CACHE_DIR`) is writable

#### Scenario: Toolchain binaries readable, credentials denied
- **WHEN** the inner harness reads `~/.cargo/bin` or `~/.rustup`
- **THEN** the read succeeds (toolchain binaries stay runnable)
- **AND WHEN** the inner harness tries to read `~/.cargo/credentials` or write under `~/go`
- **THEN** the access is denied

### Requirement: Doctor checks for sandbox prerequisites
`omac doctor` SHALL verify the platform prerequisites of the built-in sandbox and report actionable failures: on Linux, that `bwrap` is installed and the kernel supports Landlock ABI ≥ 4; on macOS, that `sandbox-exec` is present; on both, whether an interactive dialog backend is available (warning only).

#### Scenario: Missing bwrap reported
- **WHEN** `omac doctor` runs on a Linux host without bubblewrap
- **THEN** the report flags the missing dependency with an install hint

#### Scenario: Headless host warning
- **WHEN** no dialog backend is available
- **THEN** doctor warns that network prompts will fall back to the `on_unavailable` policy

### Requirement: Tool-cache isolation
omac SHALL prepare a per-scope tool-cache directory it owns and redirect the caches supported by `XDG_CACHE_HOME`, `GOCACHE`, `GOMODCACHE`, `NPM_CONFIG_CACHE`, `PIP_CACHE_DIR`, and `CARGO_HOME` into it. The launcher SHALL inject `OMAC_CACHE_DIR` and `OMAC_CACHE_MODE` selector variables plus those six tool-specific redirects, each pointing under `OMAC_CACHE_DIR`. Unsupported third-party tools that hardcode another cache path SHALL require explicit profile configuration; omac SHALL NOT redirect them automatically. The selected cache scope leaf SHALL be granted read+write at launch time (`--allow <scope>`) and be the only writable cache location; the broad host cache roots and tool homes SHALL NOT be granted by the default profile. `omac sandbox run` SHALL re-derive the cache environment map only after verifying that `OMAC_CACHE_DIR` matches an exact writable grant in the active sandbox profile, so an inherited tool-specific variable cannot bypass the profile's environment allowlist. An external nono profile that sets `environment.allow_vars` SHALL include `OMAC_*`, `XDG_CACHE_HOME`, `GOCACHE`, `GOMODCACHE`, `NPM_CONFIG_CACHE`, `PIP_CACHE_DIR`, and `CARGO_HOME`; it does not receive the built-in sandbox re-exec's trusted reinjection, so omitting a redirect lets the affected tool fall back to its default cache location.

#### Scenario: Persistent cache scope reused across launches
- **WHEN** `omac start` is run twice from the same workdir
- **THEN** both launches use the same `~/.cache/omac/<sha256(v1:workdir:<canonical workdir>)>` directory and `OMAC_CACHE_MODE=persistent`

#### Scenario: Ephemeral cache removed on exit
- **WHEN** `omac start --ephemeral-cache` runs and the inner command exits
- **THEN** the per-launch cache directory under the sandbox temp dir has been removed and `OMAC_CACHE_MODE=ephemeral` was exposed to the inner process

#### Scenario: Worktree identity is the canonical path
- **WHEN** the main worktree and a linked worktree of the same repository are each launched with `omac start`
- **THEN** they use distinct cache scopes because their canonical absolute paths differ

#### Scenario: Cache env re-derived only for exact writable grants
- **WHEN** the sandbox re-exec inherits an `OMAC_CACHE_DIR` that is not an exact writable grant in the active profile
- **THEN** `omac sandbox run` refuses to re-inject the tool-cache environment and exits with an error

#### Scenario: No sandbox preserves transport and temporary runtime state
- **WHEN** `omac start --no-sandbox` or `omac serve --no-sandbox` runs
- **THEN** omac omits cache-scope creation and cache redirects while retaining normal `OMAC_*` transport variables and the per-launch `TMPDIR`

#### Scenario: Active scope not removable
- **WHEN** `omac cache clear --all` runs while a launch holds the shared lock on a persistent scope
- **THEN** that scope is reported `active` and left intact

#### Scenario: Inactive scope removed
- **WHEN** `omac cache clear --all` runs and a scope's exclusive lock is acquirable
- **THEN** that scope is reported `removed` and deleted from `~/.cache/omac`

### Requirement: Cache cleanup CLI
`omac cache clear` SHALL remove the current workdir's persistent cache scope. `omac cache clear --all` SHALL walk every scope under `~/.cache/omac` and remove the inactive ones, reporting each as `removed`, `active` (lock held by a running launch, left intact), or `skipped` (unsafe, missing, or replaced between open and remove). There SHALL be no interactive prompt; `--all` is the explicit destructive confirmation. Active scopes SHALL be refused/skipped: omac holds a shared lock on a persistent scope for the lifetime of the launch, and `--all` takes an exclusive lock per scope.

#### Scenario: Clear current workdir scope
- **WHEN** `omac cache clear` is run from workdir `/p`
- **THEN** the scope `~/.cache/omac/<sha256(v1:workdir:/p)>` is removed and reported `removed` (or `active`/`skipped` as above)

#### Scenario: Unsafe scope skipped
- **WHEN** a scope directory under `~/.cache/omac` is a symlink or is replaced between the open and the remove
- **THEN** `omac cache clear --all` reports it `skipped` and does not follow the symlink

### Requirement: Doctor sandbox-profile grant warnings
`omac doctor` SHALL inspect every `{{self}} sandbox run` command in the launcher config, resolve the referenced built-in sandbox profile read-only, and warn — without incrementing the failure count and without mutating the on-disk profile — when the profile re-introduces a broad read/write grant on the cache roots (`~/.cache`, `~/Library/Caches`), a write grant on the tool homes (`~/go`, `~/.cargo`, `~/.rustup`), or a whole-home `Read` that transitively covers `~/.cargo/config` and credentials. Doctor SHALL detect host Cargo sentinel presence for `~/.cargo/config`, `~/.cargo/config.toml`, `~/.cargo/credentials`, and `~/.cargo/credentials.toml` through `Lstat` only, without reading or copying contents, and warn that an isolated `CARGO_HOME` will not use them. The remediation SHALL require a project-local `.cargo/config.toml` and `CARGO_REGISTRIES_<NAME>_TOKEN`, with `<NAME>` uppercased and dashes changed to underscores, exported in the environment that starts omac; if `environment.allow_vars` is set, it SHALL include that exact token variable. Cargo SHALL receive the token through the sandboxed harness environment, not `sidecar.env_passthrough`.

#### Scenario: Broad cache-root grant warned
- **WHEN** a sandbox profile grants `Allow` on `~/.cache` or `~/Library/Caches`
- **THEN** doctor emits an advisory warning naming the grant and the impact, and does not rewrite the profile

#### Scenario: Cargo credentials presence warned
- **WHEN** `~/.cargo/credentials` exists on disk and the profile grants something under `~/.cargo`
- **THEN** doctor warns that an isolated `CARGO_HOME` will not pick it up, and never reads the file

### Requirement: OpenCode Desktop folder grants
`omac serve` SHALL accept a `--for-opencode-desktop` flag. When set, omac SHALL read the project worktrees recorded in the local OpenCode state and grant each existing worktree directory read+write in the sandbox, in addition to the profile's grants. All three OpenCode project stores SHALL be merged, since they drift apart: the JSON storage files under `~/.local/share/opencode/storage/project/`, the `opencode.db` SQLite `project.worktree` column, and the Desktop app's own store (`opencode.global.dat` in the `ai.opencode.desktop` / `ai.opencode.desktop.beta` application data directories) — folders opened in the Desktop UI may exist only in the latter. Within the Desktop store both the saved project list (key `globalSync.project`) and the directories of currently open tabs/windows (key `layout.page`, field `lastProjectSession`) SHALL be harvested, since an open tab is not necessarily a saved project. The pseudo-project with worktree `/` MUST be ignored. Worktrees nested inside another recorded worktree SHALL be collapsed into the ancestor (granting the parent already covers the child); path-prefix siblings (e.g. `/a/proj` and `/a/proj-2`) MUST NOT be collapsed. The list of granted worktrees SHALL be logged at startup.

#### Scenario: Desktop projects granted
- **WHEN** `omac serve --for-opencode-desktop` starts and the OpenCode state lists worktrees `/a` and `/b` (both existing) plus the global `/` record
- **THEN** the sandboxed harness can read and write `/a` and `/b`, and `/` is not granted

#### Scenario: Currently open Desktop tab granted
- **WHEN** a folder is open as a Desktop tab (present in `layout.page.lastProjectSession`) but is not in the saved `globalSync.project` list
- **THEN** that folder is still granted read+write

#### Scenario: Stale worktree skipped
- **WHEN** a recorded worktree no longer exists on disk
- **THEN** it is skipped with a notice and the launch proceeds

#### Scenario: Nested worktree collapsed
- **WHEN** the OpenCode state lists both `/a/proj` and `/a/proj/sub/module`
- **THEN** only `/a/proj` is granted (the subdirectory is covered by the parent), while an unrelated `/a/proj-2` remains granted separately

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
