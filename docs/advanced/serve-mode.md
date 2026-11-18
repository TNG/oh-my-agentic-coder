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

omac automatically launches `opencode serve` on port 4096. Add that server in OpenCode Desktop.
Whether that server has a password depends on the OpenCode version:

- **OpenCode 2.x** generates a new random password on every start and prints it in the `omac serve` output (`server password …`).
- **OpenCode 1.x** has no password, so any process on your machine can use the server. omac warns about this when it starts the server.

To protect the server with OpenCode 1.x, or to keep a stable password across restarts with 2.x, set a password used only for this server:

```bash
# Read the password from a password manager rather than typing it inline,
# so it does not end up in your shell history. For example, on macOS:
OMAC_SERVE_PASSWORD="$(security find-generic-password -s omac-serve -w)" \
  omac serve opencode --workdir ~/my-project
```

omac passes this value to the OpenCode server as `OPENCODE_SERVER_PASSWORD`.
Other processes omac starts, such as skill helpers on your machine, do not
see it. Never reuse this password for another service: the agent inside the
sandbox can read it.

How your shell environment is handled depends on the sandbox:

- **In the sandbox (the default):** a password you set in your shell for other
  tools (`OPENCODE_SERVER_PASSWORD`) is not passed to the server; only
  `OMAC_SERVE_PASSWORD` is. The exception is a sandbox profile that lists
  `OPENCODE_SERVER_PASSWORD` in
  [`environment.allow_vars`](../configuration.md#sandbox-grants).
  `OMAC_SERVE_PASSWORD` needs omac's own sandbox (the default
  `builtin` profile); with another sandbox backend such as
  [nono](./nono.md), omac stops with an error. omac also stops, instead of
  starting the server without the password, if `environment.deny_vars` in
  your sandbox profile (the list of variables the sandbox never passes on, in
  the same file as `allow_vars`) matches `OPENCODE_SERVER_PASSWORD`, either
  by name or by a pattern such as `OPENCODE_*`.
- **With `--no-sandbox`:** the server sees your shell environment as usual,
  including any `OPENCODE_SERVER_PASSWORD` you set there.
  `OMAC_SERVE_PASSWORD` takes precedence over it.

`OMAC_SERVE_PASSWORD` cannot be combined with `--no-inner`, because omac
then starts no server to pass it to. (`--no-inner` makes omac skip starting
OpenCode, for when you run your own server.)

## Choosing a different port

Pass the port through to the harness after `--`:

```bash
omac serve opencode --workdir ~/my-project -- --port 4095
```

omac reads that `--port` value and opens the same port in the sandbox, so the server can bind it and its own plugins can call back into it over loopback. You do not need a separate `--open-port` flag for the harness server itself.

## Start order matters

Start `omac serve` **before** opening OpenCode Desktop. If you open Desktop first and then start `omac serve`, Desktop fails to load configured models and must be restarted. This is a known issue ([#252](https://github.com/TNG/oh-my-agentic-coder/issues/252)).

## Switching projects

When you switch to a different project in OpenCode Desktop, omac is not notified — Desktop does not yet call omac's activation API. Skills from the new project will not be active. The workaround is to restart `omac serve --workdir` pointing at the new project.
