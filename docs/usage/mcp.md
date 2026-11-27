---
title: MCP servers
description: Running MCP servers through the omac gateway so the agent never holds their credentials.
---

An MCP server gives the agent typed tools for an external service, such as Jira or GitHub. The omac MCP gateway runs such a server on your machine, outside the sandbox, with its credentials. The agent gets the tools you have reviewed and never gets the credential.

MCP in omac is for reaching something outside the sandbox with credentials, OAuth tokens included. Outside omac, use your harness's own MCP config as usual. Inside omac, MCP configuration comes only from the omac registry, which omac translates into the harness's configuration at launch.

## When to use it

| You want | Use |
|---|---|
| The agent to call a service with a secret, and the secret must stay out of the sandbox, and a stdio MCP server or a Streamable HTTP server with a bearer token for it exists | The MCP gateway (this page) |
| The same, and you write the integration yourself or one exists as a skill | A [skill](./skills.md) |
| An MCP server with no secret, or you accept that the agent can read its token, or a server that needs OAuth | Not supported by the gateway yet (OAuth is planned; a server with no secret can be registered by URL). omac does not load the harness's own MCP entries by default (Claude Code, and Codex and Copilot CLI for the entries it can turn off) or refuses to start with them (the others, and what it cannot turn off), so run once with `--mcp-unregistered=allow`; the server then runs inside the sandbox. See [Existing MCP entries](#existing-mcp-entries-in-the-harness-config) and [Running an MCP server the harness launches](../configuration.md#running-an-mcp-server-the-harness-launches) |

