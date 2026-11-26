---
title: CLI reference
description: Commonly used omac subcommands and flags
---

This page covers the most commonly used subcommands and flags. Run `omac --help` for a list of all subcommands, and `omac <subcommand> --help` for the full flag list of any subcommand.

## Global flags

| Flag | Description |
|---|---|
| `--workdir <dir>` | Set the working directory (default: cwd). |

---

## Sessions

### omac start

Starts skill sidecars and launches the harness (agent CLI) inside the OS sandbox. After the session ends, prints a hint with a session ID you can use to resume with `omac continue -s <id>`.

Refuses to start if any skill in the workdir is unregistered, its source files have changed since registration (review and run `omac register` again to fix), or a required config field cannot be resolved.

```bash
omac start              # default harness (opencode)
omac start claude       # specific harness
```

| Flag | Default | Description |
|---|---|---|
| `--sandbox <profile>` | builtin | Sandbox profile: `builtin` (recommended), `nono` (deprecated), `nono-netprofile` (deprecated), `no-sandbox-debug` (removes all isolation, debug only). |
| `--no-sandbox` | false | Run the harness without sandboxing. Removes all isolation — debug use only. |
| `--ephemeral-cache` | false | Use a temporary cache deleted when the session ends. Use this for a clean build environment or when you do not want the agent's package downloads to persist. |
| `--cache-scope <scope>` | workdir | Which cache to use for the agent's package downloads (npm, pip, cargo, etc.). `global` shares one cache across all your projects, `config` shares it across projects using the same config file, `workdir` gives each project its own isolated cache. See [Cache](../advanced/cache.md). |
| `--open-port <port>` | — | Allow the sandboxed process to bind and connect on this local TCP port (repeatable) for this session only. Useful for a local dev server or an MCP server the harness talks to. To make it permanent, use `network.open_port` in the grants file — see [Opening a port](../configuration.md#opening-a-port). |
| `--no-audit` | false | Disable the security audit trail for this session. |
| `--mcp-unregistered <mode>` | config value | Override `mcp.unregistered` for this run: `allow`, `warn`, `ignore` or `refuse`. `ignore` does not load the harness's own entries and starts. Claude Code supports it with `--strict-mcp-config`; Codex and Copilot CLI support it by turning off each entry omac finds (with a warning per entry) and still refuse what they cannot turn off; OpenCode, pi and CodeWhale fall back to `refuse` with a message. The default is `ignore` for Claude Code, Codex and Copilot CLI and `refuse` otherwise. With Claude Code, `ignore` and `refuse` also start it with `--strict-mcp-config`; `warn` and `allow` do not. See [MCP servers](./mcp.md#existing-mcp-entries-in-the-harness-config) and [Other harnesses](./mcp.md#other-harnesses). |

For all flags: `omac start --help`.

### omac serve

Use this instead of `omac start` when running **OpenCode Desktop**. Unlike `omac start`, which handles one project at a time and requires a restart to switch projects, `omac serve` is designed to stay running and serve multiple projects. Use `--workdir` to specify which project to activate at startup — automatic project switching from OpenCode Desktop is not yet implemented.

```bash
omac serve
omac serve --workdir ~/my-project
```

| Flag | Default | Description                                                                                                                                                                                                                                    |
|---|---|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--workdir <dir>` | — | Activate this directory immediately at startup, so skills are ready as soon as OpenCode Desktop connects. Without this flag, no skills will be active — OpenCode Desktop does not yet automatically tell omac which project is currently open. |
| `--root <dir>` | — | Only allow directories under this path to be activated (repeatable). For example, `--root ~/work --root ~/personal` restricts activation to those two trees. If not set, any directory can be activated.                                       |

| Environment variable | Description |
|---|---|
| `OMAC_SERVE_PASSWORD` | Password for the OpenCode server that `omac serve` starts. Needs omac's own sandbox (or `--no-sandbox`) and can't be combined with `--no-inner`. Without it, OpenCode 2.x picks a new random password on every start, and OpenCode 1.x runs the server without a password. See [Serve mode](../advanced/serve-mode.md#how-to-use-it-today). |

#### OpenCode v2 project discovery on macOS

OpenCode 2.0.x treats denied configuration-discovery probes as fatal. In the
native macOS sandbox, `omac start` and `omac serve` therefore set
`OPENCODE_CONFIG_PROJECT_DISABLE=1` for confirmed 2.0.x versions. This avoids
startup failures and HTTP 500 responses from discovery attempts outside granted
directories.

OpenCode 2.1.0 and later tolerate denied discovery probes, so project-local
config, skills, agents, plugins, and instructions load normally there and omac
does not set the variable. A wrapper spec pinned only to `@2` cannot prove the
minor version, so omac treats it conservatively like 2.0.x and prints a
warning at launch; pin `@2.1` or newer to get project sources back.

When the sandbox profile cannot read `~/.agents`, omac warns as well: OpenCode
2.0.x may fail to start there, and OpenCode 2.1+ silently skips the global
skills stored in that directory. Add `~/.agents` to the profile's
`filesystem.read` to load them.

### omac continue

Re-enters the most recent session for this workdir or a session with a certain ID (omac prints an `omac continue -s <id>` hint when a session ends).

```bash
omac continue              # most recent session
omac continue -s <id>      # specific session by ID
```

### omac resume

Use this when you want to choose from a list of past sessions rather. Shows a numbered picker of recent sessions for the active workdir.

---

## Skill management

### omac register

Prompts for secrets (API keys) and config fields (non-secret settings such as base URLs), stores them securely, and adds the skill to the registry. If the skill includes installation scripts for dependencies, omac prints their paths but never runs them.

```bash
omac register <skill>
omac register <skill> --defaults   # use previously stored values, prompt only for new ones
```

| Flag | Default | Description |
|---|---|---|
| `--defaults` | false | Use previously remembered values without prompting. omac saves values from every registration globally, matched by skill and secret name. The first registration still prompts; after that, `--defaults` reuses them silently in any project. |
| `--no-secrets` | false | Skip secret prompts. Use together with `--secrets-from`, or when you already stored the secrets in a prior step via `omac secrets import` and don't want to be prompted again. |
| `--secrets-from <file>` | — | Read secrets from a `KEY=VALUE` file instead of prompting. Each line must match a secret name declared in the skill's `omac.yaml`. |

**Injecting secrets in CI:**

```bash
# Write CI environment variables to a file, then register non-interactively
echo "API_KEY=$MY_CI_SECRET" > /tmp/secrets.env
omac register <skill> --secrets-from /tmp/secrets.env --no-fields

# Or import into an already-registered skill
omac secrets import <skill> --from /tmp/secrets.env
```

Note: the OS keychain must be running even in CI. On Linux this requires gnome-keyring or an equivalent Secret Service provider — see [Quick start](../getting-started/quick-start.md) for setup.

### omac deregister

Removes a skill's registration and the stored approvals ("Allow always" answers) of that skill, so a different skill registered later under the same name does not inherit them: those of this project, or with `--global` those of every project. The command prints how many it removed; failing to remove them is only a warning. Source files are kept on disk. After deregistering, `omac start` will refuse to run until you re-register the skill or delete its directory.

```bash
omac deregister <skill>
omac deregister <skill> --purge-secrets   # also remove secrets from the keychain
```

| Flag | Default | Description |
|---|---|---|
| `--purge-secrets` | false | Also delete the skill's secrets from the keychain. |

For all flags: `omac deregister --help`.

### omac list

Shows registered skills, their mount points, and whether the sidecar binary is present.

```bash
omac list
omac list --all   # include stale registrations whose directory has been deleted
```

### omac secrets

Manage skill secrets in the OS keychain without re-running the full registration.

```bash
omac secrets list <skill>                      # show which secrets are stored
omac secrets set <skill> <NAME>                # set one secret (prompts for value)
omac secrets unset <skill> <NAME>              # remove one secret from the keychain
omac secrets import <skill> --from <file>      # set multiple secrets from a KEY=VALUE file
```

`omac secrets import` is the recommended way to pre-seed secrets in CI before running `omac register --no-secrets`.

### omac mcp

Manages MCP servers that omac runs on the host for the agent; see [MCP servers](./mcp.md). Run these in a host terminal, not inside the sandbox.

```bash
omac mcp add --harness claude-code --secret JIRA_TOKEN -e JIRA_URL=https://example.atlassian.net jira -- npx -y mcp-jira@1.2.3
omac mcp add-json --harness claude-code --secret JIRA_TOKEN jira '{"command":"npx","args":["-y","mcp-jira@1.2.3"],"env":{"JIRA_TOKEN":"..."}}'
omac mcp tools jira --classify
omac mcp diagnose --probe
```

| Command | What it does | Key flags |
|---|---|---|
| `omac mcp add <name> -- <command> [args...]` | Registers a stdio server. Secrets come from a hidden prompt or `OMAC_SECRET_<NAME>` and live only in the keychain. No tool is callable until its tools are reviewed. | `--scope project\|global` (default `project`), `--harness a,b` (required for a new server; `opencode`, `claude-code`, `codex`, `copilot`, `pi`, `codewhale`), `-e KEY=value` (repeatable, not for credentials), `--secret NAME` (repeatable), `--replace` |
| `omac mcp add-json <name> '<json>'`, `omac mcp add-json --file <f> [names...]` | Registers from an `mcpServers` entry with `command`, `args` and `env`. Remote entries and unknown keys are refused. | `--scope`, `--harness` (same list as `add`), `--secret NAME` (names the `env` keys that are secrets), `--replace`, `--file` |
| `omac mcp register <file>` | Registers a definition file as written, but drops a `reviewed` block (the tools must be reviewed with `omac mcp tools --classify`) and warns when `approvals` is `none` for `write` or `delete`. | `--scope`, `--harness` (required; same list as `add`) |
| `omac mcp tools <name>` | Starts the server on the host and lists its tools with a proposed class, current class and review status, then each tool's description and input parameters (untrusted server text, sanitized, cut at 500 characters). | `--scope`, `--classify`, `--set NAME=CLASS` (repeatable), `--rest CLASS` (default `off`) |
| `omac mcp review <name>` | Shows which tools were added, removed or changed since the last review, with each tool's description and parameters (old and new for a changed tool), refuses while a live tool has no class, asks `[y/N]` and records the current tools as reviewed. | `--scope`, `--yes` |
| `omac mcp diagnose` | Reports load failures, missing secrets, commands not found, unclassified tools, tools not reviewed yet, registrations for a harness that cannot receive MCP, and harness entries omac does not manage (for every harness with a known config location). Exit 0 clean, 1 problem found, 2 misuse. | `--scope`, `--all`, `--probe` (starts the servers, host only) |
| `omac mcp list` | Shows registered servers with tool count, classified count and `reviewed` or `not reviewed`. | `--all` includes stale project scopes |
| `omac mcp show <name>` | Shows one definition: scope, hash, command, access, approvals, when the tools were last reviewed and every tool with its class. Never secret or environment values. | `--scope` |
| `omac mcp deregister <name>` | Removes the server, its secrets and its stored approvals (this project's, or with `--scope global` all projects'), and prints how many approvals it removed. | `--scope` |

Classes are `read`, `write`, `delete` and `off`. All of these refuse to run inside the sandbox. See [MCP servers](./mcp.md) for the details.

### omac approvals

A skill can ask you before the agent calls one of its routes (see [approvals](../skills/authoring.md#approvals-which-routes-run-without-asking)). When you answer "Allow always" in that dialog, omac stores the answer on the host. Run these from a host terminal, outside the sandbox:

```bash
omac approvals list         # each stored answer: id, skill, route, project, date (MCP tools show as mcp:<server>)
omac approvals revoke <id>  # the next request to that route asks again
```

`omac deregister` and `omac mcp deregister` remove the stored answers of the skill or server they remove.

Only "Allow always" answers are stored. Answers "for this session" end when `omac start` or `omac serve` stops.

### omac config

Show or retrieve resolved config values for a skill.

```bash
omac config show <skill>          # all config + secret fingerprints
omac config get <skill> <field>   # one value, suitable for $(...)
```

---

## Operations

### omac setup

Provisions omac's built-in skills into each installed harness's skills directory. Scans `PATH` to find which harnesses are installed. Run once after installing a new harness.

```bash
omac setup              # all harnesses found on PATH
omac setup opencode     # one harness only
```

### omac doctor

Checks that omac and its prerequisites are correctly set up. Prints a specific fix for anything it finds wrong.

### omac diagnose

Reads the audit trail from a previous session and explains what the sandbox blocked and why. Use this when a run fails due to a network or filesystem denial. Does not use an LLM — purely deterministic analysis.

```bash
omac diagnose
```

Requires the audit trail from a previous `omac start` or `omac serve` run. The audit trail is written automatically unless `--no-audit` was passed.

### omac update

```bash
omac update        # prompts before installing
omac update --yes  # skip confirmation
```

### omac cache

The cache stores downloaded packages (npm, pip, cargo, etc.) between sessions, isolated from your regular package caches. Use `omac start --ephemeral-cache` if you want a session with a clean cache that is deleted afterwards.

```bash
omac cache clear        # remove the cache for the current scope
omac cache clear --all  # remove all caches not currently in use
```

### omac provenance

Shows what your current sandbox configuration allows and blocks — useful for verifying your effective policy before a run.

```bash
omac provenance          # show full policy
omac provenance --check  # report risky grants only
```

### omac audit

The audit trail is written automatically during `omac start` and `omac serve` — there is nothing to run manually. Use `omac diagnose` to read and interpret the trail. To disable or redirect audit logging, pass `--no-audit` or `--audit-log <path>` to `omac start` or `omac serve`.

---

## Low-level

### omac sandbox

Runs any command inside the OS sandbox without a full agent session — no skills, no facade. Use this to test your sandbox configuration or debug what a sandboxed process can access.

```bash
omac sandbox run -- ls ~                       # check which home directory files are visible
omac sandbox run -- curl https://example.com   # test network access
```

For all available flags: `omac sandbox run --help`.

### omac version

```bash
omac version
```
