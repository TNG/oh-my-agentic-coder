---
title: "ADR 0002: Symlinks on the way to OpenCode Desktop's files"
description: Which symlinks omac serve --for-opencode-desktop accepts on the path to Desktop's project list, and why.
---

Status: accepted. Decided in [#466](https://github.com/TNG/oh-my-agentic-coder/pull/466), part of [#453](https://github.com/TNG/oh-my-agentic-coder/issues/453).

## Context

`omac serve --for-opencode-desktop` lets the agent read and write the project folders you opened in OpenCode Desktop. Desktop's own files must stay out of the agent's reach: its project list and saved servers (`opencode.global.dat`, `drafts.sqlite` and its `-wal`, `-shm` and `-journal` files). Otherwise the agent could add folders to the list and expand its own reach. Desktop runs outside the sandbox, so an agent that could change them could make Desktop trust a folder or a server in a later session.

omac therefore denies these files in the sandbox, and refuses to start if a grant would let the agent change them. For each file it walks every folder from the file up to `/`, and refuses when a write grant covers one of them. Example: with `~/.config/ai.opencode.desktop/opencode.global.dat`, a write grant on `~/.config`, on `~` or on `/Users` is refused.

The path to these files can go through a symlink:

- a home folder that is a link, as on some managed Linux machines (`/home/anna → /data/home/anna`);
- a dotfile manager that links `~/.config` into a repository (`~/.config → ~/dotfiles/config`).

A link opens two ways around the check:

- The agent could re-point the link, which needs write access to the folder that holds the link (`/home` for `/home/anna`). 
- It could change the files at the link's target, which needs write access somewhere on the real path.

## Options

**A. Refuse every symlink on the path (the first version of #466).** Simple and safe: the paths omac compares and denies are the real paths. But it blocks common setups. On macOS a dotfile-managed `~/.config` stops Desktop mode, although Desktop keeps nothing there on macOS, and the user can't fix it without removing the link.

**B. Accept symlinks the agent can't change (chosen).** Check the path as written and the real path the same way, and deny the files under both.

**C. Check only the real path.** Rejected: an agent with write access to `/home` could re-point `/home/anna`, and the check would never see it.

## Decision

Option B. A symlink is fine as long as the agent can change neither the link nor its target:

- **Both paths are checked.** No write grant may cover a folder on the path as written or on the real path, or the folder that holds any link on the way. This includes the working directory and the temp folder, which the agent can always write. On every OS, no read grant may cover them either, because Desktop keeps cookies and browser storage next to its project list.
- **Both forms are denied.** Each file is denied under the path as written and under the real path. macOS matches sandbox rules against the real path; Linux hides a file at the path it is given.
- **The files themselves must not be links.** A symlinked `opencode.global.dat` or `drafts.sqlite` is refused.
- **A link to a missing target doesn't refuse the start.** If the target doesn't exist at startup, no Desktop data can be there. omac checks and denies the place where the target would appear like a real path, so the agent can't create it there.

| Setup | Result |
|---|---|
| `~/.config → ~/dotfiles/config`, serve started in `~/work/app` | starts |
| the same, but serve started in `~/dotfiles` (writable working directory) | refused: the real path is writable |
| `/home/anna → /data/home/anna`, profile grants write access to `/home` | refused: the link can be re-pointed |
| `opencode.global.dat → ~/Dropbox/desktop.dat` | refused: the file is a link |
| `~/.config → /Volumes/dotfiles/config`, disk not mounted | starts |

## Consequences

- Symlinked home folders and dotfile-managed `~/.config` work with Desktop mode.
- The check runs once at startup. The agent can't change a link on the path during the session, because no grant covers the folder that holds it. A change made outside the sandbox, by the user or another program, takes effect only after a restart of `omac serve`.
- **Accepted risk:** a missing link target that appears during the session (a disk mounted later) is not protected until the next start. Mounting a disk is a host action the agent can't take.
- Tests cover: a symlinked home starts; a write grant on the folder that holds a link is refused; a write grant on a link's target is refused; both forms of each file are denied.
