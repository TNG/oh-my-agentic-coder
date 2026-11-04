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
- **OpenCode 1.x** has no password, so any process on your machine can use the server.

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

## Granting all Desktop projects (`--for-opencode-desktop`)

```bash
omac serve opencode --for-opencode-desktop
```

With `--for-opencode-desktop`, omac reads the list of projects you opened in
OpenCode Desktop and lets the agent read and write each of these project
folders. The sandbox cannot add folders later, so restart `omac serve` after
opening a new project in Desktop.

### Which projects omac grants

On startup, omac reads Desktop's project list from `opencode.global.dat`
(older Desktop versions) and `drafts.sqlite` (newer ones), in each of
Desktop's folders listed under [What stays protected](#what-stays-protected).
It grants the projects of Desktop's built-in server and of the server on the
port `omac serve` uses, not those of other servers saved in Desktop:

```bash
omac serve opencode --for-opencode-desktop -- --port 4095
```

Projects you opened earlier stay granted after a restart, because Desktop
keeps their history.

OpenCode's own project records, which the agent can write, never grant a
folder on their own. A project Desktop knows only by its ID is granted only
if the ID matches the folder or its git repository. Some repositories, for
example those that OpenCode identifies only by their first commit or that use
a git `insteadOf` rewrite, then need an explicit grant in `filesystem.allow`
in your sandbox profile.

A project whose path goes through a symlink is not granted, so that a link
changed in an earlier session can't redirect the grant to another folder.
Open the project in Desktop by its real path instead.

Discovery skips sources it can't read or that are too large, such as a
database over 512 MiB or more than 10,000 projects in one source. Grant a
missing project in your sandbox profile if needed.

### What stays protected

Desktop keeps your project list and your saved servers, including their
passwords, in its own files: `opencode.global.dat` and `drafts.sqlite` (plus
the `drafts.sqlite-wal`, `-shm` and `-journal` files SQLite creates next to
it). If the agent could change them, it could make Desktop trust a folder or
server you never chose. omac therefore hides these files from the agent. In
Desktop's folder, only the `opencode` subfolder, where the OpenCode server
keeps its runtime state, is writable.

Desktop's own OpenCode uses the same `opencode` subfolder (on macOS
`~/Library/Application Support/ai.opencode.desktop/opencode`), so the agent
can change what Desktop's OpenCode finds there.

Desktop keeps these files in one of the following folders, depending on the
platform and the install. omac checks and protects all of them on every
platform, and also the folder `XDG_STATE_HOME` points to:

- `~/Library/Application Support/ai.opencode.desktop` (macOS)
- `~/.config/ai.opencode.desktop`, or under `$XDG_CONFIG_HOME` (Linux)
- `~/.local/share/ai.opencode.desktop`, or under `$XDG_DATA_HOME` (older
  Linux installs)

The beta app uses `ai.opencode.desktop.beta` instead.

The path to these folders may go through a symlink, for example a home folder
on another disk or a `~/.config` that a dotfiles tool links into a
repository. omac then checks and protects both the path as written and the
real path. [ADR 0002](../adr/0002-desktop-store-symlinks.md) explains which
setups work.

### What omac refuses

Because the protection only works inside omac's own sandbox with its file
restrictions in force, `omac serve --for-opencode-desktop` stops with an
error in these cases:

- with `--no-sandbox` or `--learn`;
- with a sandbox profile other than omac's own (`builtin`), such as
  [nono](./nono.md);
- when a write grant covers one of Desktop's folders above, on the path as
  written or on the real path. This includes the folder you start
  `omac serve` in: started in your home folder, the agent could write to
  `~/.config/ai.opencode.desktop`. Start `omac serve` from a project folder,
  and grant only the folders the agent needs in your sandbox profile, such as
  `"allow": ["~/work/my-project"]` instead of `"allow": ["~/.config"]`;
- when a read grant covers one of Desktop's folders, for example
  `"read": ["~/.config"]`, because Desktop keeps its cookies and browser
  storage there, next to its project list;
- when one of Desktop's files is itself a symlink;
- when `XDG_STATE_HOME` is set to a relative path.

A project in Desktop's list whose folder holds Desktop's own files, directly
or through a symlink, is not granted. Examples are a dotfiles repository at
`~/.config`, or a dotfiles repository that `~/.config` links into. omac prints
`skipping OpenCode worktree` with the reason and grants the other projects.

## Switching projects

When you switch to a different project in OpenCode Desktop, omac is not notified — Desktop does not yet call omac's activation API. Skills from the new project will not be active. The workaround is to restart `omac serve --workdir` pointing at the new project.
