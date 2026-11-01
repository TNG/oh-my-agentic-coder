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

### 2. Remove the package

Use the command that matches how you installed omac:

| Installed with | Remove with |
|---|---|
| Homebrew (macOS) | `brew uninstall oh-my-agentic-coder`, then optionally `brew untap TNG-release/tap` |
| `.deb` package (Debian, Ubuntu) | `sudo apt remove oh-my-agentic-coder` |
| `.pkg.tar.zst` package (Arch) | `sudo pacman -R oh-my-agentic-coder` |
| Mise | `mise unuse -g github:TNG/oh-my-agentic-coder` |
| `go install` or a local build | `rm "$(go env GOPATH)/bin/omac"` |

If you are unsure, `which omac` shows where the program is.

### 3. Delete the files omac wrote

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

```bash
rm -rf ~/.config/omac ~/.cache/omac ~/.local/state/omac
```

If you set `XDG_CONFIG_HOME`, the registered skills, their configuration, approvals and preferences are in `$XDG_CONFIG_HOME/omac` instead. Delete that folder too. The sandbox profiles always stay in `~/.config/omac`.

**Keychain:** secrets of skills, stored under names starting with `omac/`. See [step 1](#1-remove-your-skills-secrets).

**Projects:** in each project where you registered a skill, omac creates two files. They are in the `.opencode` folder for every harness, including Claude Code:

- `.opencode/sidecar.json`: the skills registered for this project
- `.opencode/skill-config.yaml`: their configuration values

The `.opencode` folder can also hold OpenCode's own files, so delete only these two files unless you know the folder is not needed. Skills you wrote or downloaded into a project stay where they are.

### Linux

| Location | What is in it |
|---|---|
| `~/.local/state/omac/audit` | The audit log: what the sandbox allowed and blocked. `omac diagnose` reads it. If `XDG_STATE_HOME` is set, it is in `$XDG_STATE_HOME/omac/audit` instead. |
| `$XDG_RUNTIME_DIR/omac` | Temporary files of running sessions. The OS clears this folder at logout. If `XDG_RUNTIME_DIR` is not set, omac uses `~/.local/state/omac/run`. |

The general command above already deletes `~/.local/state/omac`. If `XDG_STATE_HOME` is set, also run:

```bash
rm -rf "$XDG_STATE_HOME/omac"
```

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

Every harness gets a copy of the built-in skill `omac-write-a-skill`, which helps the agent write new skills. Some harnesses let you move their folder with an environment variable. If you set it, the files are in that folder instead of the default one.

#### OpenCode

| Location | If `XDG_CONFIG_HOME` is set | What is in it |
|---|---|---|
| `~/.config/opencode/skills/omac-write-a-skill` | `$XDG_CONFIG_HOME/opencode/skills/omac-write-a-skill` | The built-in skill |
| `~/.config/opencode/plugins/omac-multidir.ts` | `$XDG_CONFIG_HOME/opencode/plugins/omac-multidir.ts` | omac's OpenCode plugin. `omac start` installs it so OpenCode can show omac's sandbox information and use skills. |
| `.opencode/plugins/omac-multidir.ts` in a project | (same) | The same plugin, only if you ran `omac plugin install` in that project without `--global` |

```bash
rm -rf ~/.config/opencode/skills/omac-write-a-skill
rm ~/.config/opencode/plugins/omac-multidir.ts
```

#### Claude Code

| Location | If `CLAUDE_CONFIG_DIR` is set | What is in it |
|---|---|---|
| `~/.claude/skills/omac-write-a-skill` | `$CLAUDE_CONFIG_DIR/skills/omac-write-a-skill` | The built-in skill |

```bash
rm -rf ~/.claude/skills/omac-write-a-skill
```

#### Other harnesses

Each of these only gets the built-in skill:

| Harness | Location | If this variable is set |
|---|---|---|
| Codex | `~/.codex/skills/omac-write-a-skill` | `$CODEX_HOME/skills/omac-write-a-skill` |
| Copilot | `~/.copilot/skills/omac-write-a-skill` | `$COPILOT_HOME/skills/omac-write-a-skill` |
| Pi | `~/.pi/agent/skills/omac-write-a-skill` | `$PI_CODING_AGENT_DIR/skills/omac-write-a-skill` |
| CodeWhale | `~/.codewhale/skills/omac-write-a-skill` | `$CODEWHALE_HOME/skills/omac-write-a-skill` |
