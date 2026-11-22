---
title: MCP servers
description: Running MCP servers through the omac gateway so the agent never holds their credentials.
---

An MCP server gives the agent typed tools for an external service, such as Jira or GitHub. The omac MCP gateway runs such a server on your machine, outside the sandbox, with its credentials. The agent gets the tools and never gets the credential.

## When to use it

| You want | Use |
|---|---|
| The agent to call a service with a secret, and the secret must stay out of the sandbox, and a stdio MCP server for it exists | The MCP gateway (this page) |
| The same, and you write the integration yourself or one exists as a skill | A [skill](./skills.md) |
| An MCP server with no secret, or you accept that the agent can read its token | A plain entry in the harness's own MCP config. omac refuses to start with such an entry once you register a server, unless you allow it; see [Existing MCP entries](#existing-mcp-entries-in-the-harness-config) and [Running an MCP server the harness launches](../configuration.md#running-an-mcp-server-the-harness-launches) |

If you register no MCP server, nothing changes. omac does not touch the harness's MCP configuration.

## How it works

```
 host (outside the sandbox)                      sandbox
 ┌───────────────────────────────┐              ┌────────────────────────┐
 │ MCP server process            │              │ harness (OpenCode,     │
 │   env: your secrets           │              │   Claude Code)         │
 │        ▲ stdin/stdout         │              │        │ starts        │
 │ omac gateway                  │◄─ omac proxy ┤ "omac mcp bridge <name>"│
 └───────────────────────────────┘   (facade)   └────────────────────────┘
```

- You register the server once. Its definition and secrets are stored on the host, where the sandbox cannot read them.
- At `omac start`, omac runs the server on the host and gives the harness a small bridge command instead of the real server.
- The bridge only passes messages through omac's proxy. The agent sees the tools, never the credential.

## Writing a definition

A definition is a YAML file, by convention `mcp.yaml`. This one runs the reference "everything" server with one secret:

```yaml
name: demo
server:
  command: [npx, -y, "@modelcontextprotocol/server-everything@2025.1.14"]
  env:
    DEMO_REGION: eu-west-1
secrets: [DEMO_TOKEN]
mode:
  name: DEMO_MODE
  value: read-only
```

| Field | Meaning |
|---|---|
| `name` | Name of the server. The harness shows it, and you use it in `omac mcp show <name>`. |
| `server.command` | The command that starts a stdio MCP server. Use an absolute path or a command found on `PATH` (such as `npx` or `uvx`). Relative paths and absolute paths inside your project are refused. |
| `server.env` | Fixed, non-secret environment variables for the server. |
| `secrets` | Names of secrets the server needs. omac asks for them at registration, stores them in the OS keychain and sets them as environment variables of the server process only. |
| `mode` | Optional. One environment variable (`name`, `value`) that some servers use to switch between, for example, read-only and read-write. |

Only stdio servers are supported.

## Registering and managing servers

Run these in a normal terminal on your machine, not inside the agent's sandbox. `register` and `deregister` refuse to run when they detect they are inside an omac session.

```bash
omac mcp register ./mcp.yaml --harness opencode,claude-code             # this project only (default)
omac mcp register ./mcp.yaml --scope global --harness claude-code       # every project
omac mcp list
omac mcp show demo
omac mcp deregister demo
```

| Command | What it does |
|---|---|
| `omac mcp register <file>` | Reads the definition, prompts for each secret (input hidden), stores the secrets in the keychain and saves the definition. |
| `omac mcp list` | Shows registered servers, their scope and harnesses. `--all` includes registrations for project directories that no longer exist. |
| `omac mcp show <name>` | Shows one definition, its scope and hash. |
| `omac mcp deregister <name>` | Removes the registration and deletes its secrets from the keychain. |

- **`--scope`**: `project` (default) applies to the project in the current directory. `global` applies to every project. If both exist under one name, the project entry wins and omac prints a notice. A project registration is tied to the directory's path, so moving or renaming the directory leaves it behind (visible with `omac mcp list --all`), and each git worktree is its own scope.
- **`--harness`**: required. A comma-separated list (`opencode`, `claude-code`). A server is only injected into the harnesses you list.
- **Secrets in scripts**: instead of the prompt, export `OMAC_SECRET_<NAME>` (for example `OMAC_SECRET_DEMO_TOKEN`) before `register`.

### Where things are stored

- Definitions and snapshots: `~/.config/omac/mcp/<scope>/<name>/` (under `$XDG_CONFIG_HOME/omac` if you set it). The sandbox cannot read or write this directory.
- Secrets: the OS keychain, under the service name `omac-mcp/<scope>/<name>`. `omac mcp deregister` deletes them.
- omac never reads a definition from your project. Only `omac mcp register` creates one.

### Hash and snapshot

At registration omac copies the definition into a snapshot and records its sha256 hash. At every start, omac checks the snapshot against the hash and runs the server from the snapshot, not from your file. If you edit `mcp.yaml` afterwards, nothing changes until you register it again, which shows you what you are approving. The server runs with the snapshot directory as its working directory (never your project, because tools such as `npx` and `uvx` read config files like `.npmrc` from the working directory, and the agent can write those) and with a cleaned environment: omac's own variables are removed, and only the `server.env`, `secrets` and `mode` entries are added.

## What omac does at launch

For each registered server that matches the harness, omac starts the server on the host before the harness and injects an entry that starts the bridge. It never edits your own config files.

- **OpenCode**: omac passes the entry in the `OPENCODE_CONFIG_CONTENT` environment variable, which takes precedence over project config.
- **Claude Code**: omac writes a config file to the session's temporary directory and passes it with `--mcp-config`.

