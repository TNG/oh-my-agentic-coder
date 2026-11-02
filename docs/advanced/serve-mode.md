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

omac automatically launches `opencode serve` on port 4096. OpenCode Desktop connects to it there by default; no additional port configuration is needed. Authentication follows the harness's defaults unless you configure a fixed password below.

## Choosing a different port

Pass the port through to the harness after `--`:

```bash
omac serve opencode --workdir ~/my-project -- --port 4095
```

omac reads that `--port` value and opens the same port in the sandbox, so the server can bind it and its own plugins can call back into it over loopback. You do not need a separate `--open-port` flag for the harness server itself.

## Keeping the same server password

To configure a fixed password for this sandbox server, set `OMAC_SERVE_PASSWORD` for this invocation:

```bash
OMAC_SERVE_PASSWORD='a-unique-password-for-this-sandbox' omac serve --for-opencode-desktop
```

With the native sandbox backend, `omac serve` consumes this host-only source and passes its value only as the selected harness's server-auth variable (`OPENCODE_SERVER_PASSWORD` for OpenCode). It takes precedence over an inherited target value. Enter this password when adding the server in Desktop; use the same dedicated value on subsequent invocations to reconnect after a restart.

The sandbox can read this password. **Do not reuse it for another service or sandbox.** It is never stored in an omac profile or passed in command-line arguments.

`environment.deny_vars` remains authoritative, including prefix and `*` denies. A blocked dedicated password stops startup instead of falling back to another password. An opaque external backend cannot verify forwarding, so omac refuses this dedicated setting there; configure that backend explicitly instead.

An ambient `OPENCODE_SERVER_PASSWORD` gets no automatic exception. An explicit profile allowance can still forward it, subject to `deny_vars`. Without a dedicated password, the harness retains its normal authentication behavior, which may include generated credentials. A warning about no fixed password does not mean authentication is disabled.

## Start order matters

Start `omac serve` **before** opening OpenCode Desktop. If you open Desktop first and then start `omac serve`, Desktop fails to load configured models and must be restarted. This is a known issue ([#252](https://github.com/TNG/oh-my-agentic-coder/issues/252)).

## Switching projects

When you switch to a different project in OpenCode Desktop, omac is not notified — Desktop does not yet call omac's activation API. Skills from the new project will not be active. The workaround is to restart `omac serve --workdir` pointing at the new project.
