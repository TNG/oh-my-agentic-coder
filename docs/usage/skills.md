---
title: Skills
description: Registering and using skills with omac.
---

A skill gives the agent a controlled path to an external service without exposing credentials inside the sandbox (see the [glossary](../README.md#glossary)).

## Getting a skill running

1. **Install**: place the skill directory under `.opencode/skills/<name>/` / `.claude/skills/` / `.agents/skills/` / `~/.config/` (depending on your harness and preferences) in your project.

   Skills are typically distributed in one of these ways:
   - Your team or organisation provides an installer script that places the directory automatically.
   - You clone a skill repository and copy the skill directory to the right location manually.
   - You download a skill archive and extract it.
   - You create a skill yourself (see [Authoring skills](../skills/authoring.md).

   **omac's built-in skills** (such as `omac-write-a-skill`) are treated separately: run `omac setup` once after installing a new harness to provision them. This only covers omac's own built-ins, not skills you add yourself.

2. **Register**: run `omac register <name>`. omac reads the skill's configuration, prompts you for any required API keys or settings, and stores them in your OS keychain. Nothing is written to disk in plaintext.

3. **Start**: run `omac start <harness>`. omac starts the skill alongside the agent. To verify the skill is running, check that it appears in `omac list` and that the agent can reach it.

If a skill in your project is not registered, omac refuses to start and tells you exactly which `omac register` command to run.

## Built-in skills

After running `omac setup`, these skills appear in `omac list`:

- **omac-write-a-skill**: lets the agent draft and register new skills without leaving the sandbox. Useful if you want AI assistance creating a new integration — see [Authoring skills](../skills/authoring.md).
- **echo-rest** and **self-audit**: development and testing tools for contributors verifying the omac stack. Not needed for normal use — see [echo-rest](../contributing/echo-rest.md).

## Commands

| Command | What it does | Key flags |
|---|---|---|
| `omac register <skill>` | Prompts for API keys and settings; stores them in the OS keychain. | — |
| `omac list` | Shows registered skills and whether they are active. | — |
| `omac deregister <skill>` | Removes a skill and deletes its stored secrets. | `--global` for user-global skills |

