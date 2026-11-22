---
title: MCP servers
description: Running MCP servers through the omac gateway so the agent never holds their credentials.
---

An MCP server gives the agent typed tools for an external service, such as Jira or GitHub. The omac MCP gateway runs such a server on your machine, outside the sandbox, with its credentials. The agent gets the tools you have reviewed and never gets the credential.

MCP in omac is for reaching something outside the sandbox with credentials, OAuth tokens included. Outside omac, use your harness's own MCP config as usual. Inside omac, MCP configuration comes only from the omac registry, which omac translates into the harness's configuration at launch.

## When to use it

| You want | Use |
|---|---|
| The agent to call a service with a secret, and the secret must stay out of the sandbox, and a stdio MCP server for it exists | The MCP gateway (this page) |
| The same, and you write the integration yourself or one exists as a skill | A [skill](./skills.md) |
| An MCP server with no secret, or you accept that the agent can read its token, or a remote or OAuth server | Not supported by the gateway yet. omac refuses to start with a harness MCP entry by default, so run once with `--mcp-unregistered=allow`; the server then runs inside the sandbox. See [Existing MCP entries](#existing-mcp-entries-in-the-harness-config) and [Running an MCP server the harness launches](../configuration.md#running-an-mcp-server-the-harness-launches) |

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

- You register the server once and review its tools once. The definition, the classes, the pin and the secrets are stored on the host, where the sandbox cannot read them.
- At `omac start`, omac runs the server on the host and gives the harness a small bridge command instead of the real server.
- The bridge only passes messages through omac's proxy. The proxy checks every call against your classes before it reaches the gateway, and the gateway checks again. The agent sees the tools, never the credential.

## Registering a server

Run these in a normal terminal on your machine, not inside the agent's sandbox. All registration commands refuse to run when they detect they are inside an omac session. Registering only stores the server. **No tool is callable until you have reviewed and pinned its tools** (see [Reviewing and pinning tools](#reviewing-and-pinning-tools)).

There are three ways to register, with the same result.

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

`register` stores the file exactly as written. Classes, approvals and a pin that are not in the file are gone, so to change only the command or environment of a registered server use `omac mcp add --replace` instead. The [definition reference](#definition-reference) lists the fields.

Options shared by all three:

- **`--scope`**: `project` (default) applies to the project in the current directory. `global` applies to every project. If both exist under one name, the project entry wins and omac prints a notice. A project registration is tied to the directory's path, so moving or renaming the directory leaves it behind (visible with `omac mcp list --all`), and each git worktree is its own scope. Run the command in the project directory you mean.
- **`--harness`**: required for a new server. A comma-separated list (`opencode`, `claude-code`). A server is only injected into the harnesses you list.
- **`--replace`** (`add` and `add-json`): changes the command, environment and secrets of a registered server and keeps its tools, approvals, access and pin. The harness list cannot change this way; deregister first.

On success `add` prints that no tool is callable yet and the next command: `omac mcp tools NAME --classify`.

```bash
omac mcp list                  # servers, scope, harnesses, tool count, classified count, pinned or unpinned
omac mcp show demo             # one server in full
omac mcp deregister demo       # removes the registration and its secrets from the keychain
```

`omac mcp show` prints the command, the names of the `env` keys, the secrets with `stored` or `missing`, the access mode, the approvals, the pin (first 12 hex digits of its hash, date and tool count) and every tool with its class. It never prints a secret or an environment value.

### Where things are stored

- Definitions and snapshots: `~/.config/omac/mcp/<scope>/<name>/` (under `$XDG_CONFIG_HOME/omac` if you set it). The sandbox cannot read or write this directory.
- Secrets: the OS keychain, under the service name `omac-mcp/<scope>/<name>`. `omac mcp deregister` deletes them.
- omac never reads a definition from your project. Only the registration commands create one.

### Hash and snapshot

At registration omac writes the canonical form of the definition into a snapshot and records the sha256 hash of that canonical form (not of your file's bytes, so comments and key order in your file do not matter). At every start, omac checks the snapshot against the hash and runs the server from the snapshot, not from your file. If you edit `mcp.yaml` afterwards, nothing changes until you register it again. The server runs with the snapshot directory as its working directory (never your project, because tools such as `npx` and `uvx` read config files like `.npmrc` from the working directory, and the agent can write those) and with a cleaned environment: omac's own variables are removed, and only the `server.env`, `secrets`, `mode` and `env_passthrough` entries are added, plus a small base set (`PATH`, `HOME`, `USER`, `LANG`, `LC_ALL`, `LC_CTYPE`, `TMPDIR`).

## Reviewing and pinning tools

A server announces its tools by itself, and it can change them at any time. So omac does not trust the announcement: you review the tools once, give each a class, and omac pins what you saw.

```bash
omac mcp tools demo                    # list the tools, change nothing
omac mcp tools demo --classify         # review, classify and pin in one step
omac mcp pin demo                      # after a drift: review the change and pin again
```

`omac mcp tools` starts the server on the host with its secrets and lists its tools: a proposed class and the reason, the current class and the pin status (`pinned`, `changed`, `new`, `removed`). The proposal comes from the annotations the server attaches to its tools. They are claims, not facts, so read each tool yourself.

| Class | Meaning |
|---|---|
| `read` | Only reads. |
| `write` | Changes something. |
| `delete` | Deletes or is destructive. This is also the proposal when no annotation says the tool is safe. |
| `off` | Pinned, but hidden from the agent and refused. |

`--classify` asks for a class per tool (Enter takes the proposal) and then writes the classes and the pin in one step. To script it, give `--set NAME=CLASS` (repeatable) and `--rest CLASS` for all other tools. `--rest` defaults to `off`, so a script that names only the tools it wants cannot enable the rest by accident.

`omac mcp pin` starts the server, shows which tools were added, removed or changed since the pin, refuses while a live tool has no class, and asks `[y/N]` (`--yes` skips the question). Both commands run only on the host. After either, and after any registration change, a running `omac start` session restarts that server's gateway by itself.

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
| `server.command` | The command that starts a stdio MCP server. Use an absolute path or a command found on `PATH` (such as `npx` or `uvx`). Relative paths and absolute paths inside your project are refused. |
| `server.env` | Fixed, non-secret environment variables for the server. |
| `server.env_passthrough` | Names of host environment variables to hand to the server as they are, for example a proxy setting. |
| `secrets` | Names of secrets the server needs. omac asks for them at registration, stores them in the OS keychain and sets them as environment variables of the server process only. |
| `mode` | Optional. One environment variable (`name`, `value`) that some servers use to switch between, for example, read-only and read-write. |
| `access` | `read-only`, `read-write` or `read-write-delete` (default). Tools of a class above the access mode are hidden and refused. |
| `tools` | The classes: a list of `name` and `class` (`read`, `write`, `delete`, `off`). A tool that is not listed is refused. |
| `approvals` | Whether a call asks you first, per class: `none` or `required`. Defaults: `read: none`, `write: required`, `delete: required`. |
| `pin` | `tools_hash`, `tools` (a hash per tool) and `pinned_at`. Written by omac. Do not edit it by hand: it is rewritten by `omac mcp tools --classify` and `omac mcp pin`. |

Only stdio servers are supported.

## What omac does at launch

For each registered server that matches the harness, omac starts the server on the host before the harness and injects an entry that starts the bridge. It never edits your own config files.

- **OpenCode**: omac passes the entry in the `OPENCODE_CONFIG_CONTENT` environment variable, which takes precedence over project config.
- **Claude Code**: omac writes a config file to the session's temporary directory and passes it with `--mcp-config`.

omac adds these variables to the sandbox only when it injects, so a value you export yourself does not reach the agent.

If a secret is missing from the keychain, omac prints a warning and skips that server. The rest of the session starts normally. `omac serve` does not run or inject MCP servers; it prints a notice and you use `omac start` for them.

### While a session runs

- **Changed registration.** After `omac mcp pin`, `omac mcp tools --classify` or a changed registration, the running session restarts that server's gateway and picks up the new classes and pin.
- **A new server needs a restart.** The harness reads its MCP configuration once at launch, so a server you register during a session is only reported (a notice says to restart). A server you deregister keeps running until the session ends.
- **A gateway that dies** is restarted with a growing delay and a fresh secret, and the bridge reconnects. A request in flight when it died fails with an error. After 5 restarts within 10 minutes omac gives up: calls to that server get HTTP 502 (`skill-broken`) until you restart the session or a changed registration refreshes it.
- **Cleanup.** On Linux, a gateway that is killed with SIGKILL takes its server process with it. On macOS there is no equivalent, so the server process relies on exiting when its stdin closes.

## Existing MCP entries in the harness config

Outside omac, use your harness's own MCP config as usual. Inside omac, MCP configuration comes only from the omac registry; omac translates it into the harness's configuration at launch.

An MCP entry in the harness's own config (for example `.mcp.json`, `opencode.json`, `~/.claude.json`) either runs inside the sandbox or keeps its login in the harness's state directory. In both cases the agent can reach its credentials. omac therefore refuses to start while such an entry exists. This includes an entry with the same name as a registered server: the old entry, and any token in it, stays in a file the agent can read, so registering the server does not make it safe. omac never removes, disables or rewrites these entries. It only adds your registered servers next to them.

One harness config cannot serve a server with credentials both inside and outside omac. To use a server in both places, keep the harness entry out of the files omac scans and register the server with omac, or run omac once with `--mcp-unregistered=allow`. Under `allow` or `warn`, if a harness entry has the same name as a registered server, the harness uses the entry omac injects.

The refusal and the warning print, under each stdio entry, an equivalent command, for example:

```text
omac mcp add --scope project --harness claude-code --secret JIRA_TOKEN jira -- npx -y mcp-jira@1.2.3
```

Every `env` key of the entry becomes a `--secret`; no `env` or header value is ever printed. The command arguments are copied as they are, so check them for a token before you run or share the line. A remote entry gets "remote MCP servers are not supported yet", and an entry that cannot be expressed this way points to a definition file and `omac mcp register`.

Fix a refusal in one of three ways:

- **Register the server** with `omac mcp add` (or `register`), so its credentials stay on the host.
- **Remove the entry** from the harness config.
- **Allow it for one run** with `omac start --mcp-unregistered=allow`.

If an entry already holds a token, rotate it: agents that ran earlier may have read it.

Only stdio servers are supported, so a remote or OAuth server cannot be registered. With the default `refuse` it needs `--mcp-unregistered=allow` for that run, and it then runs inside the sandbox.

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

## Diagnosing problems

```bash
omac mcp diagnose [--scope project|global] [--all] [--probe]
```

For each registered server, `diagnose` reports:

- a registration that fails to load (its snapshot does not match its hash),
- secrets missing from the keychain (names only),
- a command that is not on `PATH`, or that lies inside the workdir,
- tools that have no class,
- no pin yet,
- harness MCP entries omac does not manage, with the `omac mcp add` line that would register them.

With `--probe` (host terminal only) it also starts each server and reports drift from the pin, or why the server did not start, with the tail of its stderr. Exit code 0 means nothing was found, 1 means a problem was found, 2 means wrong usage.

## Troubleshooting

- **Start refused: unregistered MCP entry.** The message names the entry and the file and prints an `omac mcp add` line for it. Register the server, remove the entry, or run once with `--mcp-unregistered=allow` (or `warn`).
- **Start refused: config file cannot be parsed.** omac cannot check a config file it cannot read (for example JSONC or `~/.claude.json`), so in `refuse` mode it stops. Fix the file, or run once with `--mcp-unregistered=warn` or `allow`.
- **A server is missing in the agent, with a warning at start.** A secret is not in the keychain. Register it again with `omac mcp add --replace`, which asks for the secret.
- **The agent sees no tools, or every call fails with `mcp-unpinned` or `mcp-drift`.** Run `omac mcp diagnose --probe`, then `omac mcp tools <name> --classify` or `omac mcp pin <name>` on the host.
- **I changed the definition and nothing happened.** Register it again, or use `omac mcp add --replace` for the command and environment.
- **A server I just registered is not in the running session.** Restart the session; the harness reads its MCP config once at launch.

## What the agent can do

The agent can call the tools that are classified `read`, `write` or `delete`, are allowed by the server's `access` mode, and still match the pin. Everything else is refused. In detail:

- **Closed world.** A tool that has no class is not listed and not callable. A tool classified `off` is not listed and not callable either.
- **Methods.** `initialize`, `ping`, `tools/list`, `tools/call` and notifications pass. `resources/*`, `prompts/*` and every other method are refused.
- **Strict parsing.** The proxy parses every `tools/call`. It refuses batches, duplicate or case-variant keys (`Name` for `name`), encoded bodies and anything it cannot parse. It forwards exactly the bytes it checked, so the server cannot read the message differently from the proxy.
- **Checked twice.** The gateway, which runs separately, applies the same policy again on its own, so one bug in one place does not open a tool.
- **Approvals.** Classes whose `approvals` mode is `required` (by default `write` and `delete`) raise a native dialog before the call. It shows the server, the tool, the class and a summary of the arguments, and offers **Allow once**, **Allow for this session**, **Allow always (this tool of this server)** and **Deny**. No answer within 60 seconds is a deny.
  - The summary shows each top-level argument with at most 40 characters of its value, nested objects and arrays reduced to their size, and the value of an argument whose name contains `token`, `secret`, `password`, `key`, `auth`, `bearer` or `credential` masked. Treat it as a hint, not as the complete call.
  - "Always" covers only that tool, in its current class, in that project. If you reclassify the tool, it asks again. Calls of different tools never share a dialog, so a denial for one never answers another.
  - Revoke with `omac approvals list` and `omac approvals revoke <id>`. The rows appear as `mcp:<server>` and `tool <name> (<class>)`.
- **What the agent sees on a refusal.** A JSON-RPC error that names the reason: `mcp-unlisted` (no class, `off`, or a method that is not allowed), `mcp-access` (the class is above the server's `access`), `mcp-unpinned`, `mcp-drift`, `mcp-unparseable`, `approval-denied`, `approval-timeout`, `approval-unavailable` or `approval-abandoned`.

omac decides per tool, as it does for skill sidecars. A rule on the arguments, such as "post to channel A but not channel B", belongs in the server itself, in the same way a skill sidecar checks it in its own code. omac does not add a second policy language on top. Use the approval dialog, which shows the arguments, for anything you want to see before it happens.

### Pinning and drift

The gateway compares the live tool list (names, descriptions and input schemas) with the pin for each session, and again whenever the server announces that its tools changed. With no pin, or with any difference, **every `tools/call` is blocked** with an error that names the added, removed and changed tools and tells the agent to ask you to run `omac mcp pin <name>` on the host. A tool that changed is never listed, even before you notice. You then review the change with `omac mcp pin`, which shows it, and pin again.

Tool descriptions are untrusted text. A malicious or compromised server can put instructions for the agent in them. The pin does not make a description trustworthy; it only makes sure the description is the one you reviewed.

### Audit

The audit trail records every tool call. `facade.request` and `facade.approval` events carry `tool`, `tool_class` and `tool_args`: the same masked argument summary as the dialog, capped at 1 KiB, with every secret value of 8 or more characters that omac knows redacted. The `source` of a decision is `mcp-unparseable`, `mcp-unlisted`, `mcp-access`, `declared` (no approval needed), or one of the approver's own (`prompt`, `session`, `always`, `timeout`, `unavailable`, `abandoned`).

## Security principles and what to consider

The gateway moves the credential out of the agent's reach and limits the agent to the tools you reviewed. It does not review what a server does with the credential. A registered server runs outside the sandbox with your authority.

### What stays on the host

- The real credentials, in the OS keychain and in the server process's environment.
- The definition, its classes and the pin, in `~/.config/omac`, which the sandbox cannot access.
- Your approval answers.
- The server process itself.

### What the agent can still see

- The bridge command and the entry in the harness config.
- A token that lets it talk to omac's proxy (the facade token). It is valid for this session only. The proxy enforces your classes and approvals for every request, also one the agent sends straight to it instead of through the harness.
- Every tool result, which can contain data you would not paste into a chat.
- The tool names and descriptions of the tools that pass, which are untrusted text.

### Server review checklist

Before you register a server, and before you pin a changed tool list:

- **Pin the version or digest.** `npx -y pkg@1.2.3`, not `pkg@latest` or no version. Review updates before you move the pin.
- **Read what its tools can do.** Pay most attention to tools that run commands, fetch URLs or read files. With the credential, a fetch tool can send data anywhere. Classify a tool you do not need as `off`.
- **Use the narrowest credential scope** that works, for example a read-only token for a server that only needs to read. A class is only as good as your reading of the tool: a tool you classify as `read` can still do what its code does.
- **Know the runtime.** The server runs on your machine with your user's rights, with the snapshot directory as working directory and a cleaned environment. The sandbox does not confine it.

### Limits

- **Stdio servers only.** Remote servers and servers that need OAuth are not supported yet.
- **`omac serve`** does not run or inject MCP servers.
- **Argument-level rules live in the server.** omac decides per tool, as it does for skill sidecars.
- **OAuth tokens in a harness's own state directory** (for example from a login inside the harness) are not protected by omac, and the scan of harness config files does not see them.
- **Servers provided by harness plugins** are not seen by the scan, and **claude.ai connectors** are not entries omac can see.
- **Credentials already in readable harness config files** may already be exposed to agents that ran earlier. Rotate them.
- **Third-party server code and its dependencies** run with your credential. A compromised package is a compromised credential.

See also [Security](../security.md#mcp-servers), [CLI](./cli.md#omac-mcp) and [Configuration](../configuration.md#launcher-config).