If you register no MCP server, nothing changes. omac does not touch the harness's MCP configuration. Once a server is registered for this project or globally, for any harness, the rule applies to every harness you start in it: the harness's own MCP entries are not loaded (Claude Code; Codex and Copilot CLI for the entries omac can turn off by name) or refused, because inside omac MCP configuration comes only from the registry. OpenCode, Claude Code, Codex, Copilot CLI, pi and CodeWhale can all receive registered servers. See [Other harnesses](#other-harnesses).

## Safety and degradation by harness

omac aims to be as safe as the harness allows. The harnesses differ in what they let omac control, so the protection differs. Read the row for the harness you use, above all if it is experimental.

The goal is the same everywhere: inside omac, MCP servers come only from the registry, because a server in the harness's own config can reach credentials the agent can read. How far omac gets depends on the harness. When it cannot fully get there, it degrades in this order: it turns the harness's own entries off and warns, or it refuses to start and says what to resolve, and it never loads them silently. `--mcp-unregistered=allow` is the explicit way to accept the risk for one run.

| Harness | Tier | Default | What omac does with the harness's own MCP entries | What stays open |
|---|---|---|---|---|
| Claude Code | priority | `ignore` | Loads none of them: `--strict-mcp-config`, plus `--no-chrome`. Only the registered servers load. | Whether strict mode also skips claude.ai connectors is not documented and needs a check with a real session. |
| OpenCode (v2; v1 is legacy) | priority | `refuse` | Cannot switch entries off. Refuses to start while the scan finds one. | An MCP config location omac does not know loads silently, and so do remote and account-level sources. |
| Codex | priority | `ignore` | Switches off, by name, each entry found in your own `config.toml`, with a warning. Refuses for project entries, plugin or administrator entries, same-name entries and files it cannot read. | An unknown config location is neither found nor switched off. The switch was read in the documentation and source, not run. |
| Copilot CLI | priority | `ignore` | Switches off, by name, each entry found, with a warning. Refuses for plugin entries, same-name entries and files it cannot read. | An unknown location is not found. Copilot's built-in GitHub MCP server and servers from your account or organisation policy cannot be read by omac and stay active. The switch was read, not run. |
| pi (experimental) | experimental | `refuse` | No switch for one entry. Refuses to start while the scan finds an entry. | Unknown locations load. The registered servers arrive through an extension (`-e`) that runs inside pi with pi's permissions. Under `warn` or `allow`, a same-name entry in pi's `mcp.json` wins over the registered one. |
| CodeWhale (experimental) | experimental | `refuse` | No switch for one entry. Refuses to start while the scan finds an entry. | The project file `.codewhale/mcp.json` (after your approval in CodeWhale) and plugin bundles load. Under `warn` or `allow`, an approved project file wins over a same-name registered server. |

What this means in practice:
- **Where omac switches an entry off, the entry and any token in it stay in the file** and the agent can read them. Rotate the token, or register the server so the credential stays on the host.
- **The scan only knows the locations in [Where omac looks](#where-omac-looks).** They come from each harness's documentation and are researched again at regular intervals. A location that is missing is a gap, see [Known gap](#known-gap-config-locations-omac-does-not-know).
- **None of the Codex, Copilot CLI, pi and CodeWhale mechanisms has been run against the real harness.** Their injection and switches were checked against documentation and source code only.
- **Experimental harnesses may lag behind.** A feature that is missing there degrades as described and is not a promise that the gap will be closed. omac prints a warning at every start of an experimental harness (pi, CodeWhale) and of OpenCode v1.

## How it works

```
 host (outside the sandbox)                      sandbox
 ┌───────────────────────────────┐              ┌────────────────────────┐
 │ MCP server process            │              │ harness (OpenCode,     │
 │   env: your secrets           │              │   Claude Code, ...)    │
 │        ▲ stdin/stdout         │              │        │ starts        │
 │ omac gateway                  │◄─ omac proxy ┤ "omac mcp bridge <name>"│
 └───────────────────────────────┘   (facade)   └────────────────────────┘
```

- You register the server once and review its tools once. The definition, the classes, the review record and the secrets are stored on the host, where the sandbox cannot read them.
- At `omac start`, omac runs the server on the host and gives the harness a small bridge command instead of the real server.
- The bridge only passes messages through omac's proxy. The proxy checks every call against your classes before it reaches the gateway, and the gateway checks again. The agent sees the tools, never the credential.

## Registering a server

Run these in a normal terminal on your machine, not inside the agent's sandbox. All registration commands refuse to run when they detect they are inside an omac session. Registering only stores the server. **No tool is callable until you have reviewed its tools** (see [Reviewing tools and detecting changes](#reviewing-tools-and-detecting-changes)).

There are three ways to register, with the same result. A server reached over HTTP is registered by URL, see [Remote servers](#remote-servers-streamable-http).

**1. From the command line.** Everything after `--` is the command that starts the server:

```bash
omac mcp add --harness claude-code --secret DEMO_TOKEN -e DEMO_REGION=eu-west-1 \
  demo -- npx -y @modelcontextprotocol/server-everything@2025.1.14
```

`-e KEY=value` sets a fixed, non-secret environment variable. An empty value or a `${VAR}` reference is refused with "declare --secret KEY": a credential must never be written into the definition. `--secret NAME` declares a secret the server needs. omac asks for its value with a hidden prompt, or reads `OMAC_SECRET_<NAME>` from the environment, and stores it in the OS keychain only.

**2. From an `mcpServers` entry** as most harnesses and server READMEs show it:

```bash
omac mcp add-json --harness claude-code --secret DEMO_TOKEN demo \
  '{"command":"npx","args":["-y","@modelcontextprotocol/server-everything@2025.1.14"],"env":{"DEMO_TOKEN":"abc"}}'
omac mcp add-json --harness claude-code --secret GITHUB_TOKEN --file mcp.json [names...]
```

The JSON is one entry, or an `{"mcpServers": {...}}` object. With `--file`, name the servers to take, or leave the names out to take all. An entry has `command`, `args`, `env` and optionally `"type": "stdio"`. Anything else is refused: unknown keys, and any remote entry (`url`, `httpUrl`, `serverUrl`, `headers`, or a type other than stdio) with "remote MCP servers are not supported yet". An `env` key you name with `--secret` becomes a secret and its value from the JSON goes to the keychain; every other `env` value is stored in the definition and must be a literal that is not a credential.

**3. From a definition file**, when you need `mode`, `env_passthrough` or a prepared policy:

```bash
omac mcp register ./mcp.yaml --harness opencode,claude-code
```

`register` stores the file as written, with one exception: a `reviewed` block is dropped (a file must not pre-approve its own tool list) and `register` prints a notice when it does. After registering, run `omac mcp tools <name> --classify` to review the live tools. Classes and `approvals` are kept, so read a definition file before you register it, above all one from a repository or written by an agent: a file that sets `approvals` to `none` for `write` or `delete` lets the agent call those tools without asking you, and `register` prints a warning for it. Classes and approvals that are not in the file are gone, so to change only the command or environment of a registered server use `omac mcp add --replace` instead. The [definition reference](#definition-reference) lists the fields.

Options shared by all three:

- **`--scope`**: `project` (default) applies to the project in the current directory. `global` applies to every project. If both exist under one name, the project entry wins and omac prints a notice. A project registration is tied to the directory's path, so moving or renaming the directory leaves it behind (visible with `omac mcp list --all`), and each git worktree is its own scope. Run the command in the project directory you mean.
- **`--harness`**: required for a new server. A comma-separated list of harnesses (for example `opencode,claude-code`; any of `opencode`, `claude-code`, `codex`, `copilot`, `pi`, `codewhale`). A server is only injected into the harnesses you list.
- **`--replace`** (`add` and `add-json`): changes the command, environment and secrets of a registered server and keeps its tools, approvals, access and review record. The harness list cannot change this way; deregister first.

On success `add` prints that no tool is callable yet and the next command: `omac mcp tools NAME --classify`.

```bash
omac mcp list                  # servers, scope, harnesses, tool count, classified count, reviewed or not reviewed
omac mcp show demo             # one server in full
omac mcp deregister demo       # removes the registration, its secrets from the keychain and its stored approvals
```

`omac mcp show` prints the command, the names of the `env` keys, the secrets with `stored` or `missing`, the access mode, the approvals, when the tools were last reviewed (date, tool count and the first 12 hex digits of the fingerprint) and every tool with its class. It never prints a secret or an environment value.

### Where things are stored

- Definitions and snapshots: `~/.config/omac/mcp/<scope>/<name>/` (under `$XDG_CONFIG_HOME/omac` if you set it). The sandbox cannot read or write this directory.
- Secrets: the OS keychain, under the service name `omac-mcp/<scope>/<name>`. `omac mcp deregister` deletes them.
- omac never reads a definition from your project. Only the registration commands create one.

### Hash and snapshot

At registration omac writes the canonical form of the definition into a snapshot and records the sha256 hash of that canonical form (not of your file's bytes, so comments and key order in your file do not matter). At every start, omac checks the snapshot against the hash and runs the server from the snapshot, not from your file. If you edit `mcp.yaml` afterwards, nothing changes until you register it again. The server runs with the snapshot directory as its working directory (never your project, because tools such as `npx` and `uvx` read config files like `.npmrc` from the working directory, and the agent can write those) and with a cleaned environment: omac's own variables are removed, and only the `server.env`, `secrets`, `mode` and `env_passthrough` entries are added, plus a small base set (`PATH`, `HOME`, `USER`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TMPDIR`).

### Remote servers (Streamable HTTP)

A server that is already running somewhere, on your machine or on the network, is registered by URL:

```bash
omac mcp add --harness claude-code --url https://mcp.example.com/mcp --bearer-secret EXAMPLE_TOKEN example
omac mcp add --harness claude-code --url http://localhost:8931/mcp local-tools
```

The gateway on your host connects to the server, not the sandbox, so the bearer token stays on the host like any other secret: omac asks for its value (or reads `OMAC_SECRET_<NAME>`), keeps it in the keychain and sends it as `Authorization: Bearer <token>`. Tool classes, approvals, review and audit work exactly as for a stdio server.

- **`--url`** must be `https`. Plain `http` is accepted only for `localhost`, `127.0.0.1` and `::1`. A URL with user info, a query or a fragment is refused, because a token in a URL would be stored in the definition.
- **`--bearer-secret NAME`** names the secret. It is optional for a server without authentication. Other credential headers (such as `X-Api-Key`) are not supported yet.
- **`-H 'Name: value'`** adds a literal header that is not a credential. A header whose name looks like a credential (`Authorization`, anything with `token`, `key`, `secret`, `cookie`, `password`) is refused.
- **Redirects are not followed**, because they would carry the token to another host. A redirect is an error.
- **Private addresses are refused.** The gateway checks the address the name resolves to when it connects. A host that resolves to a private, link-local or carrier-grade-NAT address (`10/8`, `172.16/12`, `192.168/16`, `169.254/16`, `100.64/10`, unique-local IPv6) is refused, so a hostile or rebinding DNS answer cannot point your token at an internal service. A server inside your company network needs `--allow-private-network` for that server. Loopback is allowed only when the URL itself names a loopback host. If an HTTP proxy is set in the environment of `omac`, the proxy resolves the name and this check sees the proxy, not the target.
- **The network prompt does not cover this connection yet.** The gateway runs on the host, outside the sandbox, so omac's domain allow/deny list and prompt for sandboxed traffic do not apply to it. Registering the server is the approval: check the URL before you register it.

Not supported yet: OAuth, the legacy SSE transport, resuming a dropped event stream, and registering a remote entry with `add-json` or from a harness config.

## Reviewing tools and detecting changes

A server announces its tools by itself, and it can change them at any time. So omac does not trust the announcement: you review the tools once, give each a class, and omac records a fingerprint of what you saw.

When you review a server's tools, omac records a fingerprint of their names, descriptions and input schemas. If the server later changes any of them (an update, or a compromised server changing what a tool does), omac blocks every call until you review again. Tool descriptions are untrusted text: a review is how you decide to trust the current ones.

```bash
omac mcp tools demo                    # list the tools, change nothing
omac mcp tools demo --classify         # review, classify and record as reviewed in one step
omac mcp review demo                   # after a change: see what changed and review again
```

`omac mcp tools` starts the server on the host with its secrets and lists its tools: a proposed class and the reason, the current class and the review status (`reviewed`, `changed`, `new`, `removed`). The proposal comes from the annotations the server attaches to its tools. They are claims, not facts, so read each tool yourself. Below the table, every tool is listed with its description and a summary of its input parameters (name, type, and `required`), so you can read what a tool does before you classify it. That text comes from the server and is untrusted: control and escape characters are removed, it is shown on one line and cut at 500 characters, and it may still try to instruct the agent, so judge it as you would any text from a third party.

| Class | Meaning |
|---|---|
| `read` | Only reads. |
| `write` | Changes something. |
| `delete` | Deletes or is destructive. This is also the proposal when no annotation says the tool is safe. |
| `off` | Reviewed, but hidden from the agent and refused. |

`--classify` asks for a class per tool (Enter takes the proposal) and then writes the classes and records the tools as reviewed in one step. To script it, give `--set NAME=CLASS` (repeatable) and `--rest CLASS` for all other tools. `--rest` defaults to `off`, so a script that names only the tools it wants cannot enable the rest by accident.

`omac mcp review` starts the server, shows which tools were added, removed or changed since the last review (for an added tool its description and parameters, for a changed tool the description and parameters from the last review next to the current ones, for a removed tool its last reviewed text), refuses while a live tool has no class, and asks `Accept the current tools of <name> as reviewed? [y/N]` (`--yes` skips the question). When nothing changed it says so and writes nothing. Both commands run only on the host. After either, and after any registration change, a running `omac start` session restarts that server's gateway by itself.

## Definition reference

Written by the commands above, or by hand for `omac mcp register`:

```yaml
name: demo
server:
  command: [npx, -y, "@modelcontextprotocol/server-everything@2025.1.14"]
  env:
    DEMO_REGION: eu-west-1
  env_passthrough: [HTTPS_PROXY]
secrets: [DEMO_TOKEN]
mode:
  name: DEMO_MODE
  value: read-only
access: read-write
tools:
  - {name: echo, class: read}
  - {name: add, class: write}
approvals:
  write: required
```

| Field | Meaning |
|---|---|
| `name` | Name of the server. The harness shows it, and you use it in `omac mcp show <name>`. |
| `server.url` | Instead of `server.command`: the endpoint of a Streamable HTTP server (`https`, or `http` for a loopback host). |
| `server.bearer_secret` | With `server.url`: the declared secret sent as `Authorization: Bearer`. |
| `server.headers` | With `server.url`: literal, non-credential request headers. |
| `server.allow_private_network` | With `server.url`: allow the host to resolve to a private address. Default false. |
| `server.command` | The command that starts a stdio MCP server. Use an absolute path or a command found on `PATH` (such as `npx` or `uvx`). Relative paths and absolute paths inside your project are refused. A program name that `PATH` resolves to a file inside your project (for example through `node_modules/.bin` on `PATH`) is refused when the gateway starts, and the gateway runs the resolved path for the whole session. |
| `server.env` | Fixed, non-secret environment variables for the server. |
| `server.env_passthrough` | Names of host environment variables to hand to the server as they are, for example a proxy setting. |
| `secrets` | Names of secrets the server needs. omac asks for them at registration, stores them in the OS keychain and sets them as environment variables of the server process only. |
| `mode` | Optional. One environment variable (`name`, `value`) that some servers use to switch between, for example, read-only and read-write. |
| `access` | `read-only`, `read-write` or `read-write-delete` (default). Tools of a class above the access mode are hidden and refused. |
| `tools` | The classes: a list of `name` and `class` (`read`, `write`, `delete`, `off`). A tool that is not listed is refused. |
| `approvals` | Whether a call asks you first, per class: `none` or `required`. Defaults: `read: none`, `write: required`, `delete: required`. |
| `reviewed` | `tools_hash`, `tools` (a hash per tool), `text` (the description and parameter summary of each tool as you saw it, capped, so a later review can show what changed; reviews recorded before this existed have none and show only that the fingerprint differs) and `at` (when the tools were reviewed). Written by omac. Do not edit it by hand: it is rewritten by `omac mcp tools --classify` and `omac mcp review`. |

A definition sets either `server.command` (a stdio server the gateway starts) or `server.url` (a Streamable HTTP server it connects to), not both. `server.env`, `server.env_passthrough` and `mode` apply to commands only.

## What omac does at launch

For each registered server that matches the harness, omac starts the server on the host before the harness and injects an entry that starts the bridge. It never edits your own config files.

- **OpenCode**: omac passes the entry in the `OPENCODE_CONFIG_CONTENT` environment variable, which takes precedence over project config.
- **Claude Code**: omac writes a config file to the session's temporary directory and passes it with `--mcp-config`. With the default `ignore` mode (and with `refuse`) it also passes `--strict-mcp-config`, see [Claude Code runs in strict MCP mode](#claude-code-runs-in-strict-mcp-mode).
- **Codex**: omac passes one `-c mcp_servers.<name>={command = ..., args = [...], env_vars = [...]}` override per server and sets the bridge variables in the harness environment, so no value is on the command line. Server names may use only letters, digits, `-` and `_`.
- **Copilot CLI**: omac writes a config file to the session's temporary directory and passes it with `--additional-mcp-config @<file>`. If you pass `--additional-mcp-config` yourself, omac refuses to start with registered servers instead of combining them. A bridge value that contains `$` is refused, because Copilot CLI expands `$VAR` and `${VAR}` in `env` and documents no escape.
- **pi**: omac writes a small extension to the session's temporary directory and loads it with `-e <file>`; the extension calls `pi.registerMcpServer(name, config)` for each server. No value is in the extension: the bridge variables are set in the harness environment, which the server process inherits. Server names may use only letters, digits, `-` and `_`, and two names that differ only in `-` versus `_` are refused. pi 0.99.0 or later is needed (the version that added `registerMcpServer`).
- **CodeWhale**: omac writes a config file to the session's temporary directory and names it in `CODEWHALE_MCP_CONFIG`, which CodeWhale reads in place of `~/.codewhale/mcp.json`. A bridge value that contains `${` is refused, because CodeWhale expands `${NAME}` in `env`.

None of the Codex, Copilot CLI, pi and CodeWhale injections, and neither Codex's nor Copilot CLI's switch for turning an entry off, has been run against the real harness. Their mechanisms and entry fields were checked against each harness's documentation and source code (see [Other harnesses](#other-harnesses) for the sources); that is not a test of the running tool, and omac's own tests only check what it passes.

omac adds these variables to the sandbox only when it injects, so a value you export yourself does not reach the agent.

If a secret is missing from the keychain, omac prints a warning and skips that server. The rest of the session starts normally. `omac serve` does not run or inject MCP servers; it prints a notice and you use `omac start` for them.

### While a session runs

- **Changed registration.** After `omac mcp review`, `omac mcp tools --classify` or a changed registration, the running session restarts that server's gateway and picks up the new classes and review.
- **A new server needs a restart.** The harness reads its MCP configuration once at launch, so a server you register during a session is only reported (a notice says to restart). A server you deregister keeps running until the session ends.
- **A gateway that dies** is restarted with a growing delay and a fresh secret, and the bridge reconnects. A request in flight when it died fails with an error. After 5 restarts within 10 minutes omac gives up: calls to that server get HTTP 502 (`skill-broken`) until you restart the session or a changed registration refreshes it.
- **Cleanup.** On Linux, a gateway that is killed with SIGKILL takes its server process with it. On macOS there is no equivalent, so the server process relies on exiting when its stdin closes.

## Existing MCP entries in the harness config

Outside omac, use your harness's own MCP config as usual. Inside omac, MCP configuration comes only from the omac registry; omac translates it into the harness's configuration at launch.

An MCP entry in the harness's own config (for example `.mcp.json`, `opencode.json`, `~/.claude.json`) either runs inside the sandbox or keeps its login in the harness's state directory. In both cases the agent can reach its credentials. omac therefore does not let such an entry load: Claude Code ignores it by default (`ignore` mode, strict MCP mode), Codex and Copilot CLI turn it off by name for the run and warn (`ignore` mode; see [Other harnesses](#other-harnesses) for what they cannot turn off), and every other harness refuses to start while it exists (`refuse` mode). This includes an entry with the same name as a registered server: the old entry, and any token in it, stays in a file the agent can read, so registering the server does not make it safe. omac never removes or rewrites these entries. It only adds your registered servers next to them, and with `ignore` it asks the harness, for this run only, not to load the others.

One harness config cannot serve a server with credentials both inside and outside omac. To use a server in both places, keep the harness entry out of the files omac scans and register the server with omac, or run omac once with `--mcp-unregistered=allow`. Under `allow` or `warn`, if a harness entry has the same name as a registered server, the harness normally uses the entry omac injects, but pi and an approved CodeWhale project file are documented to prefer their own entry, and Codex merges the two (see [Other harnesses](#other-harnesses)).

### Where omac looks

The scan reads server names, transport, command, arguments and the names (never the values) of environment variables and headers. In `refuse` mode a file that exists but cannot be read or parsed also stops the start, because it may hide servers. A missing file is ignored. "Walks up" means the current directory and every parent directory up to the filesystem root.

| Harness | Place | Status |
| --- | --- | --- |
| OpenCode | `opencode.json` / `opencode.jsonc` in the user config directory (`$XDG_CONFIG_HOME/opencode` or `~/.config/opencode`), in `$OPENCODE_CONFIG_DIR`, and in the current directory and every parent (walks up), also inside each `.opencode/` directory | scanned |
| OpenCode | file named by `$OPENCODE_CONFIG`; inline JSON in `$OPENCODE_CONFIG_CONTENT` | scanned when set on the host |
| OpenCode | administrator files `/Library/Application Support/opencode/` and `/etc/opencode/` | scanned, reported as administrator-managed |
| OpenCode | both config shapes: `mcp.<name>` (v1) and `mcp.servers.<name>` (v2); an entry with `enabled: false` is not reported when every place that mentions the name turns it off, and is reported when any other definition or `enabled: true` override of that name exists | scanned |
| OpenCode | remote `.well-known/opencode` config, macOS MDM managed preferences, Windows `%ProgramData%\opencode`, MCP servers that a plugin registers in code | not scanned (not a file omac can read at start) |
| Claude Code | `~/.claude.json`, user scope and the entry of the current project only (other projects are never reported), also `$CLAUDE_CONFIG_DIR/.claude.json` | scanned |
| Claude Code | `.mcp.json` in the current directory and every parent (walks up) | scanned |
| Claude Code | `settings.json` in `~/.claude` and `$CLAUDE_CONFIG_DIR`, and `.claude/settings.json`, `.claude/settings.local.json` of the project (`mcpServers` key) | scanned |
| Claude Code | entries Claude Code does not load | not reported: servers in `disabledMcpjsonServers` (any `settings.json`) for `.mcp.json`, and servers in this project's `disabledMcpServers` (the `/mcp` toggle, kept in `~/.claude.json`) for `~/.claude.json` and `settings.json` entries. Not skipped: servers that are merely not approved yet (print mode loads them without asking), and a per-server `enabled`/`disabled` key, which Claude Code does not document |
| Claude Code | `--mcp-config` files or inline JSON that you pass after the omac flags | scanned |
| Claude Code | `mcpServers` in the frontmatter of agent files in `.claude/agents/` (current directory and parents), `~/.claude/agents/`, `$CLAUDE_CONFIG_DIR/agents/` and `.claude/agents/` of `--add-dir` directories; `--agents` JSON | scanned |
| Claude Code | a plugin directory passed with `--plugin-dir`: its `.mcp.json` and `mcpServers` in `.claude-plugin/plugin.json` (inline or a `.json` file) | scanned (not for `.zip` files, `.mcpb` bundles or URLs) |
| Claude Code | `managed-mcp.json` and `managedMcpServers` in `managed-settings.json` under `/Library/Application Support/ClaudeCode/` and `/etc/claude-code/` | scanned, reported as administrator-managed; the settings file name is unverified because the pages we read name only the directory |
| Claude Code | plugin directories in `~/.claude/plugins` and `$CLAUDE_CONFIG_DIR/plugins`: every `.mcp.json` and `.claude-plugin/plugin.json` (`mcpServers`) under `synced/`, and the plugin install paths listed in `installed_plugins.json` when that file exists. Reported as provided by a plugin. A plugin that `enabledPlugins` in the user, project or local `settings.json` turns off (`false`) is skipped; a `synced` plugin is matched as `<name>@synced`, with the name from its `.claude-plugin/plugin.json`. A plugin whose state cannot be determined is scanned | scanned. The layout is observed on one machine, not documented, and the shape of `installed_plugins.json` is unverified: omac reads every `installPath` string in it, and a file whose `plugins` entry yields no path is reported as unparseable |
| Claude Code | `plugins/marketplaces/` | not scanned (a catalog of plugins you can install, not plugins Claude Code loads) |
| Codex | `config.toml` in `$CODEX_HOME` (default `~/.codex`); `.codex/config.toml` in the current directory and each parent up to the git root; administrator file `/etc/codex/config.toml` (reported as administrator-managed). Tables `[mcp_servers.<name>]` and `[plugins."<plugin>".mcp_servers.<name>]` | scanned |
| Codex | `-c mcp_servers.*` and `--config mcp_servers.*` that you pass after the omac flags | scanned |
| Codex | `<name>.config.toml` profile files, cloud-managed config, MCP servers inside plugin bundles | not scanned (profile files: unverified whether they may hold `mcp_servers`) |
| Copilot CLI | `mcp-config.json` in `$COPILOT_HOME` (default `~/.copilot`); `.mcp.json` and `.github/mcp.json` in the current directory and each parent up to the git root | scanned |
| Copilot CLI | `--additional-mcp-config` as inline JSON or `@file` that you pass after the omac flags | scanned |
| Copilot CLI | `mcp.json` files under `<copilot home>/installed-plugins/`. Reported as provided by a plugin | scanned (inline `mcpServers` in a plugin manifest and in agent frontmatter are not read) |
| Copilot CLI | the built-in GitHub server, account and organization policy | not scanned (not files) |
| pi | `mcp.json` in `$PI_CODING_AGENT_DIR` (default `~/.pi/agent`) and `.pi/mcp.json` in the current directory (parents are not searched) | scanned; pi's documented key is `mcpServers`, omac also reads `servers` |
| pi | servers an extension registers in code, the `pi-mcp-adapter` extension's own config | not scanned |
| CodeWhale | the file named by `$CODEWHALE_MCP_CONFIG` or the legacy `$DEEPSEEK_MCP_CONFIG`; `~/.codewhale/mcp.json` and `$CODEWHALE_HOME/mcp.json` (CodeWhale's default path follows `$CODEWHALE_HOME`); `~/.deepseek/mcp.json` only when `~/.codewhale/mcp.json` does not exist; `.codewhale/mcp.json` in the current directory; the file `mcp_config_path` names in `config.toml` | scanned; both the `servers` and the `mcpServers` key are read, and a file with both is reported as unparseable, as CodeWhale does |
| CodeWhale | servers from plugin bundles | not scanned |
| Claude Code | claude.ai connectors | not scanned (fetched from your account over the network); turn them off with `ENABLE_CLAUDEAI_MCP_SERVERS=false` or the setting `disableClaudeAiConnectors` |
| Claude Code | server-managed settings, MDM and registry policy, Windows paths | not scanned (not a file omac can read at start) |

"Up to the git root" means the current directory and each parent up to the first directory that contains `.git`; without a git root only the current directory counts.

Environment variables and `--mcp-config` files are scanned whenever they are set on the host, even if the harness would ignore them (for example with `--strict-mcp-config`). In `ignore` mode, entries from a `--mcp-config` you pass yourself still load under strict mode, so they are listed as loaded warnings and not as ignored. To get past a refusal because of them, unset the variable, drop the flag or use `--mcp-unregistered=allow`. An entry from an administrator-managed file cannot be removed by you, so `--mcp-unregistered=allow` is the way past it.

The refusal, the warning and the ignore notice print, under each stdio entry, an equivalent command, for example:

```text
omac mcp add --scope project --harness claude-code --secret JIRA_TOKEN jira -- npx -y mcp-jira@1.2.3
```

Every `env` key of the entry becomes a `--secret`; no `env` or header value is ever printed. The command arguments are copied as they are, so check them for a token before you run or share the line. A remote entry gets "remote MCP servers are not supported yet", and an entry that cannot be expressed this way points to a definition file and `omac mcp register`.

Fix a refusal in one of three ways:

- **Register the server** with `omac mcp add` (or `register`), so its credentials stay on the host.
- **Remove the entry** from the harness config.
- **Allow it for one run** with `omac start --mcp-unregistered=allow`.

If a refused or loaded entry already holds a token, rotate it: agents that ran earlier may have read it.

Only stdio servers are supported, so a remote or OAuth server cannot be registered yet. A remote entry is listed with "remote MCP servers are not supported yet" and no registration advice. To use it, run with `--mcp-unregistered=allow`; it then runs inside the sandbox.

`mcp.unregistered` sets the default:

| Value | Effect |
|---|---|
| `ignore` (default for Claude Code, Codex and Copilot CLI) | The harness does not load its own entries. For Claude Code, `omac start` proceeds and prints a notice that lists each entry and its file as ignored by strict mode (`--strict-mcp-config`). For Codex and Copilot CLI, omac turns off each entry it found by name and prints a warning for it; an entry it cannot turn off, or a file it cannot parse, still stops the start as in `refuse`. |
| `refuse` (default for OpenCode, pi and CodeWhale) | `omac start` stops and names each entry and its file. A config file omac cannot parse also stops it, because it may hide entries. |
| `warn` | The entries load. Starts and prints a warning for each entry and each unparseable file. |
| `allow` | The entries load. Starts without comment. |

`ignore` is available for Claude Code, Codex and Copilot CLI. For OpenCode, pi and CodeWhale an explicit `ignore` (setting or flag) falls back to `refuse` and omac prints `ignore is not available for <harness>, which cannot suppress its own MCP config; using refuse`. An explicit setting always wins over the default: `refuse` on Claude Code refuses, and `ignore` on OpenCode refuses with the message. OpenCode cannot switch off a single entry from the config omac passes: its MCP schema knows only complete local and remote entries.

It goes in the user-global launcher config, `~/.config/omac/config.yaml`:

```yaml
mcp:
  unregistered: ignore          # allow | warn | ignore | refuse (default: ignore for Claude Code, refuse otherwise)
```

- **Project-local files cannot set this.** A project's `.opencode/oh-my-agentic-coder.yaml` is ignored for `mcp:`, so opening a repository cannot switch the check off.
- **Where it is read from.** `~/.config/omac/config.yaml`, or `$XDG_CONFIG_HOME/omac/config.yaml` when that variable is set. The registry uses the same directory.
- **One run:** `omac start --mcp-unregistered=<allow|warn|ignore|refuse>` overrides the setting for that run. This is the way past a refusal caused by a checked-in `.mcp.json`.
- The check of harness entries applies only once a server is registered or you set the value or flag explicitly. With nothing registered and nothing set, omac does not refuse because of your MCP config. Claude Code still starts in strict MCP mode (its default `ignore`), see below.

### Claude Code runs in strict MCP mode

In the default `ignore` mode, and in `refuse` mode, omac starts Claude Code with `--strict-mcp-config`, also when nothing is registered. Claude Code then loads only the MCP servers passed with `--mcp-config`: the registered servers omac injects. It no longer loads servers from `.mcp.json`, `~/.claude.json`, settings files or plugins, so a plugin bundle's servers do not start inside omac. When the strict flag is the only change and the scan finds entries, omac prints one line naming them (names only, at most 10) so you know they are ignored. For remote entries the line says that remote servers are not supported yet instead of advising to register them.

The Claude Code documentation does not say whether strict mode also skips the claude.ai connectors (servers from your Claude account). Check it with a real session before you rely on it; omac does not claim it.

- **Keep Claude's own servers:** run with `--mcp-unregistered=warn` or `allow` (or set `mcp.unregistered`). omac then passes no strict flag and leaves Claude's MCP entries as they are.
- **Your own `--mcp-config` argument** after `--` still loads when nothing is registered, because you chose it. With a registered server or an explicit mode, omac scans it as before.
- A `--strict-mcp-config` you pass yourself is not repeated.
- Claude Code's built-in Claude in Chrome integration is an MCP server that strict mode does not cover, and it gives the agent your browser sessions. In these two modes omac therefore also passes `--no-chrome`. To use it anyway, pass `--chrome` yourself (a `--chrome` or `--no-chrome` you pass is not repeated), or use `--mcp-unregistered=warn` or `allow`.
- OpenCode and the other harnesses are not affected.

### Keeping plugins or servers you have to use

omac disables what Claude Code would load on its own by default. That is the right default for your own work. It is not what you want when your organization or a project requires Claude plugins or MCP servers that you have to use. To skip the disabling:

- **For one run:** `omac start claude --mcp-unregistered=allow`. `allow` starts silently. `warn` starts too, and lists Claude's own MCP servers first so you see what is loaded.
- **Always, for yourself:** set it in your global config, `~/.config/omac/config.yaml` (or `$XDG_CONFIG_HOME/omac/config.yaml`):

  ```yaml
  mcp:
    unregistered: allow
  ```

- **Not from a project.** A project's `.opencode/oh-my-agentic-coder.yaml` cannot set this, so a repository you open cannot switch the protection off. The setting is yours.
- **Different setups for different work.** The global config applies to everything you start. If you want the protection for your own work and the opt-out only for some projects, keep the default and start those sessions with `--mcp-unregistered=allow`, for example through a shell alias such as `alias omac-claude-open='omac start claude --mcp-unregistered=allow'`.

omac passes no strict flag and nothing else that hides Claude's own servers in `warn` and `allow` modes. Servers you registered with `omac mcp add` are still added next to them.

What you give up when you skip it:

- The plugin and config servers load inside the sandbox. Any credential or OAuth login such a server uses is kept in Claude Code's own state directory, which the agent can read.
- Claude Code may load your claude.ai connectors too (see the note above).
- omac's network proxy still asks before the agent reaches a new host, so you can deny a host, but omac sees no tool names and applies no per-tool approvals for those servers.

For a server that needs a credential, the better way is to register it, so its secret stays outside the sandbox: run the `omac mcp add` command that omac prints for it, or `omac mcp add-json`. Keep `allow` for servers that need no secret.

## Other harnesses

omac supports six harnesses. OpenCode v2, Claude Code, Codex and Copilot CLI are the priority harnesses; OpenCode v1 is legacy and pi and CodeWhale are experimental and may lag behind. For a harness that cannot suppress its own MCP config omac does not load those entries silently: it refuses to start (the default `refuse`) until you remove them or accept them with `--mcp-unregistered`. OpenCode and Claude Code are described above. Codex, Copilot CLI, pi and CodeWhale can receive registered servers too (see [What omac does at launch](#what-omac-does-at-launch)), but none of them has a documented way to turn off its own MCP config completely. Codex and Copilot CLI have a switch for one server by name, so omac uses it for the entries it finds (`ignore`, their default): `-c mcp_servers.<name>.enabled=false` for Codex and `--disable-mcp-server <name>` for Copilot CLI, with a warning for each. pi and CodeWhale have no such switch, so for them `ignore` falls back to `refuse` with `ignore is not available for <harness>, which cannot suppress its own MCP config; using refuse`, and `refuse` is the default. Because the switch works by name, omac still refuses to start when it cannot rely on it: the entry has the name of a registered server (the switch would turn off the injected server too), a plugin or an administrator provides it, its name has characters other than letters, digits, `-` and `_`, or, for Codex, it is not in the user's own `config.toml` (an entry from a project file may belong to a folder Codex does not trust, and Codex rejects the override of an entry it has not loaded). A config file omac cannot read or parse also still refuses, because it may hide entries. The switched-off entry and any token in it stay in the file; rotate the token. omac never edits the harness's own config files.

| Harness | How the registered servers arrive | What `ignore` does, or why it is not available |
|---|---|---|
| Codex | `-c mcp_servers.<name>=...` overrides plus the bridge variables in the environment | turns off each entry found in `config.toml` by `-c mcp_servers.<name>.enabled=false`; `-c` merges into the config and cannot remove servers (`-c mcp_servers={}` changes nothing), so entries omac cannot find still load; `--ignore-user-config` exists only for `codex exec` and skips only `config.toml`, not project or plugin servers |
| Copilot CLI | `--additional-mcp-config @<file>` | turns off each entry found by `--disable-mcp-server <name>`; the switch names servers one by one (`--disable-builtin-mcps` covers only the built-in ones), so entries omac cannot find still load |
| pi | `-e <file>` with an extension that calls `pi.registerMcpServer` | `--no-mcp` turns off all MCP, including the servers omac registers; there is no flag that skips `mcp.json` and keeps extension servers |
| CodeWhale | `CODEWHALE_MCP_CONFIG` names a file with only the registered servers | the variable replaces the global `mcp.json`, but the project file `.codewhale/mcp.json` (after your approval in CodeWhale) and plugin bundles still load |

Registered servers are scanned against the harness's own entries under the same policy, at the places in the [table above](#where-omac-looks). With CodeWhale, the global `mcp.json` is listed and refused although `CODEWHALE_MCP_CONFIG` means CodeWhale does not read it for this run: omac does not rely on that. An entry found there is an unregistered entry, listed with an `omac mcp add` line like those of OpenCode and Claude Code.

Under `warn` or `allow`, an entry in the harness's own config with the same name as a registered server can win over the injected one: pi documents that a server in `mcp.json` takes precedence over an extension's registration, and CodeWhale's approved project file overrides same-named global servers. Codex merges a same-named `config.toml` entry field by field into the `-c` table. Only Copilot CLI documents that `--additional-mcp-config` wins over the user's and project files. The default `refuse` mode stops such a start. If a harness cannot receive MCP servers (none of the six today), `omac mcp add`, `add-json` and `register` reject it, and a start with a registered server follows `mcp.unregistered`: `refuse` stops, `warn` starts without the servers, `allow` starts silently.

### Known gap: config locations omac does not know

`refuse` and the switching off by name both rely on the scan finding the entry. A config location the scan does not know is not refused and not switched off, and the harness loads it. The locations come from each harness's documentation and change with its releases, so they are researched again at regular intervals; the links and the date of the last check are in `internal/mcp/mcpinject/disable.go`. If you find a place a harness reads MCP servers from that [the table](#where-omac-looks) does not list, please open an issue.

### What was checked, and how

All of this is from documentation and source code, read on 2026-11-23. None of it comes from running the tool.

- **Codex** (source: `openai/codex`, `codex-rs/utils/cli/src/config_override.rs`, `config/src/overrides.rs`, `config/src/merge.rs`, `config/src/mcp_types.rs`, `rmcp-client/src/utils.rs`): the value of `-c key=value` is parsed as TOML, so an inline table works; the override layer is deep-merged over `config.toml`, so it adds the server next to the others; `env_vars` is a list of variable names (or `{ name, source }` objects) that Codex copies from its own environment into the server's, besides a small default set.
- **Turning entries off** (Codex: `codex-rs/config/src/mcp_types.rs`; Copilot CLI: `github/docs`, `cli-command-reference.md`): a Codex server has an `enabled` field, and the `-c` layer merges over the loaded config, but an entry that sets only `enabled` and has no `command` or `url` fails with `invalid transport`, which is why omac only switches off entries from the user's own `config.toml`. Copilot CLI documents `--disable-mcp-server` and `--enable-mcp-server` by server name; whether a name from a plugin or an administrator policy can be switched off is not stated, so those entries refuse. Neither was run.
- **Copilot CLI** (documentation: `github/docs`, `cli-command-reference.md`; `github/copilot-cli` changelog): `--additional-mcp-config` takes inline JSON or `@file`, adds to the user's `mcp-config.json` and wins over a same-named server; the entry fields are `type` (`local` or `stdio`), `command`, `args`, `tools`, `env`; the flag may be repeated (changelog 0.0.343). Not stated: whether repeated flags merge by server name or replace each other, which is why omac refuses a flag you pass yourself.
- **pi** (documentation and source: `earendil-works/pi`, `packages/coding-agent/docs/cli.md`, `docs/extensions.md`, `docs/mcp.md`, `src/core/extensions/loader.ts`, `src/extensions/mcp/runtime.ts`): `-e <path>` loads an extension for one run (repeatable); `pi.registerMcpServer(name, config)` takes a config in the shape of an `mcpServers` entry and registers it for the session; there is no flag or environment variable that adds servers, and none that keeps extension servers while skipping `mcp.json`. The extension runs inside pi with its permissions; pi's own text calls `-e` a way for quick tests, which is how omac uses it for one session.
- **CodeWhale** (source: `Hmbown/CodeWhale`, `crates/runtime/src/config.rs`, `crates/engine/src/mcp.rs`, `crates/mcp/src/config.rs`): `CODEWHALE_MCP_CONFIG` overrides `mcp_config_path` from `config.toml` and replaces the global file; the key is `servers` or its alias `mcpServers`; entries take `command`, `args`, `env`; the legacy `~/.deepseek/mcp.json` is read only when no path is set; the project file and plugin bundles are merged in afterwards.
- **OpenCode 2** (source: `sst/opencode`, branch `dev`, `packages/cli/src/server-process.ts`, `packages/core/src/config.ts`): the v2 server process reads `OPENCODE_CONFIG_CONTENT` and accepts the v1-shaped `mcp.<name>` entry omac writes. OpenCode 1 and 2 share one harness descriptor, injection and scan.

Not covered:

- Command-line config overrides other than the ones the table names, such as Copilot CLI's `--plugin-dir` and the disable switches.
- Cloud, account and organization sources: Codex cloud-managed config, Copilot's built-in and policy-derived servers, CodeWhale plugin bundles.
- Servers an extension or plugin registers in code (pi extensions you load yourself) or that sit in bundles omac cannot read from the host.
- Unverified, because only the documentation and source were read and no harness was run: that each harness behaves at run time as they describe; whether Copilot CLI merges repeated `--additional-mcp-config` flags by name; whether a CodeWhale managed config can override `mcp_config_path`; the full environment CodeWhale gives a stdio server; whether Codex profile files can hold `mcp_servers`; whether a pi older than 0.99.0 reports a missing `registerMcpServer` clearly.

## Diagnosing problems

```bash
omac mcp diagnose [--scope project|global] [--all] [--probe]
```

For each registered server, `diagnose` reports:

- a registration that fails to load (its snapshot does not match its hash),
- secrets missing from the keychain (names only),
- a command that is not on `PATH`, or that lies inside the workdir,
- tools that have no class,
- tools not reviewed yet or changed since the last review (changes only with `--probe`),
- harness MCP entries omac does not manage, with the `omac mcp add` line that would register them.

With `--probe` (host terminal only) it also starts each server and reports tools that changed since the last review, or why the server did not start, with the tail of its stderr. Exit code 0 means nothing was found, 1 means a problem was found, 2 means wrong usage.

## Troubleshooting

- **Start refused: unregistered MCP entry.** The message names the entry and the file and prints an `omac mcp add` line for it. Register the server, remove the entry, or run once with `--mcp-unregistered=allow` (or `warn`). A remote entry cannot be registered yet.
- **Start refused: config file cannot be parsed.** omac cannot check a config file it cannot read (for example JSONC or `~/.claude.json`), so in `refuse` mode it stops. Fix the file, or run once with `--mcp-unregistered=warn`, `ignore` (Claude Code) or `allow`.
- **A server is missing in the agent, with a warning at start.** A secret is not in the keychain. Register it again with `omac mcp add --replace`, which asks for the secret.
- **The agent sees no tools, or every call fails with `mcp-unreviewed` or `mcp-changed`.** Run `omac mcp diagnose --probe`, then `omac mcp tools <name> --classify` or `omac mcp review <name>` on the host.
- **I changed the definition and nothing happened.** Register it again, or use `omac mcp add --replace` for the command and environment.
- **A server I just registered is not in the running session.** Restart the session; the harness reads its MCP config once at launch.

## What the agent can do

The agent can call the tools that are classified `read`, `write` or `delete`, are allowed by the server's `access` mode, and still match the last review. Everything else is refused. In detail:

- **Closed world.** A tool that has no class is not listed and not callable. A tool classified `off` is not listed and not callable either.
- **Methods.** `initialize`, `ping`, `tools/list`, `tools/call`, notifications and the agent's answers to requests the server makes (for example `sampling/createMessage` or `elicitation/create`) pass. `resources/*`, `prompts/*` and every other method are refused.
- **Strict parsing.** The proxy parses every `tools/call`. It refuses batches, duplicate or case-variant keys (`Name` for `name`), encoded bodies and anything it cannot parse. It forwards exactly the bytes it checked, so the server cannot read the message differently from the proxy.
- **Checked twice.** The gateway, which runs separately, applies the same policy again on its own, so one bug in one place does not open a tool.
- **Approvals.** Classes whose `approvals` mode is `required` (by default `write` and `delete`) raise a native dialog before the call. It shows the server, the tool, the class and a summary of the arguments, and offers **Allow once**, **Allow for this session**, **Allow always (this tool of this server)**, **Deny for this session (this tool of this server)** and **Deny**. **Deny** refuses only this call, so an agent that retries sees the dialog again. No answer within 60 seconds is a deny.
  - The summary shows each top-level argument with at most 40 characters of its value, nested objects and arrays reduced to their size, and the value of an argument whose name contains `token`, `secret`, `password`, `key`, `auth`, `bearer` or `credential` masked. Treat it as a hint, not as the complete call.
  - "Always" covers only that tool, in its current class, in that project. If you reclassify the tool, it asks again. Calls of different tools never share a dialog, so a denial for one never answers another.
  - "Deny for this session" covers only that tool, in its current class, on that server in that project. Until omac stops, the agent gets `approval-denied` for that tool without a dialog. It is not stored, so `omac approvals list` does not show it and a restart asks again.
  - Revoke with `omac approvals list` and `omac approvals revoke <id>`. The rows appear as `mcp:<server>` and `tool <name> (<class>)`. `omac mcp deregister <name>` also removes every stored approval of that server (for a project registration those of this project, for `--scope global` those of all projects), so a different server registered later under the same name does not inherit them; it prints how many it removed, and a failure to remove them is only a warning. `omac mcp add --replace` keeps them, because it is the same server with a changed command.
- **What the agent sees on a refusal.** A JSON-RPC error that names the reason: `mcp-unlisted` (no class, `off`, or a method that is not allowed), `mcp-access` (the class is above the server's `access`), `mcp-unreviewed` (the tools were never reviewed), `mcp-changed` (the tools changed since the last review), `mcp-unparseable`, `approval-denied`, `approval-timeout`, `approval-unavailable` or `approval-abandoned`.

omac decides per tool, as it does for skill sidecars. A rule on the arguments, such as "post to channel A but not channel B", belongs in the server itself, in the same way a skill sidecar checks it in its own code. omac does not add a second policy language on top. Use the approval dialog, which shows the arguments, for anything you want to see before it happens.

### Changed tools

The gateway compares the live tool list (names, descriptions and input schemas) with the reviewed list for each session, and again whenever the server announces that its tools changed. With no review, or with any difference, **every `tools/call` is blocked** with an error that names the added, removed and changed tools and tells the agent to ask you to run `omac mcp review <name>` on the host. A tool that changed is never listed, even before you notice. You then run `omac mcp review`, which shows the change, and accept the current tools if you trust them.

Tool descriptions are untrusted text. A malicious or compromised server can put instructions for the agent in them. A review does not make a description trustworthy; it only makes sure the description is the one you reviewed.

### Audit

The audit trail records every tool call. `facade.request` and `facade.approval` events carry `tool`, `tool_class` and `tool_args`: the same masked argument summary as the dialog, capped at 1 KiB, with every secret value of 8 or more characters that omac knows redacted. The `source` of a decision is `mcp-unparseable`, `mcp-unlisted`, `mcp-access`, `declared` (no approval needed), `retry` (see below), or one of the approver's own (`prompt`, `session`, `always`, `session-deny`, `timeout`, `unavailable`, `abandoned`).

When the gateway restarts, the bridge retries the call that failed with the same request id, also on a new session. Within 15 seconds omac reuses the approval you already gave for that same call (same request id, tool and arguments), so you are not asked again, and the audit log records it as source `retry`. The approval admits one attempt at a time: while an attempt is in flight, an identical copy of the call asks again, and once the call has gone through, the approval is used up, so a deliberate repeat asks again. A changed argument, tool or request id also asks. Only allowances are reused; a deny is never reused this way.

## Security principles and what to consider

The gateway moves the credential out of the agent's reach and limits the agent to the tools you reviewed. It does not review what a server does with the credential. A registered server runs outside the sandbox with your authority.

### What stays on the host

- The real credentials, in the OS keychain and in the server process's environment.
- The definition, its classes and the review record, in `~/.config/omac`, which the sandbox cannot access.
- Your approval answers.
- The server process itself.

### What the agent can still see

- The bridge command and the entry in the harness config.
- A token that lets it talk to omac's proxy (the facade token). It is valid for this session only. The proxy enforces your classes and approvals for every request, also one the agent sends straight to it instead of through the harness.
- Every tool result, which can contain data you would not paste into a chat.
- The tool names and descriptions of the tools that pass, which are untrusted text.

### Server review checklist

Before you register a server, and before you review a changed tool list:

- **Fix the version or digest.** `npx -y pkg@1.2.3`, not `pkg@latest` or no version. Review updates before you review the new tools.
- **Read what its tools can do.** Pay most attention to tools that run commands, fetch URLs or read files. With the credential, a fetch tool can send data anywhere. Classify a tool you do not need as `off`.
- **Use the narrowest credential scope** that works, for example a read-only token for a server that only needs to read. A class is only as good as your reading of the tool: a tool you classify as `read` can still do what its code does.
- **Know the runtime.** The server runs on your machine with your user's rights, with the snapshot directory as working directory and a cleaned environment. The sandbox does not confine it.

### Limits

- **Stdio servers only.** Remote servers and servers that need OAuth are not supported yet.
- **`omac serve`** does not run or inject MCP servers.
- **Argument-level rules live in the server.** omac decides per tool, as it does for skill sidecars.
- **OAuth tokens in a harness's own state directory** (for example from a login inside the harness) are not protected by omac, and the scan of harness config files does not see them.
- **Servers provided by harness plugins that omac cannot read from the host**, **claude.ai connectors**, remote organization config and servers an OpenCode plugin registers in code are not seen by the scan. See [Where omac looks](#where-omac-looks).
- **Credentials already in readable harness config files** may already be exposed to agents that ran earlier. Rotate them.
- **Third-party server code and its dependencies** run with your credential. A compromised package is a compromised credential.

See also [Security](../security.md#mcp-servers), [CLI](./cli.md#omac-mcp) and [Configuration](../configuration.md#launcher-config).
