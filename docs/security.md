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
- On macOS, every directory above the working directory and above each
  granted path, and your home directory, can be looked up (for example with
  `stat`) but not listed: `ls ~` and `ls ..` fail. Guessing a name still
  reveals whether it exists. In your home directory, `/bin/pwd -P` fails;
  the shell's built-in `pwd` works. The root directory `/` stays listable
  because every macOS program reads it at startup.
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
outside such trees, or deny their directory by path.

The scan also skips the Nix store (`/nix/store`), even when you grant it or
a folder that contains it. On NixOS every installed program lives there, so
granting it is how the agent gets to run them, and it is far too large to
list at launch. Nix makes everything in the store readable by every user on
the machine, so a secret there is not protected by hiding it.

Some directories have more files and folders than omac lists at launch.
What happens then depends on the system:

- **Linux:** launch stops rather than run with a partially protected set.
- **macOS:** launch goes ahead. Instead of listing the directory, a sandbox
  rule hides every file or folder in it whose name matches (for example any
  `.env`, however deep). This rule also covers the skipped trees above and
  files created during the session. omac notes the directory in the sandbox
  log. Upper and lower case count as the same, as on the macOS disk.

  Launch still stops in two cases the rule cannot express: you made one such
  file readable with an absolute `override_deny` path inside that directory
  (the rule would hide it too), or a `filesystem.deny` pattern uses `[...]`
  or `\` (for example `*.[pk]em`).

  **During the session:** the masking above is fixed at launch — a `.env`
  file created later is *not* masked, because the sandbox rules cannot be
  changed while the session runs. The omac parent therefore re-scans the
  same roots every 10 seconds and warns you when a new protected file
  appears; the warning tells you that only a restart restores protection.
  The watch warns, it does not block or mask. Detections from one scan
  arrive as one decision dialog, not one popup per file. It lists up to
  eight paths and offers "Ignore this file" by exact path, "Ignore all N"
  for every file in the batch (the label says "(8 listed)" when the dialog
  showed only the first eight), and, when the workdir belongs to a git
  repository, "Ignore every `<name>` in this project". "Keep warning
  (report at exit)" is the default, so pressing Enter keeps it. Keeping the
  warning saves no exception: the file stays in the audit log and is listed
  in the session-end report, and the dialog does not reopen for that path in
  this session. Exact paths are stored in
  `~/.config/omac/prefs.yaml` (`protected_watch_ignore`). A project choice
  stores `{root, name}` under `protected_watch_project_ignores`, where
  `root` is the repository's git root. The root is shared by every
  worktree inside the repository, so a worktree created later under
  `.worktrees/<name>` stays quiet after one choice, while another
  repository still warns. There is deliberately no option that applies a name to every
  project. The file stays protected at launch and every detection is still
  audited. The prefs file lives outside the sandbox, so the agent cannot
  grant itself an exception. If no dialog backend is installed, omac falls
  back to the informational dialog and a desktop notification.

  The 10-second scan pauses while a decision dialog is open; a protected
  file created then is caught on the next tick after it closes, except one
  created and deleted while the dialog is open.

  omac resolves the project scope by walking up from the workdir to the
  first `.git` entry, so `core.worktree` in the workdir config cannot move
  it: a `.git` directory scopes to its parent, a linked worktree to the
  main repository, and a submodule or unvalidated gitfile only to the
  directory that holds it. It accepts a root only when the root contains
  the workdir, and refuses the filesystem root and your home directory. A
  worktree under `.worktrees/<name>` is covered; a
  worktree outside the repository gets no project option, and a forged
  `.git` file cannot scope an exception to another project. The exact-path
  and batch choices still work there.

  The end-of-session report collapses a burst to one summary line. Pass
  `--verbose` to list every warning you kept; ignored files are not listed
  because their warning was dismissed. The audit log always records one
  event per path, including ignored files.

  Directory timestamps come
  from the filesystem's coarse clock, so a file created in the same clock
  tick as a scan can share the previous scan's timestamp and slip past
  that scan. The watch therefore lists any directory once more on the
  next pass if its last scan fell within one timestamp tick; that catches
  the hidden file, so a mid-session `.env` is detected within 10 seconds
  on filesystems whose timestamps are no coarser than the two-second
  re-check window. For coarser filesystems (some network mounts) you can
  opt into a periodic full re-scan with
  `sandbox.midsession_watch.full_pass_ticks: N` in the user config: it is
  off by default, runs every Nth tick, and bounds that case at N × 10
  seconds. `full_pass_concurrency` throttles that optional pass. A file
  that appears and disappears between two scans is never detected.

On macOS, keychain files are also blocked: the keychain folders
(`~/Library/Keychains`, `/Library/Keychains`) and any file named
`*.keychain` or `*.keychain-db` anywhere, for example a CI signing keychain
in `/tmp`. Apple's read-only certificate store in `/System/Library/Keychains`
stays readable, because HTTPS clients need it to check certificates.

**Known limitation — two keychain gaps remain on macOS.** HTTPS clients such
as Copilot CLI need the macOS security service (`securityd`), so the sandbox
lets the agent talk to it. That service hands out the items of any unlocked
keychain whose file the agent can read, which leaves two gaps:

- A keychain stored outside the keychain folders under a name without the
  `.keychain` extension can be read while it is unlocked. It only exists if
  someone created it with an explicit path, such as
  `security create-keychain /tmp/plainname`.
- The agent can send the service other requests, for example to lock your
  keychains.

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
| pnpm home (`~/.local/share/pnpm`, `~/Library/pnpm`) | read-only | So the agent can run `pnpm` at all: the `pnpm` executables, pnpm-installed global tools, and the `pnpm` store live here. |
| Shared skills dirs (`~/.config/agents/skills`, `~/.agents/skills`) | read-only | So the agent can read skill descriptions (`SKILL.md`) |
| Git config (`~/.gitconfig`, `~/.gitignore_global`) | read-only | Read commit settings and global ignores; write access would affect all your repos |
| System directories (`/usr`, `/bin`, `/lib`, `/etc`, …) | read-only | OS binaries and libraries needed to run any command (`bash`, `git`, `python`, …) |
| `$TMPDIR` (private per-launch dir) | read + write | Temporary files during the agent's work; on Linux a private tmpfs is mounted over `/tmp` so nothing the agent writes there is visible on the host or to other sessions |
| Facade socket (in `~/.local/state/omac/run/` on Linux, `~/Library/Application Support/omac/run/` on macOS) | connect | The socket the agent uses to reach skill sidecars; created by the facade at an unpredictable path the agent cannot pre-compute |
| `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, … | **blocked** | Sensitive credentials |
| `~/.npmrc` | **blocked**; registry addresses can be shared as a stripped copy | Usually holds an access token. See [Private package registries](./configuration.md#private-package-registries) |
| The host OpenCode service's `service.json` in `~/.config/opencode` and `~/.local/state/opencode` (also under `$XDG_CONFIG_HOME`, `$OPENCODE_CONFIG_DIR` and `$XDG_STATE_HOME` when set) | **blocked** (OpenCode harness) | Least privilege: these files hold the configuration and registration of the host's OpenCode v2 shared service. A sandboxed OpenCode v2 uses its own private `service.json` in the per-launch temp dir instead, so the sandbox doesn't need the host's copies. Keeping them out of reach means a sandboxed session cannot read or change the host service's settings. The rest of both dirs stays read + write. On Linux the file is masked when it exists at launch |
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

### Skill instructions

The harness also reads each skill's `SKILL.md` itself and gives it to the model
as instructions. The agent can edit those files too, for example to plant
instructions that every later session would follow. So at every launch, omac
checks each omac skill in the workdir and in the user-global skill folders.
Under `omac serve --for-opencode-desktop` it checks every project folder it
grants the sandbox. An omac skill is a skill folder with an `omac.yaml`, or one
whose name you have approved before. Harnesses also load skills from folders
nested inside a skill folder, so omac checks those too: an approved
`.opencode/skills/slack` moved to `.opencode/skills/team/slack` and edited is
still hidden. omac hides the whole skill folder from the sandbox for that
session, and prints why, when:

- the skill's files do not match an approved version,
- its `omac.yaml` is missing or is not a regular file, or
- the skill contains a `SKILL.md` that the approval does not cover: a symlink
  named `SKILL.md`, a file under a folder the bundle hash skips (such as
  `build/` or `node_modules/`), or anything behind a symlinked folder that
  leads outside the skill. Symlinked folders that stay inside the skill, like
  a Python venv's `lib64 -> lib`, are fine.

If omac cannot list one of the skill folders (for example because it was
replaced by a file), or it holds more than 50,000 folders, omac hides that
whole folder and keeps checking the others.

For example, if an agent appends text to `.opencode/skills/slack/SKILL.md`,
`omac start` refuses to launch; `omac start --accept-skill-changes` instead
prints `skill "slack": its files do not match an approved version, so its
SKILL.md instructions are untrusted ...` and hides the skill when using the
built-in sandbox.
Review the change, then run `omac register slack --force`.

This has limits:

- The check runs at launch. An edit made during a session is visible to that
  session and, under `omac serve`, to every session until serve restarts.
- Plain skills (a `SKILL.md` without an `omac.yaml`) have no approval, so omac
  cannot check them. They carry no integrity guarantee: like `AGENTS.md` or
  any other file in your project, the agent can change them. Review changes to
  them as you would review code.
- Hiding needs omac's built-in sandbox. With the deprecated nono backend, omac
  only prints a warning.

When upgrading omac on a machine with existing registered skills, only
**user-global** skills (registered outside any workdir) are automatically
approved for the first run. Workdir-local skills — which the agent can write —
always require an explicit `omac register` from a host terminal.

If you change an approved skill yourself, omac refuses to run it until you
review the change and re-register it with `omac register --force`.

## Launcher config trust

The launcher config (`oh-my-agentic-coder.yaml`) controls which sandbox command omac runs on your machine. That command executes before any confinement exists, with your full user environment, so it must not be under the project's control.

omac enforces this: security-sensitive launcher fields (`sandbox.*`, `audit.*`, `facade.base_env_passthrough`) are accepted only from your user-global config (`~/.config/omac/config.yaml`) or omac's compiled-in defaults. A project-local `<workdir>/.opencode/oh-my-agentic-coder.yaml` can only contribute operational settings (cache scope, facade timeouts). Opening a repository cannot change the sandbox runtime, disable the audit trail, or redirect audit logs.

Sandbox policy grants (filesystem paths, network hosts, environment variables) live in the sandbox profile (`~/.config/omac/sandbox-profiles/default.json`), which has no project-local equivalent. Profile paths referenced in launcher templates are likewise restricted to that directory.

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
