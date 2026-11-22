---
title: Configuration
description: omac configuration files and options
---

## Config files

| File | Purpose | Written by                            |
|---|---|---------------------------------------|
| `oh-my-agentic-coder.yaml` | Launcher config: sandbox runtime selection, facade tuning, audit settings (global only); cache scope and facade timeouts (also project-local) | User                                  |
| `sandbox-profiles/default.json` | Sandbox grants: which filesystem paths, network hosts, and env vars the agent can access | `omac start` (first run creates it) |
| `sandbox-profiles/default.pages.json` | Permanent allow/deny network decisions made via the prompt dialog | Network prompt dialog (user answers)  |
| `sidecar.json` | Skill registry: names, directories, bundle hashes, declared secrets | `omac register` / `omac deregister`   |
| `skill-config.yaml` | Non-secret per-skill fields: API base URLs, region names, feature flags | `omac register` / `omac config`       |

These files live under `~/.config/omac/` (user-global) or `<workdir>/.opencode/` (workdir-local). When `$XDG_CONFIG_HOME` is set, the user-global directory is `$XDG_CONFIG_HOME/omac/` instead, for every file in the table and for the sandbox profiles. The launcher config is read from both locations when both exist; see [Per-project configuration](#per-project-configuration) for what each location can control.

## Launcher config

The launcher config selects which sandbox runtime to use (global config only) and tunes operational settings. The sandbox *grants* — which filesystem paths, network hosts, and env vars the agent can access — are in the sandbox profile (see below), not here.

```yaml
sandbox:
  default_profile: builtin          # which sandbox runtime: builtin (default), nono (deprecated), nono-netprofile (deprecated), no-sandbox-debug (debugging)
  profiles: { }                     # custom runtime definitions (deprecated); leave empty unless you need a non-standard sandbox command
facade:
  idle_timeout_secs: 300            # close idle connections after N seconds (HTTP keep-alive, and WebSocket connections with no traffic); does not end the session
  max_body_bytes: 10485760          # 10 MB request body cap
  base_env_passthrough: [PATH, HOME, USER, LANG, LC_ALL, LC_CTYPE, TMPDIR]
audit:
  enabled: true                     # security audit trail (default on)
  path: ""                          # "" uses the platform default path (see below)
  syslog: false                     # also mirror events to the system log (Unix)
  strict: false                     # fail-closed: abort the run if a log write fails
cache:
  scope: workdir                    # tool cache sharing: workdir (default), config, or global; see Cache
mcp:
  unregistered: refuse              # harness MCP entries omac did not register: allow | warn | refuse (default refuse once a server is registered)
```

**`cache.scope`** controls how widely omac's isolated tool cache is shared between projects. It can be overridden per session with `--cache-scope`. See [Cache](./advanced/cache.md) for details.

**`mcp`** controls what omac does about MCP entries in the harness's own config files once you have registered a server with the [MCP gateway](./usage/mcp.md). It is read from the global `config.yaml` only (`~/.config/omac/config.yaml`, or `$XDG_CONFIG_HOME/omac/config.yaml`); a project-local file cannot set it. `omac start --mcp-unregistered=<value>` overrides `unregistered` for one run. See [Existing MCP entries](./usage/mcp.md#existing-mcp-entries-in-the-harness-config).

Registered MCP servers are not configured in this file. Their definitions (command, secrets, tool classes, `access`, `approvals` and the tool-list `pin`) live on the host under `~/.config/omac/mcp/`; see the [definition reference](./usage/mcp.md#definition-reference).

**`idle_timeout_secs`** controls how long an idle connection to the facade is held open. For plain HTTP it is the keep-alive time between requests. For a WebSocket connection it is the inactivity limit: the connection closes after this many seconds with no data in either direction, and one that keeps sending messages or pings stays open. A skill can set its own WebSocket limit with `limits.idle_timeout_secs` (see [Skill authoring](./skills/authoring.md)). The setting does not end the session; the agent and sandbox keep running.

### Per-project configuration

To use different operational settings for different projects, add a project-local launcher config at `<project>/.opencode/oh-my-agentic-coder.yaml`. This works the same for every supported harness, despite the OpenCode-specific naming.

**What the project file can set:** `cache.scope`, `facade.idle_timeout_secs`, `facade.max_body_bytes`. These operational settings are layered on top of your global config (or built-in defaults when no global config exists).

**What only the global config (`~/.config/omac/config.yaml`) can set:** `sandbox.*` (which runtime to use, the confinement command, and the system prompt briefing), `audit.*`, and `facade.base_env_passthrough`. A project-local file cannot change any of these. This is intentional: the sandbox command runs on the host with your full environment, so its configuration must stay under your control, not the project's.

**Warning:** In `omac serve`, the launcher config is read once, from the `--workdir` you started the server with, so switching projects within a running server does not load a different project's file!

The sandbox grants (filesystem paths, network hosts, open ports) come from the user-global sandbox profile file described below and have no per-project equivalent.

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

The sandbox grants file (`~/.config/omac/sandbox-profiles/default.json`) controls what the agent is actually allowed to access — filesystem paths, network mode, and environment variables. This is separate from the launcher config above, which only selects which sandboxing technology to use.

omac creates this file the first time you run `omac start`. Key fields:

| Field | Type | Default | What it controls |
|---|---|---|---|
| `filesystem.deny` | `string[]` | `[".env", "*.key", "*.pem"]` | Blocks files inside granted directories by name or glob |
| `network.mode` | `string` | `"filtered"` | `filtered` (prompt for unknown hosts), `blocked` (no outbound TCP; on Linux with kernel enforcement, UDP/ICMP also blocked), `open` (unrestricted) |
| `environment.allow_vars` | `string[]` | see created file | Env vars passed into the sandbox; everything else is stripped |
| `filesystem.protected_paths` | `string[]` | `["~/.ssh", "~/.gnupg", ...]` | Paths that remain blocked even if a broader grant would cover them |
| `filesystem.registry_config` | `string[]` | `[]` | Ecosystems whose package-registry settings are copied into the sandbox without their credentials. Currently `"npm"`. See [Private package registries](#private-package-registries) |

See [Security model → Sandbox access reference](./security.md#sandbox-access-reference) for the full list of what the agent can and cannot access.

omac does not rewrite this file on its own once it exists, so upgrading omac does not add newer default grants to a profile you already have. The one exception is a learn-mode session (`omac sandbox run --learn`) where you approve adding folders, which rewrites the file. To pick up the newer defaults, make a copy of your current file, delete the original, and run `omac start` to write a fresh one. Then copy any changes you had made back from your saved copy into the new file.

The reverse also applies. An unknown field is an error, so that a typo cannot quietly weaken the sandbox. A file using a newer field, such as `filesystem.registry_config`, is therefore rejected by an older omac. If you share this file between machines, upgrade omac on all of them before adding a new field.

### Opening a port

To let the agent reach a local service, add the port to `network.open_port` in the sandbox grants file (`~/.config/omac/sandbox-profiles/default.json`):

```json
"network": { "open_port": [3000] }
```

On Linux this also permits outbound connections to that port on any host — Landlock cannot scope a port to localhost — so keep the list short. `omac doctor` and `omac provenance --check` flag every numeric `open_port`.

**macOS only:** the sentinel `"open_port": [0]` allows any loopback port (`localhost:*`) while external egress stays blocked. This is useful for tools that pick a random loopback port at runtime, such as the Gradle daemon (see [troubleshooting](./troubleshooting.md#gradle-build-hangs-cannot-reach-its-daemon-or-cannot-start-a-daemon)).
Linux has no equivalent, because Landlock cannot scope a port to localhost.

You can also open a port for a single session with `omac start --open-port 3000`, which is handy for a quick test before changing the grants file.

### Granting port ranges

In the sandbox grants file (`~/.config/omac/sandbox-profiles/default.json`), `network.listen_port` and `network.allow_tcp_connect` take a list of ports. An entry is a number, a numeric string such as `"8100"`, or an inclusive range such as `"8000-8100"`. A process that reserves a block of ports, such as a dev server scanning 8000 to 8100, needs one entry instead of 101:

```json
"network": { "listen_port": [4097, "8000-8100"] }
```

That grants port 4097 and every port from 8000 through 8100. Invalid entries are rejected when the profile loads, before the session starts, with an error naming the value.

Loading a profile never rewrites it. If you let `omac sandbox run --learn` add folders to this file, omac rewrites it, and a range such as `"8000-8100"` becomes the full list of ports.

`network.listen_port` is for servers inside the sandbox that listen on a port. `network.allow_tcp_connect` allows direct outbound TCP to any host on those ports, for tools whose protocol the proxy cannot carry, such as SSH or a database client. The domain allow/deny list does not apply to those connections.

Each of the two fields is limited to 1000 ports, counting profile entries and flags together. Overlapping ranges count in full. A longer list is rejected, not shortened. The limit applies to integer-only lists too.

On Linux, each port in a range becomes its own kernel rule, exactly as if you had listed it separately.

`network.open_port` accepts no ranges and stays a list of single ports. A port there also permits outbound connections to any host on it (see [Opening a port](#opening-a-port)), so omac keeps that grant deliberately narrow.

For a single session, `omac sandbox run` accepts these as flags: `--listen-port` and `--allow-tcp-connect`. Each takes a single port or a range and can be repeated. The flags add to the profile's ports rather than replacing them:

```bash
omac sandbox run --listen-port 8000-8100 --allow-tcp-connect 22 -- <command>
```

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

An MCP server is configured in the harness, not in omac. Inside omac, MCP configuration comes only from the omac registry (see the [MCP gateway](./usage/mcp.md)). Once you register a server, omac refuses to start while such harness entries exist, unless you run with `--mcp-unregistered=allow` (see [Existing MCP entries](./usage/mcp.md#existing-mcp-entries-in-the-harness-config)).
The harness (opencode, claude-code, …) launches MCP servers **inside the sandbox**, so the MCP server is limited by the sandbox restrictions. Two things commonly need granting:

- **A token**, if the server authenticates with an API key: add the variable to `environment.allow_vars` (see above) and export it before `omac start`.
- **A local port**, if the server uses the HTTP transport and opens one for the harness to connect to: add it to `network.open_port` (see above). Servers that use the stdio transport talk over the process's input and output instead and need no port.

For example, an MCP server that reads `KAGGLE_KEY` and listens on port 3334:

```json
"environment": { "allow_vars": ["KAGGLE_KEY"] },
"network": { "open_port": [3334] }
```

**The token can be accessed by the agent in this setting.** So:

- For anything holding a real secret, register the server with the [MCP gateway](./usage/mcp.md), which runs it outside the sandbox with its credentials, or use an omac skill if one exists or can be written (see [Authoring skills](./skills/authoring.md)).
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

## Proxy configuration

If your network routes outbound traffic through a proxy, omac picks it up automatically from `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` in your shell environment — you do not need to configure anything. omac's own network filtering runs first, then allowed traffic is forwarded through the proxy.

`NO_PROXY` entries (and `network.no_proxy` in the grants file) accept `example.com`, `.example.com` and `*.example.com`. All three match the domain itself and its subdomains. A lone `*` matches every host, and a single IP address such as `10.1.2.3` matches requests to that address. Matching hosts skip the proxy, but omac's allowlist still applies to them. Unsupported entries, such as IP ranges (`10.0.0.0/8`), entries with a port (`example.com:8443`) or `<local>`, are ignored with a warning at every start. If `network.no_proxy` is set, the `NO_PROXY` environment variable is not read.

If you need omac to use a different proxy than what your shell environment specifies, set `network.upstream_proxy` and `network.no_proxy` in the sandbox grants file.
