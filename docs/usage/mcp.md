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
| An MCP server with no secret, or you accept that the agent can read its token | A plain entry in the harness's own MCP config; see [Running an MCP server the harness launches](../configuration.md#running-an-mcp-server-the-harness-launches) |

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

Once at least one server is registered, omac looks at the MCP entries in the harness's own config files (for example `.mcp.json`, `opencode.json`, `~/.claude.json`) and applies `mcp.unregistered`:

| Value | Effect |
|---|---|
| `refuse` (default once a server is registered) | `omac start` stops and names each entry and file. |
| `warn` | Starts, lists the entries as a warning. They stay active. |
| `allow` | Starts without comment. The entries stay active. |
| `ignore` | Starts and disables the entries. Only registered servers are available. |

`mcp.literal_credentials` controls what happens when a harness config holds a credential written directly into a header, a server's `env` or a URL query string (`?token=...`):

| Value | Effect |
|---|---|
| `refuse` (default, same switch-on rule) | `omac start` stops and names the entry. |
| `warn` | Starts with a warning. |
| `allow` | Starts without comment. |

Both settings go in the user-global launcher config, `~/.config/omac/config.yaml`:

```yaml
mcp:
  unregistered: refuse          # allow | warn | ignore | refuse
  literal_credentials: refuse   # allow | warn | refuse
```

- **Project-local files cannot set this.** A project's `.opencode/oh-my-agentic-coder.yaml` is ignored for `mcp:`, so opening a repository cannot switch the checks off.
- **Where it is read from.** `~/.config/omac/config.yaml`, or `$XDG_CONFIG_HOME/omac/config.yaml` when that variable is set. The registry uses the same directory.
- **One run:** `omac start --mcp-unregistered=ignore` overrides `mcp.unregistered` for that run. This is the way past a refusal caused by a checked-in `.mcp.json`. There is no flag for `literal_credentials`.
- `mcp.unregistered` and `mcp.literal_credentials` apply only once a server is registered or you set the value explicitly. With nothing registered and nothing set, omac does not look at your MCP config.

## Troubleshooting

- **Start refused: unregistered MCP entry.** The message names the entry and the file. Register the server with `omac mcp register`, remove the entry, or run once with `--mcp-unregistered=ignore` (or `warn`).
- **Start refused: literal credential.** A harness config file contains a token. Move it out of that file, register the server so omac holds the credential, and rotate the token: it has been readable by the agent. See [Residual risks](#residual-risks).
- **Start refused: config file cannot be parsed.** omac cannot check a config file it cannot read (for example JSONC or `~/.claude.json`), so in `refuse` mode it stops. Fix the file, or set `mcp.unregistered` to `warn`, `allow` or `ignore`.
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

- **OAuth tokens in a harness's own state directory** (for example from a login inside the harness) are not protected by omac, and the credential scan cannot see them.
- **Credentials already in readable harness config files** may already be exposed to agents that ran earlier. Rotate them. This is why `literal_credentials` refuses by default.
- **Third-party server code and its dependencies** run with your credential. A compromised package is a compromised credential.

### Not covered yet

- Per-tool approvals and audit that records tool names.
- Pinning the server's tool list, so a change is caught.
- Remote HTTP servers and OAuth.
- `omac serve`: it prints a notice and injects nothing.
- Live reload: registrations are read at start, and omac does not restart a gateway that exits.

See also [Security](../security.md#mcp-servers) and [Configuration](../configuration.md#launcher-config).
