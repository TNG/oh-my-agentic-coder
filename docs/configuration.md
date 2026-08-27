---
title: Configuration
description: omac configuration files and options
---

## Config files

| File | Purpose | Written by                            |
|---|---|---------------------------------------|
| `oh-my-agentic-coder.yaml` | Launcher config: sandbox runtime selection, facade tuning, audit settings | User                                  |
| `sandbox-profiles/default.json` | Sandbox grants: which filesystem paths, network hosts, and env vars the agent can access | `omac start` (first run creates it) |
| `sandbox-profiles/default.pages.json` | Permanent allow/deny network decisions made via the prompt dialog | Network prompt dialog (user answers)  |
| `sidecar.json` | Skill registry: names, directories, bundle hashes, declared secrets | `omac register` / `omac deregister`   |
| `skill-config.yaml` | Non-secret per-skill fields: API base URLs, region names, feature flags | `omac register` / `omac config`       |

These files live under `~/.config/omac/` (user-global) or `<workdir>/.opencode/` (workdir-local). Only the launcher config is read from both: a project-local `oh-my-agentic-coder.yaml` overrides the user-global copy (named `config.yaml`). See [Per-project configuration](#per-project-configuration).

## Launcher config

The launcher config selects which sandbox runtime to use and tunes a few operational settings. None of this controls what the agent is allowed to access — that is the sandbox profile (see below).

```yaml
sandbox:
  default_profile: builtin          # which sandbox runtime: builtin (default), nono (deprecated), nono-netprofile (deprecated), no-sandbox-debug (debugging)
  profiles: { }                     # custom runtime definitions (deprecated); leave empty unless you need a non-standard sandbox command
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
  scope: global                     # tool cache sharing: global (default), config, or workdir; see Cache
```

**`cache.scope`** controls how widely omac's isolated tool cache is shared between projects. It can be overridden per session with `--cache-scope`. See [Cache](./advanced/cache.md) for details.

**`idle_timeout_secs`** controls how long idle HTTP keep-alive connections to the facade are held open. It does not end the session — the agent and sandbox keep running regardless.

### Per-project configuration

To use different launcher settings for different projects, add a project-local launcher config at `<project>/.opencode/oh-my-agentic-coder.yaml`. When you run `omac start` from that project, omac uses this file instead of your user-global one (`~/.config/omac/config.yaml`).

This works the same for every supported harness, despite the OpenCode-specific naming. (This applies only to omac's launcher config. Each harness's own skills and settings still live in the harness's own directory.)

The project file **replaces** the global one; omac does not merge the two. Any option you leave out falls back to omac's built-in defaults, not to your global settings.

**Warning:** In `omac serve`, the launcher config is read once, from the `--workdir` you started the server with, so switching projects within a running server does not load a different project's file!

The launcher config changes only *how omac launches* in the project: the sandbox runtime it selects, the cache scope, and the facade and audit settings. It does **not** change what the agent is allowed to access. Those grants (filesystem paths, network hosts, open ports) come from the user-global sandbox grants file described below and **currently have no per-project equivalent**.

## Sandbox grants

The sandbox grants file (`~/.config/omac/sandbox-profiles/default.json`) controls what the agent is actually allowed to access — filesystem paths, network mode, and environment variables. This is separate from the launcher config above, which only selects which sandboxing technology to use.

omac creates this file the first time you run `omac start`. Key fields:

| Field | Type | Default | What it controls |
|---|---|---|---|
| `filesystem.deny` | `string[]` | `[".env", "*.key", "*.pem"]` | Blocks files inside granted directories by name or glob |
| `network.mode` | `string` | `"filtered"` | `filtered` (prompt for unknown hosts), `blocked` (no outbound at all), `open` (unrestricted) |
| `environment.allow_vars` | `string[]` | see created file | Env vars passed into the sandbox; everything else is stripped |
| `filesystem.protected_paths` | `string[]` | `["~/.ssh", "~/.gnupg", ...]` | Paths that remain blocked even if a broader grant would cover them |

See [Security model → Sandbox access reference](./security.md#sandbox-access-reference) for the full list of what the agent can and cannot access.

omac never rewrites this file once it exists, so upgrading omac does not add newer default grants to a profile you already have. To pick up the newer defaults, make a copy of your current file, delete the original, and run `omac start` to write a fresh one. Then copy any changes you had made back from your saved copy into the new file.

### Opening a port

To let the agent reach a local service, add the port to `network.open_port` in the sandbox grants file (`~/.config/omac/sandbox-profiles/default.json`):

```json
"network": { "open_port": [3000] }
```

On Linux this also permits outbound connections to that port on any host — Landlock cannot scope a port to localhost — so keep the list short. `omac doctor` and `omac provenance --check` flag every numeric `open_port`.

You can also open a port for a single session with `omac start --open-port 3000`, which is handy for a quick test before changing the grants file.

### Passing an environment variable into the sandbox

The sandbox passes through only the variables named in `environment.allow_vars`, plus a small set of operational defaults (such as `PATH` and `HOME`). Everything else, including any token, is stripped unless configured otherwise.

If a tool inside the sandbox needs a variable from your shell, for example an API token, add its name to the list:

```json
"environment": { "allow_vars": ["MY_API_TOKEN"] }
```

Only the *name* goes here. The value still comes from your shell at start time. There is no CLI flag for this on `omac start`.

Note that this is separate from a skill's secrets: skill credentials are injected on the host and never enter the sandbox (see [Security model](./security.md)). `allow_vars` is for variables that a program *inside* the sandbox reads directly. Those variables can then also be accessed by the agent.

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
