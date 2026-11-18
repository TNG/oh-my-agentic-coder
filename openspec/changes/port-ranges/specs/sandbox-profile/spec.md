# sandbox-profile

<!--
Baseline: openspec/specs/ has no sandbox-profile spec yet. The requirements
below currently live in openspec/changes/native-sandbox/specs/sandbox-profile/
spec.md; both are MODIFIED against that text, so archive native-sandbox before
archiving this change.
-->

## MODIFIED Requirements

### Requirement: Profile file format

The sandbox SHALL be configured by a JSON profile with the top-level sections
`meta`, `workdir`, `filesystem`, `network`, and `environment`. All sections
SHALL be optional. The parser MUST reject unknown fields with an error naming
the offending field.

The supported fields are:

- `meta.name` (string)
- `workdir.access` — one of `none`, `read`, `write`, `readwrite` (default
  `none`)
- `filesystem.allow` (string[]) — read+write paths (directories or files)
- `filesystem.read` (string[]) — read-only paths
- `filesystem.write` (string[]) — write-only paths
- `filesystem.allow_unix_dir` (string[]) — directories whose every (subpath)
  AF_UNIX socket the child may connect to, plus read+write access to the dir,
  for tools that mint sockets with dynamic names at runtime (e.g. Agent View's
  `/tmp/cc-daemon-<uid>`); unlike the other grants these are not
  existence-filtered (the dir may be created after launch)
- `filesystem.deny` (string[]) — paths masked (denied read+write) inside granted
  trees. An entry with a path separator, `~`, or `$VAR` denies that exact
  expanded path; a bare basename glob (no separator, e.g. `.env` or `*.key`) is
  denied wherever that name appears in a granted tree, the working directory
  included. See sandbox-process-isolation.
- `filesystem.override_deny` (string[]) — paths removed from the built-in
  protected-path deny set (see sandbox-process-isolation); does not grant access
  by itself
- `network.mode` — one of `filtered` (default), `blocked`, `open`
- `network.allow_domain` (string[]) — exact hostnames or `*.suffix` wildcards
- `network.deny_domain` (string[]) — exact hostnames or `*.suffix` wildcards
- `network.listen_port` (int[] | string[] mixed) — TCP ports the child may
  bind/listen on; an entry is a port number or a string `"N"` or `"A-B"`,
  expanded at load time
- `network.allow_tcp_connect` (int[] | string[] mixed) — the same entry syntax,
  for direct outbound connect to any host on those ports
- `network.open_port` (int[]) — localhost TCP ports allowed for both connect and
  bind; range strings are not accepted here
- `network.network_prompt.enabled` (bool, default `true` when the object is
  present)
- `network.network_prompt.prompt_timeout_secs` (int, default 60)
- `network.network_prompt.on_unavailable` — `deny` (default) or `allow`
- `network.enforcement` — `kernel` (default) or `env-only`
- `network.proxy_injection` (string[]) — families of proxy-unaware toolchains to
  route through the omac filtering proxy under `filtered` mode. Accepted values:
  `jvm` (injects a supervisor-controlled `JAVA_TOOL_OPTIONS`) and `node`
  (injects `NODE_USE_ENV_PROXY=1`; requires Node ≥ 22.21.0 on the 22.x line, or
  ≥ 24.5.0 on current and later lines). Tools that already honor `HTTP(S)_PROXY`
  (`curl`, `git`, `pip`, `npm`, `go`, …) need no entry. Unknown families are
  rejected at load time.
- `environment.allow_vars` (string[]) — exact names or trailing-`*` prefixes;
  absent or empty means all variables pass (subject to the blocklist)

Port-grant entries in `network.listen_port` and `network.allow_tcp_connect`
SHALL be either a JSON integer (no fractional part) or a JSON string of the
form `N` or `A-B`, where both endpoints are inclusive decimal port numbers in
0–65535 and `A` does not exceed `B`; leading zeros in a string entry are
allowed, and whitespace around an entry and around the dash of a range is
ignored. String entries SHALL be expanded to individual ports when the profile
is loaded, so the in-memory value holds numbers only; loading SHALL NOT modify
the profile file. A learn-mode save of the profile writes the expanded integer
list, not the range. The expansion of one field SHALL NOT exceed 1000 ports.
The cap counts entries as written, before duplicate removal, whatever their
form: a plain integer and a range both
contribute every port they name. An over-cap expansion, a reversed range,
an out-of-bounds endpoint, a fractional number, and an entry that is neither an
integer nor a valid string SHALL be rejected at load time, before any process
is launched, with an error naming the offending value. Both fields SHALL keep
their minimum of 1, so an expansion that includes port 0 is rejected by
validation. `network.open_port` SHALL remain an integer list and SHALL NOT
accept range strings.

#### Scenario: Valid profile is parsed

- **WHEN** `omac sandbox run` loads a profile containing the fields above
- **THEN** the sandbox is configured accordingly and the child process is
  launched

#### Scenario: Unknown field is rejected

- **WHEN** a profile contains a field not in the schema (e.g. `security.groups`)
- **THEN** loading fails before any process is launched, with an error naming
  the unknown field

#### Scenario: Mixed port list is expanded at load time

- **WHEN** a profile contains `"listen_port": [4097, "8000-8100"]`
- **THEN** loading succeeds and the child may bind 4097 and every port from 8000
  through 8100, inclusive (102 ports)
