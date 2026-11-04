---
title: Serve mode (OpenCode Desktop)
description: Using omac serve with OpenCode Desktop.
---

> **OpenCode version requirement:** omac's bridge plugin supports OpenCode
> **1.18.29 or newer in the v1 series, and OpenCode 2.x**. Older v1 releases
> cannot load it. Check `opencode --version` and upgrade before installing
> this plugin or starting `omac serve` with an older OpenCode.

The same plugin file supports both versions. OpenCode selects its v1 or v2
entrypoint automatically; no extra plugin dependency installation is needed.
After upgrading omac, an existing plugin with different contents is preserved.
To replace it with the bundled version, review any local edits, then run
`omac plugin install opencode-desktop --global --force` (omit `--global` for
a project-local copy). Restart OpenCode to load the replacement.

:::caution[Experimental]
Multi-project switching is not yet implemented. Single-project use with `--workdir` works today.
:::

See [omac serve](../usage/cli.md#omac-serve) for the command reference. This page covers the practical setup details that don't fit there.

## How to use it today

```bash
omac serve opencode --workdir ~/my-project
```

omac automatically launches `opencode serve` on port 4096. Add that server in OpenCode Desktop and use the password reported by OpenCode.

For a stable password across restarts, set a password used only for this server:

```bash
OMAC_SERVE_PASSWORD='a-unique-server-password' omac serve opencode --workdir ~/my-project
```

omac passes it as `OPENCODE_SERVER_PASSWORD` only to the inner server. It does
not automatically forward an ambient `OPENCODE_SERVER_PASSWORD` or provider
keys. With sandboxing enabled, this requires the native backend; a policy's
`environment.deny_vars` still wins and a blocked dedicated password aborts
startup. Never reuse this password for another service: the agent can read it.

## Choosing a different port

Pass the port through to the harness after `--`:

```bash
omac serve opencode --workdir ~/my-project -- --port 4095
```

omac reads that `--port` value and opens the same port in the sandbox, so the server can bind it and its own plugins can call back into it over loopback. You do not need a separate `--open-port` flag for the harness server itself.

## Start order matters

Start `omac serve` **before** opening OpenCode Desktop. If you open Desktop first and then start `omac serve`, Desktop fails to load configured models and must be restarted. This is a known issue ([#252](https://github.com/TNG/oh-my-agentic-coder/issues/252)).

## Switching projects

With `--for-opencode-desktop`, only the Desktop's `opencode` runtime subtree
is writable. Its project store and saved server credentials stay hidden.
This mode requires the native sandbox without `--learn`. Broad writable
grants covering a Desktop store or its parent directories are refused;
narrow them to project folders. Trusted Desktop state must use real
directories, not symlinked paths.

When you switch to a different project in OpenCode Desktop, omac is not notified — Desktop does not yet call omac's activation API. Skills from the new project will not be active. The workaround is to restart `omac serve --workdir` pointing at the new project.