omac adds these variables to the sandbox only when it injects, so a value you export yourself does not reach the agent.

If a secret is missing from the keychain, omac prints a warning and skips that server. The rest of the session starts normally.

## Existing MCP entries in the harness config

Outside omac, use your harness's own MCP config as usual. Inside omac, MCP configuration comes only from the omac registry; omac translates it into the harness's configuration at launch.

An MCP entry in the harness's own config (for example `.mcp.json`, `opencode.json`, `~/.claude.json`) either runs inside the sandbox or keeps its login in the harness's state directory. In both cases the agent can reach its credentials. omac therefore refuses to start while such an entry exists. This includes an entry with the same name as a registered server: the old entry, and any token in it, stays in a file the agent can read, so registering the server does not make it safe. omac never removes, disables or rewrites these entries. It only adds your registered servers next to them.

One harness config cannot serve a server with credentials both inside and outside omac. To use a server in both places, keep the harness entry out of the files omac scans and register the server with omac, or run omac once with `--mcp-unregistered=allow`. Under `allow` or `warn`, if a harness entry has the same name as a registered server, the harness uses the entry omac injects.

Fix a refusal in one of three ways:

- **Register the server** with `omac mcp register`, so its credentials stay on the host.
- **Remove the entry** from the harness config.
- **Allow it for one run** with `omac start --mcp-unregistered=allow`.

If an entry already holds a token, rotate it: agents that ran earlier may have read it.

While only stdio servers are supported, a remote or OAuth server cannot be used through the harness with the default `refuse`. It needs `--mcp-unregistered=allow` for that run.

`mcp.unregistered` sets the default:

| Value | Effect |
|---|---|
| `refuse` (default once a server is registered) | `omac start` stops and names each entry and its file. A config file omac cannot parse also stops it, because it may hide entries. |
| `warn` | Starts and prints a warning for each entry and each unparseable file. |
| `allow` | Starts without comment. |

It goes in the user-global launcher config, `~/.config/omac/config.yaml`:

```yaml
mcp:
  unregistered: refuse          # allow | warn | refuse
```

- **Project-local files cannot set this.** A project's `.opencode/oh-my-agentic-coder.yaml` is ignored for `mcp:`, so opening a repository cannot switch the check off.
- **Where it is read from.** `~/.config/omac/config.yaml`, or `$XDG_CONFIG_HOME/omac/config.yaml` when that variable is set. The registry uses the same directory.
- **One run:** `omac start --mcp-unregistered=<allow|warn|refuse>` overrides the setting for that run. This is the way past a refusal caused by a checked-in `.mcp.json`.
- `mcp.unregistered` applies only once a server is registered or you set the value or flag explicitly. With nothing registered and nothing set, omac does not look at your MCP config.

## Troubleshooting

- **Start refused: unregistered MCP entry.** The message names the entry and the file. Register the server with `omac mcp register`, remove the entry, or run once with `--mcp-unregistered=allow` (or `warn`).
- **Start refused: config file cannot be parsed.** omac cannot check a config file it cannot read (for example JSONC or `~/.claude.json`), so in `refuse` mode it stops. Fix the file, or run once with `--mcp-unregistered=warn` or `allow`.
- **A server is missing in the agent, with a warning at start.** A secret is not in the keychain. Run `omac mcp deregister <name>` and register it again.
- **I changed the definition and nothing happened.** Register it again.

## Security principles and what to consider

The gateway moves the credential out of the agent's reach. It does not review what a server does with it, and it does not limit which of its tools the agent may call.

### What stays on the host

- The real credentials, in the OS keychain and in the server process's environment.
- The definition and its snapshot, in `~/.config/omac`, which the sandbox cannot access.
- The server process itself.

### What the agent can still see

- The bridge command and the entry in the harness config.
- A token that lets it talk to omac's proxy (the facade token). It is valid for this session only.
- Every tool result, which can contain data you would not paste into a chat.
- The server's tool names and descriptions. Treat these as untrusted text: a malicious or compromised server can put instructions in them.

### The agent can call every tool of a registered server

omac M1 has no per-tool approvals and no audit with tool names. The agent holds the facade token, so it can call any tool of a registered server, including by sending requests straight to the proxy instead of through the harness. A harness permission rule such as "ask before this tool" is not a security control against this.

So:

- Only register servers and credentials you would let the agent use fully.
- Use the narrowest-scope token the server can work with, for example a read-only token for a server that only needs to read.

### Server review checklist

Before you register a server:

- **Pin the version or digest.** `npx -y pkg@1.2.3`, not `pkg@latest` or no version. Review updates before you move the pin.
- **Read what its tools can do.** Pay most attention to tools that run commands, fetch URLs or read files. With the credential, a fetch tool can send data anywhere.
- **Use the narrowest credential scope** that works.
- **Know the runtime.** The server runs on your machine with your user's rights, with the snapshot directory as working directory and a cleaned environment. The sandbox does not confine it.

### Residual risks

- **OAuth tokens in a harness's own state directory** (for example from a login inside the harness) are not protected by omac, and the scan of harness config files does not see them.
- **Credentials already in readable harness config files** may already be exposed to agents that ran earlier. Rotate them.
- **Third-party server code and its dependencies** run with your credential. A compromised package is a compromised credential.

### Not covered yet

- Per-tool approvals and audit that records tool names.
- Pinning the server's tool list, so a change is caught.
- Remote HTTP servers and OAuth.
- `omac serve`: it prints a notice and injects nothing.
- Live reload: registrations are read at start, and omac does not restart a gateway that exits.

See also [Security](../security.md#mcp-servers) and [Configuration](../configuration.md#launcher-config).