- **AND** loading leaves the profile file on disk unchanged; a later learn-mode
  save writes the expanded integer list

#### Scenario: String entries in allow_tcp_connect

- **WHEN** a profile contains `"allow_tcp_connect": ["22", "8000-8100"]`
- **THEN** direct outbound TCP is allowed on port 22 and on every port from 8000
  through 8100, inclusive

#### Scenario: Integer-only profile is unchanged

- **WHEN** both fields contain only numbers, as in every profile written before
  this change, and neither field exceeds the 1000-port cap
- **THEN** loading and validation behave exactly as before

#### Scenario: Reversed range is rejected

- **WHEN** a profile contains `"listen_port": ["8100-8000"]`
- **THEN** loading fails with an error naming the value
- **AND** no port from that entry is granted and no child is launched

#### Scenario: Out-of-bounds endpoint is rejected

- **WHEN** a profile contains `"allow_tcp_connect": ["8000-70000"]`
- **THEN** loading fails with an error naming the value, and the endpoint is not
  clamped to 65535

#### Scenario: Range above the cap is rejected

- **WHEN** the expansion of one field would exceed 1000 ports, for example
  `"listen_port": ["1-65535"]` or several entries summing to more than 1000
- **THEN** loading fails with an error naming the field, or the single over-large
  entry, and the 1000-port cap
- **AND** no narrowed grant is applied and no child is launched

#### Scenario: Non-integer entry is rejected

- **WHEN** an entry is `8000.5`, `true`, `null`, or `"abc"`
- **THEN** loading fails with an error naming the entry, and the process does
  not panic

#### Scenario: Zero parses but stays invalid for these fields

- **WHEN** a profile contains `"listen_port": ["0-100"]`
- **THEN** the range parses, and validation still rejects port 0 for
  `listen_port`

#### Scenario: open_port does not accept ranges

- **WHEN** a profile contains `"open_port": ["3000-3100"]`
- **THEN** loading fails with an error naming `open_port`
- **AND** `open_port` semantics are otherwise unchanged by this change

### Requirement: CLI flags merge additively onto the profile

`omac sandbox run` SHALL accept the flags `--allow <path>`, `--read <path>`,
`--write <path>`, `--deny <path|glob>`, `--allow-file <path>`,
`--allow-unix-dir <dir>`, `--open-port <port>`, `--listen-port <port|A-B>`,
`--allow-tcp-connect <port|A-B>`, `--allow-domain <domain>`,
`--deny-domain <domain>`, `--block-net`, `--workdir-access <level>`, and
`--profile <ref>`, each repeatable where list-valued. A `<port|A-B>` value is
either a single port or an inclusive range using the same syntax as the profile
fields, and it expands before merging. The merged list of each range-accepting
field, profile entries plus flag entries, SHALL NOT exceed 1000 ports, and a
merge above that cap SHALL be rejected before any process is launched. Flag
values SHALL be merged additively into the loaded profile, except `--block-net`
which sets `network.mode` to `blocked` and overrides all other network settings,
and `--workdir-access` which replaces `workdir.access`. The `--deny` flag merges
into `filesystem.deny`. The command to run inside the sandbox follows a `--`
separator.

#### Scenario: Launcher-style invocation

- **WHEN** `omac sandbox run --profile tng-sandbox --allow-file /tmp/x/bridge.sock --read /tmp/x --open-port 49152 -- opencode` is invoked
- **THEN** the profile's grants plus the socket file, socket dir, and localhost
  port 49152 are all in effect for the `opencode` child

#### Scenario: Dynamic unix socket dir

- **WHEN** `omac sandbox run --allow-unix-dir /tmp/cc-daemon-502 -- claude` is invoked and `/tmp/cc-daemon-502` does not exist at launch
- **THEN** launch proceeds without a skip notice, and once the daemon creates
  sockets with dynamic names under that dir the child may connect to them over
  AF_UNIX (a subpath rule), without any broader unix-socket access

#### Scenario: Block-net override

- **WHEN** `--block-net` is passed alongside a profile with `allow_domain` entries and prompt enabled
- **THEN** all network access except granted Unix sockets is denied, no proxy is
  started, and a warning states that `--block-net` overrides the profile's
  network settings

#### Scenario: A range on a flag is expanded

- **WHEN** `omac sandbox run --listen-port 8000-8100 -- <cmd>` is invoked
- **THEN** the child may bind every port from 8000 through 8100, inclusive
- **AND** repeating the flag with several ranges adds all of their ports

#### Scenario: Repeating a flag keeps the profile's ports

- **WHEN** the profile grants `"allow_tcp_connect": [22]` and the invocation adds `--allow-tcp-connect 8000-8100`
- **THEN** the child may connect to any host on port 22 and on 8000 through 8100

#### Scenario: Invalid range on a flag is rejected

- **WHEN** `--listen-port 8100-8000` or `--allow-tcp-connect 8000-70000` is
  passed
- **THEN** the command exits with an error naming the flag and the value, and no
  process is launched

#### Scenario: Single port on a flag is unchanged

- **WHEN** `--listen-port 4097` is passed
- **THEN** the child may bind 4097, exactly as before this change

#### Scenario: open-port keeps a single port

- **WHEN** `--open-port 49152` is passed
- **THEN** the value is parsed as one port, and a range such as `--open-port 49152-49160` is rejected
