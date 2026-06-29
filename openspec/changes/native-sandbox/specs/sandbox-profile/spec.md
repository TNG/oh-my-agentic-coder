# sandbox-profile

Sandbox configuration profile format and the `omac sandbox run` CLI surface.

## ADDED Requirements

### Requirement: Profile file format
The sandbox SHALL be configured by a JSON profile with the top-level sections `meta`, `workdir`, `filesystem`, `network`, and `environment`. All sections SHALL be optional. The parser MUST reject unknown fields with an error naming the offending field.

The supported fields are:
- `meta.name` (string)
- `workdir.access` — one of `none`, `read`, `write`, `readwrite` (default `none`)
- `filesystem.allow` (string[]) — read+write paths (directories or files)
- `filesystem.read` (string[]) — read-only paths
- `filesystem.write` (string[]) — write-only paths
- `filesystem.allow_unix_dir` (string[]) — directories whose every (subpath) AF_UNIX socket the child may connect to, plus read+write access to the dir, for tools that mint sockets with dynamic names at runtime (e.g. Agent View's `/tmp/cc-daemon-<uid>`); unlike the other grants these are not existence-filtered (the dir may be created after launch)
- `filesystem.deny` (string[]) — paths masked (denied read+write) inside granted trees. An entry with a path separator, `~`, or `$VAR` denies that exact expanded path; a bare basename glob (no separator, e.g. `.env` or `*.key`) is denied wherever that name appears in a granted tree, the working directory included. See sandbox-process-isolation.
- `filesystem.override_deny` (string[]) — paths removed from the built-in protected-path deny set (see sandbox-process-isolation); does not grant access by itself
- `network.mode` — one of `filtered` (default), `blocked`, `open`
- `network.allow_domain` (string[]) — exact hostnames or `*.suffix` wildcards
- `network.deny_domain` (string[]) — exact hostnames or `*.suffix` wildcards
- `network.listen_port` (int[]) — TCP ports the child may bind/listen on
- `network.allow_tcp_connect` (int[]) — TCP ports for direct outbound connect to any host
- `network.open_port` (int[]) — localhost TCP ports allowed for both connect and bind
- `network.network_prompt.enabled` (bool, default `true` when the object is present)
- `network.network_prompt.prompt_timeout_secs` (int, default 60)
- `network.network_prompt.on_unavailable` — `deny` (default) or `allow`
- `network.enforcement` — `kernel` (default) or `env-only`
- `environment.allow_vars` (string[]) — exact names or trailing-`*` prefixes; absent or empty means all variables pass (subject to the blocklist)

#### Scenario: Valid profile is parsed
- **WHEN** `omac sandbox run` loads a profile containing the fields above
- **THEN** the sandbox is configured accordingly and the child process is launched

#### Scenario: Unknown field is rejected
- **WHEN** a profile contains a field not in the schema (e.g. `security.groups`)
- **THEN** loading fails before any process is launched, with an error naming the unknown field

### Requirement: Path expansion and missing paths
Filesystem path entries SHALL support `~` expansion to the user home directory and `$VAR` / `${VAR}` environment variable expansion, evaluated in the supervisor's environment. Paths that do not exist at launch time SHALL be skipped with a printed notice rather than causing a failure.

#### Scenario: Tilde path
- **WHEN** a profile contains `"read": ["~/.gitconfig"]`
- **THEN** the child can read `$HOME/.gitconfig` and cannot write it

#### Scenario: Nonexistent path skipped
- **WHEN** a profile grants a path that does not exist on disk
- **THEN** launch proceeds, a notice naming the skipped path is printed, and no rule is emitted for it

### Requirement: Profile resolution and first-start scaffolding
Sandbox profiles SHALL reside in `~/.config/omac/sandbox-profiles/`, one file per profile (`<name>.json`). `omac sandbox run --profile <ref>` SHALL resolve `<ref>` as follows: if `<ref>` is a path (contains a path separator or ends in `.json`), load that file; otherwise load `~/.config/omac/sandbox-profiles/<ref>.json`. If no `--profile` flag is given, `default` is used.

On first start (when `~/.config/omac/sandbox-profiles/default.json` does not exist), the compiled-in default settings SHALL be written to that file — pretty-printed — and then loaded from it, so the user always has an editable on-disk copy. Other profile names that have no file SHALL fail with an error listing the search location; only `default` is auto-created.

#### Scenario: First start writes default.json
- **WHEN** `omac sandbox run` is invoked and `~/.config/omac/sandbox-profiles/default.json` does not exist
- **THEN** the file is created with the compiled-in default settings, pretty-printed, and the sandbox launches with exactly those settings

#### Scenario: Existing default.json wins
- **WHEN** `~/.config/omac/sandbox-profiles/default.json` exists and was edited by the user
- **THEN** the edited file is loaded verbatim and the compiled-in defaults are not consulted

#### Scenario: Unknown profile name
- **WHEN** `--profile nosuch` matches no file
- **THEN** the command exits non-zero with an error naming `~/.config/omac/sandbox-profiles/nosuch.json`

### Requirement: Page policy lives in a sibling pages file
Interactive website decisions (the "permanently" prompt choices) SHALL be stored next to the profile as `~/.config/omac/sandbox-profiles/<name>.pages.json` (e.g. `default.pages.json`), pretty-printed, in the schema `{"schema": 1, "entries": [{"host": "...", "scope": "host"|"suffix", "decision": "allow"|"deny"}]}` (nono-compatible). The file SHALL be loaded at launch and updated atomically on every permanent prompt decision.

#### Scenario: Permanent decision written to pages file
- **WHEN** the user picks `Allow permanently (*.npmjs.org)` while running with profile `default`
- **THEN** `~/.config/omac/sandbox-profiles/default.pages.json` contains the suffix allow entry, pretty-printed, and a later session with the same profile allows `registry.npmjs.org` without prompting

### Requirement: CLI flags merge additively onto the profile
`omac sandbox run` SHALL accept the flags `--allow <path>`, `--read <path>`, `--write <path>`, `--deny <path|glob>`, `--allow-file <path>`, `--allow-unix-dir <dir>`, `--open-port <port>`, `--listen-port <port>`, `--allow-tcp-connect <port>`, `--allow-domain <domain>`, `--deny-domain <domain>`, `--block-net`, `--workdir-access <level>`, and `--profile <ref>`, each repeatable where list-valued. Flag values SHALL be merged additively into the loaded profile, except `--block-net` which sets `network.mode` to `blocked` and overrides all other network settings, and `--workdir-access` which replaces `workdir.access`. The `--deny` flag merges into `filesystem.deny`. The command to run inside the sandbox follows a `--` separator.

#### Scenario: Launcher-style invocation
- **WHEN** `omac sandbox run --profile tng-sandbox --allow-file /tmp/x/bridge.sock --read /tmp/x --open-port 49152 -- opencode` is invoked
- **THEN** the profile's grants plus the socket file, socket dir, and localhost port 49152 are all in effect for the `opencode` child

#### Scenario: Dynamic unix socket dir
- **WHEN** `omac sandbox run --allow-unix-dir /tmp/cc-daemon-502 -- claude` is invoked and `/tmp/cc-daemon-502` does not exist at launch
- **THEN** launch proceeds without a skip notice, and once the daemon creates sockets with dynamic names under that dir the child may connect to them over AF_UNIX (a subpath rule), without any broader unix-socket access

#### Scenario: Block-net override
- **WHEN** `--block-net` is passed alongside a profile with `allow_domain` entries and prompt enabled
- **THEN** all network access except granted Unix sockets is denied, no proxy is started, and a warning states that `--block-net` overrides the profile's network settings
