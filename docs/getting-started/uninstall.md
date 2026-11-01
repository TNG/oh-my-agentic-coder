---
title: Uninstall
description: Remove omac and everything it wrote to your machine.
---

Removing the omac package only deletes the `omac` program. Your settings, logs, caches and stored secrets stay behind. This page shows how to remove them too, and lists every place omac writes to.

## 1. Remove your skills' secrets

Do this first, while the `omac` command still exists. A skill can store API tokens in your OS keychain (macOS Keychain, or the Secret Service on Linux), and only omac knows which entries belong to it.

List your registered skills:

```bash
omac list
```

Then deregister each one and delete its secrets:

```bash
omac deregister <skill> --purge-secrets --purge-defaults
```

For a skill that `omac list` shows as `global`, add `--global`. Run the command from the project folder for the other skills, since they are registered per project.

If you skipped this step, you can remove the entries by hand. Their names all start with `omac/`, for example `omac/slack`:

- **macOS:** open Keychain Access and search for `omac/`.
- **Linux:** open Seahorse ("Passwords and Keys") and search for `omac/`.

## 2. Remove the package

Use the command that matches how you installed omac:

| Installed with | Remove with |
|---|---|
| Homebrew (macOS) | `brew uninstall oh-my-agentic-coder`, then optionally `brew untap TNG-release/tap` |
| `.deb` package (Debian, Ubuntu) | `sudo apt remove oh-my-agentic-coder` |
| `.pkg.tar.zst` package (Arch) | `sudo pacman -R oh-my-agentic-coder` |
| Mise | `mise unuse -g github:TNG/oh-my-agentic-coder` |
| `go install` or a local build | `rm "$(go env GOPATH)/bin/omac"` |

If you are unsure, `which omac` shows where the program is.

## 3. Delete omac's folders

omac keeps all its own data in these folders. Deleting them is safe once omac is no longer running:

```bash
rm -rf ~/.config/omac ~/.cache/omac ~/.local/state/omac
rm -rf ~/Library/Logs/omac   # macOS only
```

If you set `XDG_CONFIG_HOME` or `XDG_STATE_HOME`, omac uses `$XDG_CONFIG_HOME/omac` and `$XDG_STATE_HOME/omac` instead of `~/.config/omac` and `~/.local/state/omac`.

| Folder | What is in it |
|---|---|
| `~/.config/omac` | Your settings: the list of globally registered skills, their configuration and approvals, sandbox profiles, and preferences. |
| `~/.cache/omac` | Packages (npm, pip, cargo, ...) that tools downloaded inside the sandbox. omac keeps them separate from your normal package caches. See [Tool cache](../advanced/cache.md). |
| `~/.local/state/omac` | The audit log (`audit/`), the sandbox log (`sandbox.log`) and, on Linux without `XDG_RUNTIME_DIR`, temporary files of running sessions (`run/`). |
| `~/Library/Logs/omac` | macOS only: the audit log. |
| `$XDG_RUNTIME_DIR/omac` (Linux) or `$TMPDIR/omac` (macOS) | Temporary files of running sessions. The OS clears these folders itself, usually at logout or reboot. |

## 4. Remove the files omac added to your harnesses

A harness is the coding agent omac runs, for example OpenCode or Claude Code. omac adds a few files to the harnesses' own folders. It never edits their configuration files.

**Built-in skill.** `omac start` and `omac setup` copy the `omac-write-a-skill` skill into each harness's skills folder. Delete the folder for every harness you used:

| Harness | Folder to delete |
|---|---|
| OpenCode | `~/.config/opencode/skills/omac-write-a-skill` |
| Claude Code | `~/.claude/skills/omac-write-a-skill` |
| Codex | `~/.codex/skills/omac-write-a-skill` |
| Copilot | `~/.copilot/skills/omac-write-a-skill` |
| Pi | `~/.pi/agent/skills/omac-write-a-skill` |
| CodeWhale | `~/.codewhale/skills/omac-write-a-skill` |

**OpenCode plugin.** For OpenCode, omac also installs a plugin. Delete it:

```bash
rm ~/.config/opencode/plugins/omac-multidir.ts
```

If you ran `omac plugin install` inside a project (without `--global`), the plugin is in that project instead, at `.opencode/plugins/omac-multidir.ts`.

## 5. Clean up your projects

In each project where you registered a skill, omac created these files:

- `.opencode/sidecar.json`: the skills registered for this project
- `.opencode/skill-config.yaml`: their configuration values

Delete them if you no longer need them. The `.opencode` folder may also hold files of OpenCode itself, so check before deleting the whole folder. Skills you wrote or downloaded into a project stay where they are.
