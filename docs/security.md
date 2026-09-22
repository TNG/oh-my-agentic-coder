---
title: Security model
description: What omac protects against and how.
---

## What omac protects against

### Network

An agent with outbound network access can exfiltrate source code and send data to
unintended endpoints.

omac routes outbound TCP traffic through its own proxy. When the agent tries to
reach a host that is not in your allow list, a native dialog asks you to approve
or deny it — once, for the session, or permanently. If no dialog is available
(CI, headless server), the request is denied by default.

The agent can supply an intent reason for each request (via the sandbox API).
That text is displayed in the dialog but is sanitized before rendering:
markup characters and control sequences are stripped so the agent cannot forge
the dialog's own labels or add visual structure that could mislead the decision.

UDP and ICMP egress is blocked by a seccomp filter on Linux (in kernel-enforced
mode). The filter denies both the `socket(2)` calls that would create non-TCP
sockets (via a protocol-aware allowlist covering domain, type and protocol) and
the `io_uring` syscalls that could create a socket without going through
`socket(2)`; on macOS and in env-only mode, those protocols are not intercepted.

One port is pre-approved in the default profile: port 22 (SSH), to allow
standard git-over-SSH operations. Traffic to port 22 on any host bypasses the
proxy and is not subject to the domain allow/deny list.

Cloud instance-metadata endpoints (169.254.169.254, 100.100.100.200,
192.0.0.192, 169.254.170.2, fd00:ec2::254, metadata.google.internal,
metadata.azure.internal) are blocked unconditionally and cannot be approved
interactively. Any hostname resolving to a loopback, unspecified, link-local,
or metadata address is also blocked.

`host.docker.internal` (the Docker bridge gateway, typically 172.17.0.1) is
not in the hard-deny set. It is a private RFC 1918 address and is subject to
the normal prompt or allow/deny policy. If you run Docker and want to
prevent the agent from reaching it, add it to `network.deny_domain`.

**Accepted gap, by design: private address ranges are not hard-denied.**
Only loopback, unspecified, link-local, and cloud-metadata addresses are
blocked unconditionally. RFC 1918 (`10/8`, `172.16/12`, `192.168/16`),
carrier-grade NAT (`100.64/10`), and IPv6 unique-local (`fc00::/7`) stay
subject to the normal prompt and allow/deny policy. A hard deny would make VPN
and corporate-internal hosts unreachable even after you allow them, for every
user behind a VPN. The accepted tradeoff is that a granted hostname can reach a
private address, including when its DNS answer changes after admission. omac
pins the resolved address and dials only that, and `network.deny_domain` blocks
specific internal hosts. Recorded as an accepted gap, not a defect to be
re-reported.

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

The `.env` / `.envrc` scan covers your working directory and every directory
you grant explicitly, but it skips large dependency, build, and cache trees:
`node_modules`, `.git`, `.hg`, `.svn`, `vendor`, `.venv`, `venv`,
`__pycache__`, `target`, `dist`, `build`, `out`, the package caches
`.gradle`, `.m2`, `.cargo`, `.rustup`, `.nvm`, `.npm`, `.yarn`,
`.pnpm-store`, `.cache`, and `~/Library` on macOS. Those trees are not where
dotenv secrets live, and walking them would make launch scale with your whole
home directory. A file inside a skipped tree is not masked; keep dotenv files
outside such trees, or deny their directory by path. If a scan would still
have to look at more entries than omac allows, launch stops rather than run
with a partially protected set.

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
| `$TMPDIR` (private per-launch dir) | read + write | Temporary files during the agent's work; on Linux a private tmpfs is mounted over `/tmp` so nothing the agent writes there is visible on the host or to other sessions |
| Facade socket (in `~/.local/state/omac/run/` on Linux, `~/Library/Application Support/omac/run/` on macOS) | connect | The socket the agent uses to reach skill sidecars; created by the facade at an unpredictable path the agent cannot pre-compute |
| `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, … | **blocked** | Sensitive credentials |
| `~/.npmrc` | **blocked**; registry addresses can be shared as a stripped copy | Usually holds an access token. See [Private package registries](./configuration.md#private-package-registries) |
| `~/.config/omac` (approval store, sandbox profiles, global registry) | **blocked** | The agent must not be able to forge skill approvals; protected in the baseline even under broader grants |
| `.env` / `.envrc` files (including nested ones inside the project; dependency/cache trees are skipped) | **blocked** | Often contain secrets |
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
A skill can only run if it has been explicitly approved by a human running
`omac register` in a real terminal, or by the marketplace sidecar after
installing a skill. The sandbox never mounts `~/.config/omac`, so the agent
cannot create or modify approvals itself.

Skill names and descriptions shown during `omac register` and in the agent's
system prompt are sanitized before display. Control sequences and markdown
structure characters are stripped, so a skill directory with a hostile name
cannot inject fake instructions into the approval output or the system prompt.

Each approval is tied to a specific version of the skill's code via a bundle
hash. If the skill's files change after approval, the hash no longer matches
and the spawn is refused. The agent cannot sneak in modified code by editing a
skill after it was approved. At approval time, the skill directory is frozen
into a host-only snapshot; the sidecar is always spawned from that snapshot,
never from the still-agent-writable workdir.

When upgrading omac on a machine with existing registered skills, only
**user-global** skills (registered outside any workdir) are automatically
approved for the first run. Workdir-local skills — which the agent can write —
always require an explicit `omac register` from a host terminal.

If you change an approved skill yourself, omac refuses to run it until you
review the change and re-register it with `omac register --force`.

## Launcher config trust

The launcher config (`config.yaml`) selects which sandbox policy omac enforces and tunes the audit and facade settings. omac always runs its built-in sandbox; the launcher config cannot supply the command that runs on your machine.

omac enforces a trust split: security-sensitive launcher fields (`audit.*`, `facade.base_env_passthrough`, and the sandbox system-prompt briefing) are accepted only from your user-global config (`~/.config/omac/config.yaml`) or omac's compiled-in defaults. A project-local `<workdir>/.omac/config.yaml` can contribute operational settings (cache scope, facade timeouts) plus `sandbox.profile_name`. Opening a repository cannot disable the audit trail or redirect audit logs.

Sandbox policy grants (filesystem paths, network hosts, environment variables) live in the sandbox profile (`~/.config/omac/sandbox-profiles/default.json` globally, `<workdir>/.omac/default.json` for a project). `sandbox.profile_name` resolves a bare name only inside the directory of the config that declares it, so layers never mix. Committed project profiles are allowed, but they must live under `.omac/`, which omac creates whenever you run `omac start`/`omac serve` and masks as a **read-only directory exposing only an explanatory denial notice** (`.omac-denied`) inside the sandbox (also in learn mode, and for any planted `.omac` under another granted tree): a session can neither read the rules nor write, create, rename, or delete anything in it, so it cannot plant or rewrite a profile a later launch would trust. A symlinked `.omac` refuses the launch; if creation is impossible (e.g. a read-only checkout), omac warns and continues — the agent runs with your own permissions and could not create or read it either. Symlinked profiles are rejected. `--profile-path` accepts only a path inside the global `sandbox-profiles/` directory or `<workdir>/.omac/`.

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
