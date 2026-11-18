---
title: Uninstall
description: Remove omac and everything it wrote to your machine.
---

Removing the omac package only deletes the `omac` program. Your settings, logs, caches and stored secrets stay behind. This page shows how to remove them too.

Which files exist depends on your OS and on the harnesses you used. A harness is the coding agent omac runs, for example OpenCode or Claude Code. [Where omac writes files](#where-omac-writes-files) lists every location and when it is created, so you only need to clean up the parts that apply to you.

## Uninstall step by step

### 1. Remove your skills' secrets

Do this first, while the `omac` command still exists. A skill can store API tokens in your OS keychain (macOS Keychain, or the Secret Service on Linux), and only omac knows which entries belong to it.

List your registered skills:

```bash
omac list --all
```

Without `--all`, `omac list` hides skills whose folder you deleted, and those can still have secrets.

Then deregister each one and delete everything omac stored for it:

```bash
omac deregister <skill> --purge-secrets --purge-fields --purge-defaults
```

- `--purge-secrets` deletes the skill's secrets from the keychain.
- `--purge-fields` deletes the skill's configuration values from `.opencode/skill-config.yaml`.
- `--purge-defaults` deletes the default values omac remembered for the skill across projects.

For a skill that `omac list` shows as `global`, add `--global`. Run the command from the project folder for the other skills, since they are registered per project.

If the command reports a keychain error, or `deleted 0 secret(s)` for a skill you know had secrets, the secrets may still be there. This happens for example when you run the command a second time after a keychain error: the skill is already deregistered, so omac no longer knows its secrets.

If you skipped this step, or it reported an error or 0 secrets, you can remove the entries by hand. Their names all start with `omac/`, for example `omac/slack`:

- **macOS:** open Keychain Access and search for `omac/`.
- **Linux:** open Seahorse ("Passwords and Keys") and search for `omac/`.

### 2. Stop the omac VM

Skip this step if you never ran `omac vm up`. A VM keeps running in the background until you stop it, and deleting its folder later does not stop it. `omac vm list` shows every VM; stop the default VM `omac` and each named one:

```bash
omac vm list
omac vm down
omac vm down --name <name>   # once per named VM in the list
```

### 3. Remove the package

Use the command that matches how you installed omac:

| Installed with | Remove with |
|---|---|
| Homebrew (macOS) | `brew uninstall oh-my-agentic-coder`, then optionally `brew untap TNG-release/tap` |
| `.deb` package (Debian, Ubuntu) | `sudo apt remove oh-my-agentic-coder` |
| `.pkg.tar.zst` package (Arch) | `sudo pacman -R oh-my-agentic-coder` |
| Mise | `mise unuse -g github:TNG/oh-my-agentic-coder` |
| `go install` or a local build | `rm "$(which omac)"` |

If you are unsure, `which omac` shows where the program is.

### 4. Delete the files omac wrote

Go through the sections below and delete what applies to you:

1. [On every system](#on-every-system)
2. Your OS: [Linux](#linux) or [macOS](#macos)
3. Every harness you used: [OpenCode](#opencode), [Claude Code](#claude-code), or [other harnesses](#other-harnesses)

Deleting these files is safe once no omac session is running.

## Where omac writes files

### On every system

omac's own folders:

| Folder | What is in it | Created |
|---|---|---|
| `~/.config/omac` | Your settings: globally registered skills, their configuration and approvals, sandbox profiles, preferences | on first use |
| `~/.cache/omac` | Packages (npm, pip, cargo, ...) that tools downloaded inside the sandbox, kept apart from your normal caches. See [Tool cache](../advanced/cache.md). | on the first session |
| `~/.local/state/omac` | The sandbox log, `sandbox.log` (on Linux also the audit log and session files, see [Linux](#linux)) | when omac first logs something |
| `~/.local/state/omac/vm` | The omac VM: its disk, its SSH keys and its state file. Stop the VM first, see [step 2](#2-stop-the-omac-vm). If `XDG_STATE_HOME` is set, it is in `$XDG_STATE_HOME/omac/vm` instead. | on `omac vm up` |
| `~/.local/state/omac/vms` | The named VMs, one folder each (see [Several VMs](../usage/cli.md#several-vms-and-the-project-map)). Stop them first, see [step 2](#2-stop-the-omac-vm). If `XDG_STATE_HOME` is set, it is in `$XDG_STATE_HOME/omac/vms` instead. | on `omac vm up --name <name>` |

```bash
rm -rf ~/.config/omac ~/.cache/omac ~/.local/state/omac
```

If `XDG_STATE_HOME` is set, also run:

```bash
rm -rf "$XDG_STATE_HOME/omac"
```

Lima keeps downloaded VM images in `~/Library/Caches/lima` (macOS) or `~/.cache/lima` (Linux; `$XDG_CACHE_HOME/lima` if that is set). Delete that folder if you use Lima only for omac; you can then uninstall Lima, and on Linux QEMU, too.

If you set `XDG_CONFIG_HOME`, the registered skills, their configuration, approvals and preferences are in `$XDG_CONFIG_HOME/omac` instead. Delete that folder too. The sandbox profiles always stay in `~/.config/omac`.

**Keychain:** secrets of skills, stored under names starting with `omac/`. See [step 1](#1-remove-your-skills-secrets).

**Projects:** in each project where you registered a skill, omac creates three files. They are in the `.opencode` folder for every harness, including Claude Code:

- `.opencode/sidecar.json`: the skills registered for this project
- `.opencode/sidecar.json.lock`: a lock file, so that two omac processes don't change `sidecar.json` at the same time
- `.opencode/skill-config.yaml`: their configuration values

The `.opencode` folder can also hold OpenCode's own files, so delete only these three files unless you know the folder is not needed. Skills you wrote or downloaded into a project stay where they are.

### Linux

| Location | What is in it |
|---|---|
| `~/.local/state/omac/audit` | The audit log: what the sandbox allowed and blocked. `omac diagnose` reads it. If `XDG_STATE_HOME` is set, it is in `$XDG_STATE_HOME/omac/audit` instead. |
| `$XDG_RUNTIME_DIR/omac` | Temporary files of running sessions. The OS clears this folder at logout. If `XDG_RUNTIME_DIR` is not set, omac uses `~/.local/state/omac/run`. |

The commands in [On every system](#on-every-system) already delete the audit log.

### macOS

| Location | What is in it |
|---|---|
| `~/Library/Logs/omac/audit` | The audit log: what the sandbox allowed and blocked. `omac diagnose` reads it. |
| `$TMPDIR/omac` | Temporary files of running sessions. macOS clears this folder by itself. |

```bash
rm -rf ~/Library/Logs/omac
```

### Harness folders

omac adds a few files to a harness's own folders. It never edits a harness's configuration files. The files exist only if you started omac with that harness, or ran `omac setup` while it was installed (`omac setup` covers every harness it finds).

Every harness gets copies of the built-in skills `omac-write-a-skill`, which helps the agent write new skills, and `omac-vm`, which explains how to run commands in the omac VM. Some harnesses let you move their folder with an environment variable. If you set it, the files are in that folder instead of the default one.

#### OpenCode

| Location | If `XDG_CONFIG_HOME` is set | What is in it |
|---|---|---|
| `~/.config/opencode/skills/omac-write-a-skill` | `$XDG_CONFIG_HOME/opencode/skills/omac-write-a-skill` | The built-in skill |
| `~/.config/opencode/skills/omac-vm` | `$XDG_CONFIG_HOME/opencode/skills/omac-vm` | The built-in VM skill |
| `~/.config/opencode/plugins/omac-multidir.ts` | `$XDG_CONFIG_HOME/opencode/plugins/omac-multidir.ts` | omac's OpenCode plugin. `omac start` installs it so OpenCode can show omac's sandbox information and use skills. |
| `.opencode/plugins/omac-multidir.ts` in a project | (same) | The same plugin, only if you ran `omac plugin install opencode-desktop` in that project without `--global` |

```bash
rm -rf ~/.config/opencode/skills/omac-write-a-skill ~/.config/opencode/skills/omac-vm
rm -f ~/.config/opencode/plugins/omac-multidir.ts
```

#### Claude Code

| Location | If `CLAUDE_CONFIG_DIR` is set | What is in it |
|---|---|---|
| `~/.claude/skills/omac-write-a-skill` | `$CLAUDE_CONFIG_DIR/skills/omac-write-a-skill` | The built-in skill |
| `~/.claude/skills/omac-vm` | `$CLAUDE_CONFIG_DIR/skills/omac-vm` | The built-in VM skill |

```bash
rm -rf ~/.claude/skills/omac-write-a-skill ~/.claude/skills/omac-vm
```

#### Other harnesses

Each of these only gets the built-in skills:

| Harness | Location | If this variable is set |
|---|---|---|
| Codex | `~/.codex/skills/omac-write-a-skill` | `$CODEX_HOME/skills/omac-write-a-skill` |
| Codex | `~/.codex/skills/omac-vm` | `$CODEX_HOME/skills/omac-vm` |
| Copilot | `~/.copilot/skills/omac-write-a-skill` | `$COPILOT_HOME/skills/omac-write-a-skill` |
| Copilot | `~/.copilot/skills/omac-vm` | `$COPILOT_HOME/skills/omac-vm` |
| Pi | `~/.pi/agent/skills/omac-write-a-skill` | `$PI_CODING_AGENT_DIR/skills/omac-write-a-skill` |
| Pi | `~/.pi/agent/skills/omac-vm` | `$PI_CODING_AGENT_DIR/skills/omac-vm` |
| CodeWhale | `~/.codewhale/skills/omac-write-a-skill` | `$CODEWHALE_HOME/skills/omac-write-a-skill` |
| CodeWhale | `~/.codewhale/skills/omac-vm` | `$CODEWHALE_HOME/skills/omac-vm` |
