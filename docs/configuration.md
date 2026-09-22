---
title: Configuration
description: omac configuration files and options
---

## Config files

| File | Purpose | Written by                            |
|---|---|---------------------------------------|
| `config.yaml` | Launcher config: `sandbox.profile_name` (global or project-local); facade tuning, cache scope, and audit settings (global only) | User                                  |
| `<name>.json` | Sandbox grants: which filesystem paths, network hosts, and env vars the agent can access | `omac start` (first run scaffolds the global `default.json`) |
| `<name>.pages.json` | Permanent allow/deny network decisions made via the prompt dialog | `omac start` (creates it empty at launch); network prompt dialog (user answers) |
| `sidecar.json` | Skill registry: names, directories, bundle hashes, declared secrets | `omac register` / `omac deregister`   |
| `skill-config.yaml` | Non-secret per-skill fields: API base URLs, region names, feature flags | `omac register` / `omac config`       |

Each layer has its own directory:

| Layer | Directory | Launcher config | Profiles |
|---|---|---|---|
| user-global | `~/.config/omac/` | `config.yaml` | `sandbox-profiles/<name>.json` |
| project-local | `<workdir>/.omac/` | `config.yaml` | `<name>.json` |

The project-local directory is created whenever you run `omac start`/`omac serve` and is **read-only inside the sandbox, exposing only an explanatory denial notice** (`.omac-denied`): a session can neither read the rules nor write or create files in it, and a symlinked `.omac` refuses the launch. Because the workdir is agent-writable, omac also **pins the approved project-local content host-side** (`~/.config/omac/project-sandbox.json`, invisible to the sandbox): if the `.omac` configuration changes between sessions without re-approval, the next launch ignores the local layer and uses the global one, so a replaced profile is never trusted. Re-approve a deliberate change with `--accept-project-config` (after `git diff -- .omac`), or restore it with `git checkout -- .omac`. If the workdir (e.g. a read-only checkout) prevents creation, omac warns and continues — the agent runs with your own permissions, so it could not create or read it either. See [Per-project configuration](#per-project-configuration).

## Launcher config

The launcher config tunes a few operational settings. None of this controls what the agent is allowed to access — that is the sandbox profile (see below).

```yaml
sandbox:
  profile_name: ""                  # pick <name>.json from this config's own directory; "" uses default.json
facade:
  idle_timeout_secs: 300            # close idle HTTP keep-alive connections after N seconds; does not end the session
  max_body_bytes: 10485760          # 10 MB request body cap
  base_env_passthrough: [PATH, HOME, USER, LANG, LC_ALL, LC_CTYPE, TMPDIR]
audit:
  enabled: true                     # security audit trail (default on)
  path: ""                          # "" uses the platform default path (see below)
  syslog: false                     # also mirror events to the system log (Unix)
  strict: false                     # fail-closed: abort the run if a log write fails
cache:
  scope: workdir                    # tool cache sharing: workdir (default), config, or global; see Cache
```

**`sandbox.profile_name`** selects a sandbox grants profile by name. The name is
resolved in the directory of the config that declares it and never crosses
layers: a project-local `config.yaml` selects from `<workdir>/.omac/`, a global
`config.yaml` from `~/.config/omac/sandbox-profiles/`. Empty means "this
layer's `default.json` if it exists, else the other layer, else the built-in
default". A name that does not exist, or one containing a path separator, is a
hard error. See [Sharing a profile across a team](#sharing-a-profile-across-a-team).

**`cache.scope`** controls how widely omac's isolated tool cache is shared between projects. It can be overridden per session with `--cache-scope`. See [Cache](./advanced/cache.md) for details.

**`idle_timeout_secs`** controls how long idle HTTP keep-alive connections to the facade are held open. It does not end the session — the agent and sandbox keep running regardless.

### Per-project configuration

To use different operational or sandbox settings for a project, add a
project-local config at `<project>/.omac/config.yaml`. omac creates the
`.omac/` directory whenever you run `omac start`/`omac serve` if it is missing.

**What the project config can set:** `cache.scope`,
`facade.idle_timeout_secs`, `facade.max_body_bytes`, and
`sandbox.profile_name` (which resolves only inside `<project>/.omac/`). These
are layered on top of your global config (or built-in defaults when no global
config exists).

**What only the global config (`~/.config/omac/config.yaml`) can set:** the
sandbox system-prompt briefing, `audit.*`, and `facade.base_env_passthrough`. A
project-local file cannot change any of these. Settings that decide how the
sandbox runs on the host, or whether its actions are recorded, must stay under
your control, not the project's. `sandbox.profile_name` is the one sandbox
setting a project may set, because the file it names is committed and reviewed
in git, lives in a directory the agent cannot read or write, and never crosses
into the global layer.

**Warning:** In `omac serve`, the launcher config is read once, from the
`--workdir` you started the server with, so switching projects within a running
server does not load a different project's file!

The sandbox grants (filesystem paths, network hosts, open ports) come from the
sandbox grants file below — per-project by committing `.omac/default.json` (or
another profile plus `sandbox.profile_name`).

## Harness config home

Each harness can relocate its configuration directory (its "config home") via an environment variable — typically to keep two logins side by side, for example a personal and a work `~/.claude`. omac follows the redirect: the redirected directory is granted to the sandbox instead of the default one and created if it does not exist yet, the variable is passed through to the harness, and global skills and resumable sessions are read from the redirected directory.

| Harness | Variable | Default |
|---|---|---|
| claude-code | `CLAUDE_CONFIG_DIR` | `~/.claude` |
| codex | `CODEX_HOME` | `~/.codex` |
| copilot | `COPILOT_HOME` | `~/.copilot` |
| pi | `PI_CODING_AGENT_DIR` | `~/.pi/agent` |
| codewhale | `CODEWHALE_HOME` | `~/.codewhale` |
| opencode | *(none)* — relocate with `$XDG_CONFIG_HOME` | `~/.config/opencode` |

Example: `CLAUDE_CONFIG_DIR=~/.work-claude omac start claude` runs Claude Code with the work login instead of prompting for a fresh one.

A redirect also **hides** the skills installed under the default home: omac scans only the redirected directory for global skills. After switching homes, run `omac setup` to provision the built-in skills there too.

## Sandbox grants

The sandbox grants profile (`~/.config/omac/sandbox-profiles/default.json` globally, or `<workdir>/.omac/default.json` for a project) controls what the agent is actually allowed to access — filesystem paths, network mode, and environment variables. This is separate from the launcher config above, which selects the profile and tunes operational settings (facade, cache, audit).

omac creates this file the first time you run `omac start`. Key fields:

**Selection is by file name.** A profile is selected by its *file name*
(`profile_name: team` → `<layer>/team.json`); the `"name"` in the JSON's
`meta` block (`meta.name`) is a display label only — it never selects a
profile, and omac does not check that it matches the file name. Keep them in
sync to avoid confusing diagnostics output.

| Field | Type | Default | What it controls |
|---|---|---|---|
| `filesystem.deny` | `string[]` | `[".env", "*.key", "*.pem"]` | Blocks files inside granted directories by name or glob |
| `network.mode` | `string` | `"filtered"` | `filtered` (prompt for unknown hosts), `blocked` (no outbound TCP; on Linux with kernel enforcement, UDP/ICMP also blocked), `open` (unrestricted) |
| `environment.allow_vars` | `string[]` | see created file | Env vars passed into the sandbox; everything else is stripped |
| `filesystem.protected_paths` | `string[]` | `["~/.ssh", "~/.gnupg", ...]` | Paths that remain blocked even if a broader grant would cover them |
| `filesystem.registry_config` | `string[]` | `[]` | Ecosystems whose package-registry settings are copied into the sandbox without their credentials. Currently `"npm"`. See [Private package registries](#private-package-registries) |

See [Security model → Sandbox access reference](./security.md#sandbox-access-reference) for the full list of what the agent can and cannot access.

omac never rewrites this file once it exists, so upgrading omac does not add newer default grants to a profile you already have. To pick up the newer defaults, make a copy of your current file, delete the original, and run `omac start` to write a fresh one. Then copy any changes you had made back from your saved copy into the new file.

The reverse also applies. An unknown field is an error, so that a typo cannot quietly weaken the sandbox. A file using a newer field, such as `filesystem.registry_config`, is therefore rejected by an older omac. If you share this file between machines, upgrade omac on all of them before adding a new field.

### Sharing a profile across a team

Commit a profile when a project needs different grants and everyone should get
the same ones (reviewed in git).

1. Scaffold a starting file globally: `omac start` writes
   `~/.config/omac/sandbox-profiles/default.json`.
2. Copy it into the project as `.omac/default.json`, then edit the grants. If
   the project should not use `default.json`, name it and select it:

```yaml
# <project>/.omac/config.yaml
sandbox:
  profile_name: team
```

- **Shared:** the profile(s) under `.omac/` (commit them). Team allowlist →
  `network.allow_domain`.
- **Local:** `<profile>.pages.json` (per-user "allow permanently" clicks) —
  created empty on the first launch, git-ignored automatically, never shared.

Several local profiles can live side by side in `.omac/`; switch between them
with `sandbox.profile_name` or `--profile-path .omac/<name>.json`. A name never
resolves across layers, so a local `strict` is always `<workdir>/.omac/strict.json`
and never the global `strict`.

**Tamper and read protection.** Inside the sandbox the `.omac/` directory is
masked read-only, so the agent can neither read the rules nor write or create
files in it — it cannot rewrite the grants a later launch enforces, nor
pre-allow network hosts by editing the pages file. On Linux the directory is
also an unremovable read-only mount. On macOS Seatbelt cannot block replacing
the directory inside a writable workdir, so omac anchors trust host-side
instead: the approved project-local content is pinned in
`~/.config/omac/project-sandbox.json` (invisible to the session), and a `.omac`
whose content changed is ignored on the next launch in favour of the global
layer until you re-approve it with `--accept-project-config`. A global profile
(`~/.config/omac/`) always sits outside every granted path and is invisible to
the session on both platforms. omac writes learned decisions from outside the
sandbox, so nothing changes for you.

`--profile-path` is constrained too: it accepts only a path inside
`~/.config/omac/sandbox-profiles/` or `<workdir>/.omac/`, and refuses symlinks.

### Migrating from 0.9.0

- The project launcher config moved from
  `<project>/.opencode/oh-my-agentic-coder.yaml` to `<project>/.omac/config.yaml`.
  Move it with `mkdir -p .omac && mv .opencode/oh-my-agentic-coder.yaml .omac/config.yaml`.
  The old path is no longer read; omac warns when it exists.
- `sandbox.default_profile` and the `sandbox.profiles` argv templates have no
  effect and are rejected: remove the line/block. If `default_profile` named a
  profile you still want (omac lists the profiles it finds for that layer),
  rename the field instead:
  `default_profile: team` → `profile_name: team`. omac always runs its built-in
  sandbox; `sandbox.profile_name` chooses sandbox grants, put the profile at
  `~/.config/omac/sandbox-profiles/<name>.json` (global config) or
  `<workdir>/.omac/<name>.json` (project config).
- The `--sandbox <name>` flag on `omac start`/`serve` was removed. Use
  `--profile-path .omac/<name>.json` (or `sandbox.profile_name`).
- `omac sandbox run --profile` is unchanged (name or path).

### Opening a port

To let the agent reach a local service, add the port to `network.open_port` in the sandbox grants file (`~/.config/omac/sandbox-profiles/default.json`):

```json
"network": { "open_port": [3000] }
```

On Linux this also permits outbound connections to that port on any host — Landlock cannot scope a port to localhost — so keep the list short. `omac doctor` and `omac provenance --check` flag every numeric `open_port`.

**macOS only:** the sentinel `"open_port": [0]` allows any loopback port (`localhost:*`) while external egress stays blocked. This is useful for tools that pick a random loopback port at runtime, such as the Gradle daemon (see [troubleshooting](./troubleshooting.md#gradle-build-hangs-or-cannot-reach-its-daemon)).
Linux has no equivalent, because Landlock cannot scope a port to localhost.

You can also open a port for a single session with `omac start --open-port 3000`, which is handy for a quick test before changing the grants file.

### Passing an environment variable into the sandbox

By default, the sandbox strips every variable from your shell except a small set of operational defaults (`PATH`, `HOME`, `LANG`, …). Ambient secrets like cloud tokens never reach the agent.

If a program running inside the sandbox needs one of your shell's variables — for example an API token a build tool or MCP server reads — add its name to `environment.allow_vars` in the sandbox grants file (`~/.config/omac/sandbox-profiles/default.json`):

```json
"environment": { "allow_vars": ["MY_API_TOKEN"] }
```

Only the *name* goes here; the value comes from your shell when you run `omac start`, so export it first.

A few things to know:
- For safety, a few variables that let a program load extra code (`LD_*`, `NODE_OPTIONS`, `PYTHONPATH`, …) are always stripped, even if you add them to `allow_vars`. Run `omac provenance` and look at the `environment` section: the always-stripped variables are the rows with action `deny` and source `blocklist`. To list just those, run `omac provenance | grep blocklist`.
- An empty `allow_vars` means "operational defaults only", not "pass everything through".
- Do not use this to pass through secrets (e.g. for skills). See the next section and [Security model](./security.md) for secure ways to do so.

### Running an MCP server the harness launches

An MCP server is configured in the harness, not in omac.
The harness (opencode, claude-code, …) launches MCP servers **inside the sandbox**, so the MCP server is limited by the sandbox restrictions. Two things commonly need granting:

- **A token**, if the server authenticates with an API key: add the variable to `environment.allow_vars` (see above) and export it before `omac start`.
- **A local port**, if the server uses the HTTP transport and opens one for the harness to connect to: add it to `network.open_port` (see above). Servers that use the stdio transport talk over the process's input and output instead and need no port.

For example, an MCP server that reads `KAGGLE_KEY` and listens on port 3334:

```json
"environment": { "allow_vars": ["KAGGLE_KEY"] },
"network": { "open_port": [3334] }
```

**The token can be accessed by the agent in this setting.** So:

- For anything holding a real secret, prefer an omac skill if one exists or can be written (see [Authoring skills](./skills/authoring.md)).
- If you do use an MCP server with a token, use a scoped, least-privilege token.

### Java and Node dependency downloads

Java (Maven/Gradle) and Node/npm do not reliably route their package downloads through a proxy on their own, so their downloads can fail inside the sandbox. To fix this, add `jvm`, `node`, or both to `network.proxy_injection` in the sandbox grants file, and omac configures those toolchains to use its proxy.

Node injection requires Node ≥ 22.21.0 (22.x line) or ≥ 24.5.0; on older versions it is skipped and downloads may still fail.

### Private package registries

If your company hosts its own npm packages, `~/.npmrc` says where to find them. A line like `@acme:registry=https://npm.acme.test` means "packages starting with `@acme/` come from that server".

The sandbox blocks `~/.npmrc`, because the same file usually holds an access token. Without it, npm looks for `@acme/` packages on the public registry instead, does not find them, and reports a 404. The error looks like the package does not exist, so this is easy to misread. Allowing the registry's host does not help, because npm never asks it.

To fix this, add `npm` to `filesystem.registry_config`:

```json
{ "filesystem": { "registry_config": ["npm"] } }
```

omac then writes a copy of `~/.npmrc` that contains only the registry addresses, lets the sandbox read that copy, and points npm at it. The real file stays blocked, so no token is copied. If a line cannot be copied without also copying a secret, omac skips that line and tells you which one, both at startup and in `omac doctor`.

Private registries usually also need their host added to `network.allow_domain`, or allowed once at the network prompt.

omac cannot pass on your access token, so packages that require login still fail to install. Only the address is shared, never the credential.

## Audit trail

omac logs every security-relevant action to an append-only file: process launches, network decisions, secret injections. The file is outside the sandbox so the agent cannot tamper with it.

| Platform | Default path |
|---|---|
| Linux | `~/.local/state/omac/audit/audit.jsonl` |
| WSL2 | `~/.local/state/omac/audit/audit.jsonl` (same as Linux; `~` is the WSL Linux home, not `C:\Users\…`) |
| macOS | `~/Library/Logs/omac/audit/audit.jsonl` |

| Flag | Effect |
|---|---|
| `--no-audit` | Disable the audit trail entirely |
| `--audit-strict` | Fail-closed: abort if the log cannot be opened or a write fails mid-session |
| `--audit-log <path>` | Write the log to `<path>` instead of the default |

By default, a write error emits a warning and the run continues.

## Corporate proxy

If your network routes outbound traffic through a corporate proxy, omac picks it up automatically from `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` in your shell environment — you do not need to configure anything. omac's own network filtering runs first, then allowed traffic is forwarded through the proxy.

If you need omac to use a different proxy than what your shell environment specifies, set `network.upstream_proxy` and `network.no_proxy` in the sandbox grants file.
