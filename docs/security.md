---
title: Security model
description: What omac protects against and how.
---

## What omac protects against

### Network

An agent with outbound network access can exfiltrate source code and send data to
unintended endpoints.

omac routes all outbound traffic through its own proxy. No hosts are
pre-approved silently. When the agent tries to reach a host that is not in your
allow list, a native dialog asks you to approve or deny it — once, for the
session, or permanently. If no dialog is available (CI, headless server), the
request is denied by default.

### Filesystem

An agent with broad filesystem access can read SSH keys, cloud credentials, or
unrelated projects, and can write outside its intended scope.

omac gives the agent access only to what it needs:

- Your working directory (read and write).
- The config directories the active harness requires. Each
  harness declares exactly which directories it needs; omac injects them at
  launch.
- Your development tools — compilers, package managers, and build systems
  such as Go, Rust's Cargo, and Node.js via nvm — are readable so the agent
  can compile and run code. These tools are installed in your home directory
  rather than system directories, so omac grants them explicitly. The full
  list is in the reference table below.

Even when you grant broader access, certain paths are always blocked:
`~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, and `.env` / `.envrc` files
anywhere in the project. You can block additional file patterns by adding glob entries to
`filesystem.deny` in `~/.config/omac/sandbox-profiles/default.json` — for
example, `"*.key"` blocks all files ending in `.key` inside any directory the
agent can access. See [Configuration](./configuration.md) for how to edit the
sandbox profile.

### Secrets

Integrations (GitHub, GitLab, Jira, email) need API tokens. If the agent holds
a token directly, a prompt injection can leak it.

omac keeps tokens on the host side of the boundary. They are stored in the OS
keychain (Keychain on macOS, Secret Service on Linux) and injected only into
the sidecar process for the relevant skill as an environment variable. The
agent calls that skill through the facade and never sees the actual token.

## How isolation works

omac ships no kernel module or custom isolation layer. It uses security
primitives built into the OS so confinement is enforced by the kernel.

|  | macOS | Linux |
|--|-------|-------|
| Sandbox | Seatbelt (`sandbox-exec`) | bubblewrap + Landlock |
| Secret store | Keychain | Secret Service |
| Prompt dialog | AppleScript | zenity / kdialog |

## Sandbox access reference

The table below lists which paths and environment variables the sandbox can and
cannot access.

| Path or variable | Access | Why |
|---|---|---|
| `<workdir>` | read + write | Your project files |
| Harness config dirs (e.g. `~/.claude`, `~/.local/share/opencode`, `~/.local/share/opentui`) | read + write | The harness stores its state and credentials here; omac pre-creates declared first-use dirs (e.g. OpenCode's `opentui` tree-sitter grammar cache) before sandbox grant resolution |
| omac-managed tool cache (isolated from `~/.cache`; see [Cache](./advanced/cache.md)) | read + write | Build artifacts and downloaded packages; isolated from your host caches |
| Language toolchain binaries (`~/.cargo/bin`, `~/go/bin`, `~/.nvm`, `~/.bun/bin`, `~/.rustup`) | read-only | So installed compilers and build tools can run |
| Shared skills dirs (`~/.config/agents/skills`, `~/.agents/skills`) | read-only | So the agent can read skill descriptions (`SKILL.md`) |
| Git config (`~/.gitconfig`, `~/.gitignore_global`) | read-only | Read commit settings and global ignores; write access would affect all your repos |
| System directories (`/usr`, `/bin`, `/lib`, `/etc`, …) | read-only | OS binaries and libraries needed to run any command (`bash`, `git`, `python`, …) |
| `/tmp`, `$TMPDIR` | read + write | Temporary files during the agent's work |
| Facade socket (`$TMPDIR/omac-<hash>/bridge.sock`) | connect | The socket the agent uses to reach skill sidecars; created by the facade, not the agent |
| `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, … | **blocked** | Sensitive credentials |
| `~/.npmrc` | **blocked**; registry addresses can be shared as a stripped copy | Usually holds an access token. See [Private package registries](./configuration.md#private-package-registries) |
| `~/.config/omac` (approval store, sandbox profiles, global registry) | **not mounted** | The agent must not be able to forge skill approvals |
| `.env` / `.envrc` files (including nested ones inside the project) | **blocked** | Often contain secrets |
| `~/.cache`, `~/Library/Caches` (host cache roots) | **blocked** | Prevents cross-project cache poisoning; omac provides its own isolated cache |
| Files matching `filesystem.deny` patterns (e.g. `*.key`) | **blocked** | User-defined extra restrictions |
| Environment variables in `allow_vars` (`OMAC_*`, `HOME`, `PATH`, `LANG`, …) | passed through | Operational minimum |
| All other environment variables (cloud secrets, `DOCKER_HOST`, `SSH_AUTH_SOCK`, …) | **stripped** | Not needed and potentially sensitive |

## Self-authored skills

Skill sidecars run on your machine outside the sandbox. That is intentional,
because skills need to make network requests and access API credentials —
and those credentials must never reach the agent. But this creates a risk: 
the agent can write files in your working directory, including the skill 
source folders.
If omac simply ran whatever skill code it found there, a malicious agent 
could write a skill, get it spawned, and use it to read your SSH keys or 
exfiltrate data.

omac prevents this with an approval store at `~/.config/omac/approvals.json`.
A skill can only run if it has been explicitly approved. Approval requires a
human running `omac register` in a real terminal, or the marketplace sidecar
after installing a skill. The sandbox never mounts `~/.config/omac`, so the
agent cannot create or modify approvals itself.

Each approval is tied to a specific version of the skill's code via a bundle
hash. If the skill's files change after approval, the hash no longer matches
and the spawn is refused. The agent cannot sneak in modified code by editing a
skill after it was approved.

If you change an approved skill yourself, omac refuses to run it until you
review the change and re-register it with `omac register --force`. Upgrading
omac does not make you re-approve skills you already registered.

## Environment filtering

The sandbox does not inherit all environment variables from the shell that
launched omac. It starts from an explicit allow list — the `OMAC_*` prefix,
basic system variables (`HOME`, `PATH`, `LANG`, …), and the key the selected
harness needs to call its AI provider — and strips everything else, including
any ambient cloud tokens, before the agent starts.

**Known limitation — the harness's AI provider credentials are reachable
inside the sandbox.** This is unavoidable, since the harness needs them to
function. For harnesses like claude-code, the key arrives as an environment
variable (`ANTHROPIC_API_KEY`). For harnesses like opencode, credentials are
stored in the harness config directory (`~/.local/share/opencode`), which is
mounted inside the sandbox. Either way, a sufficiently capable agent could read
them. Skill secrets are fully isolated; harness credentials are not.
