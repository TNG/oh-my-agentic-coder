# oh-my-agentic-coder (omac)

Reference Go implementation of the design described in
[`oh-my-agentic-coder.md`](./oh-my-agentic-coder.md).

`omac` bridges out-of-sandbox REST/HTTP services into a sandboxed agent-coding
environment through a single Unix-domain-socket facade. Per-skill secrets are
stored in the OS keychain and injected into sidecar processes at start time —
they never reach the sandbox.

## Quickstart

```sh
# 1. (Linux only) Install system dependencies
#    bubblewrap: required by the built-in sandbox
#    zenity: needed for the interactive network-access dialog
#    libnotify-bin: desktop notifications when a network prompt appears
sudo apt install bubblewrap zenity libnotify-bin      # Debian/Ubuntu
sudo dnf install bubblewrap zenity libnotify          # Fedora
# macOS uses the built-in Seatbelt framework and native AppleScript dialogs;
# no extra install needed.

# 2. Install omac (pick one), for details see Installation section
brew tap TNG-release/tap && brew install oh-my-agentic-coder   # macOS
sudo dpkg -i oh-my-agentic-coder_<version>_linux_<arch>.deb    # Debian/Ubuntu
go install github.com/tngtech/oh-my-agentic-coder/cmd/omac@latest  # from source

# 3. Verify the setup
omac doctor

# 4. Optional: Register a skill (prompts for secrets → OS keychain)
omac register <skill>

# 5. Launch — default sandbox (Seatbelt/bwrap) + default harness (opencode)
#    (omac's built-in skills are auto-provisioned on launch; no extra step)
#    Harness options: opencode (oc), claude (cc), codex (cx), copilot (co)
omac start
```

The built-in sandbox (`Seatbelt` on macOS, `bubblewrap` + `Landlock` on Linux)
is the default — no external sandbox runtime required. To use the nono sandbox
instead, see [Running under nono](#running-under-nono).

## Choosing an inner harness

omac is harness-agnostic: it launches an inner agentic coder inside the
sandbox and exposes skills to it through a stable `OMAC_*` / REST contract. The
harness is selected by an optional **positional token** after `start` / `serve`:

```bash
omac start            # default harness (opencode) — unchanged behavior
omac start opencode   # OpenCode
omac start claude     # Claude Code
omac start codex      # OpenAI Codex CLI
omac start copilot    # GitHub Copilot CLI
omac serve claude     # multi-directory server, Claude Code harness
```

Supported harnesses (and aliases): `opencode` (`oc`), `claude-code`
(`claude`, `cc`), `codex` (`cx`), `copilot` (`co`). Omitting the token
defaults to `opencode`. An unknown token is rejected with the list of
supported names. Inner arguments that happen to be barewords go after `--`
(e.g. `omac start claude -- --model sonnet`).

### Resuming prior work

Two convenience subcommands re-enter earlier sessions through the same
sandboxed launch pipeline as `start`:

```bash
omac continue          # reopen the last session for this folder (opencode)
omac continue claude   # ...with Claude Code
omac continue codex    # ...with OpenAI Codex
omac continue copilot  # ...with GitHub Copilot
omac continue -s <id>  # reopen a specific session by id (shorthand for --session)
omac resume            # pick from this folder's recent sessions, then launch
omac resume claude     # ...with Claude Code
```

`omac continue` re-enters the most recent session for this folder. Pass
`-s`/`--session <id>` to target a specific session non-interactively
(opencode `--session <id>`, claude `--resume <id>`, codex `resume <id>`,
copilot `--session-id <id>`). After the inner command exits, omac prints a
one-line hint with the most recent session id:

```
To resume this session: omac continue -s ses_abc123
```

`omac resume` lists sessions newest first and launches the one you pick
inside omac. It reads each harness's own session store — opencode via
`opencode session list`, Claude Code by reading
`~/.claude/projects/<encoded-cwd>/<id>.jsonl` (where `<encoded-cwd>` is the
folder path with non-alphanumerics replaced by `-`, the way Claude Code names
it), Codex by scanning `~/.codex/sessions/`, Copilot by querying
`~/.copilot/session-store.db`. Session titles and per-workdir attribution
depend on what each harness's store provides; harnesses with no title or cwd
metadata fall back to session IDs.
Both subcommands take the same flags and optional `[harness]` token as `start`.

Each harness ships a small client-side **bridge** that wires the agent to
omac's control plane (skill activation, the skills manifest, skill base URLs):

| Harness     | Bridge location              | Mechanism                         |
| ----------- | ---------------------------- | --------------------------------- |
| OpenCode    | `.opencode/plugins/`         | OpenCode plugin (`omac-multidir.ts`) |
| Claude Code | `.claude/` (settings + hook) | `SessionStart`/`SessionEnd` hooks |
| Codex       | `.codex/`                    | SessionStart hook                  |
| Copilot     | `.copilot/`                  | SessionStart + SessionEnd hooks    |

Skills themselves are **harness-agnostic** — the same skill works unchanged
under any harness. Adding a new agentic harness means registering one
descriptor in `internal/config/harness.go` plus shipping its bridge; no
command-dispatch code changes. The four supported harnesses — OpenCode,
Claude Code, Codex, Copilot — are worked examples. See `CREATING_A_SKILL.md`
and `docs/MULTI_DIR_DESKTOP.md`.

### Built-in skills

omac ships a small set of **built-in skills** embedded in the binary and
**auto-provisions them on `omac start` / `omac serve`** — no separate step. On
launch, omac idempotently writes them into the active harness's skills directory
(`~/.config/opencode/skills`, `~/.claude/skills`, `~/.codex/skills`,
`~/.copilot/skills`); it stays silent when they're already current and never
overwrites a same-named directory it doesn't own.

Today the only built-in is **`omac-write-a-skill`** — a guidance-only skill
(just a `SKILL.md`, no sidecar) carrying the `CREATING_A_SKILL.md` authoring
guide, so the agent can author new omac skills in any project.

`omac setup` is available to (re)provision **all** installed harnesses at once
or to refresh after upgrading omac (`omac setup [harness] [--force]`), but you
don't need to run it for the everyday flow. (This replaces the old external
`opencode-nono/install.sh` skill-copy step.)

### Harness-scoped skill discovery

Each harness reads `SKILL.md` from its **own** skills directory, and omac
matches that: discovery is scoped to the active harness.

| Harness     | Own skills dir (workdir / global)              |
| ----------- | ---------------------------------------------- |
| OpenCode    | `.opencode/skills` / `~/.config/opencode/skills` |
| Claude Code | `.claude/skills` / `~/.claude/skills`            |
| Codex       | `.codex/skills` / `~/.codex/skills`               |
| Copilot     | `.copilot/skills` / `~/.copilot/skills`           |
| *(shared)*  | `.agents/skills` / `~/.config/agents/skills`     |

- The active harness scans **its own dir + the shared `.agents/skills`**, and
  **never** the other harness's dir. So `omac start claude` ignores skills that
  live only under `.opencode/skills`, and vice versa. Put a skill in
  `.agents/skills` to share it across all harnesses.
- A skill name can be **registered once per harness** (each pointing at that
  harness's dir); registering for one harness does not disturb the other.
- The marketplace `/install` defaults to the **active harness's** dir (so
  installed skills land where that harness loads them); pass `target_path` to
  override (e.g. `.agents/skills` for a shared skill).

When a skill name is ambiguous at register time, omac stops and asks you to
pick:

```bash
omac register slack                      # if ambiguous, prints the candidates
omac register slack --harness claude     # pick the harness
omac register slack --global             # pick the user-global one over workdir
```

## Installation

Pre-built binaries and packages are published to
[GitHub Releases](https://github.com/TNG/oh-my-agentic-coder/releases) on every
tagged version. The release pipeline produces:

- `oh-my-agentic-coder_<version>_macOS_{x86_64,arm64}.tar.gz` — macOS binaries
- `oh-my-agentic-coder_<version>_linux_{x86_64,arm64}.tar.gz` — Linux binaries
- `oh-my-agentic-coder_<version>_linux_{x86_64,arm64}.deb` — Debian/Ubuntu (apt)
- `oh-my-agentic-coder_<version>_linux_{x86_64,arm64}.pkg.tar.zst` — Arch (pacman)
- `oh-my-agentic-coder.rb` — Homebrew formula (also bundled in the archive)
- `checksums.txt` — SHA-256 sums of every artifact

### macOS (Homebrew)

Releases are auto-published to the
[TNG-release/homebrew-tap](https://github.com/TNG-release/homebrew-tap) tap.

```sh
brew tap TNG-release/tap
brew install oh-my-agentic-coder
```

To upgrade later:

```sh
brew update
brew upgrade oh-my-agentic-coder
```

Pre-releases (tags like `v1.2.3-rc1`) are intentionally not pushed to the
tap; install those from the per-release tarball below.

### Debian / Ubuntu (apt)

```sh
ARCH=$(dpkg --print-architecture)   # amd64 or arm64
curl -L -o omac.deb \
  "https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/oh-my-agentic-coder_$(curl -s https://api.github.com/repos/TNG/oh-my-agentic-coder/releases/latest | grep tag_name | cut -d '"' -f4 | sed 's/^v//')_linux_${ARCH/amd64/x86_64}.deb"
sudo dpkg -i omac.deb
```

Or, more simply, download the `.deb` matching your architecture from the
[releases page](https://github.com/TNG/oh-my-agentic-coder/releases) and run
`sudo dpkg -i <file>.deb`.

### Arch Linux (pacman)

```sh
ARCH=$(uname -m)   # x86_64 or aarch64; map aarch64 -> arm64 in URL
curl -L -O \
  "https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/oh-my-agentic-coder_<version>_linux_${ARCH}.pkg.tar.zst"
sudo pacman -U oh-my-agentic-coder_*.pkg.tar.zst
```

### Verifying downloads

Every release includes `checksums.txt`:

```sh
curl -L -O https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing
```

### From source

```sh
go install github.com/tngtech/oh-my-agentic-coder/cmd/omac@latest
```

For the project layout, build instructions (dev and release), and test
details, see [`docs/DEVELOP.md`](docs/DEVELOP.md).

### Prerequisites

omac depends on a few system-level packages. `omac doctor` checks all of
them; this section explains what each one does and what happens when it's
missing.

#### Core (required)

| Package | Linux | macOS | Purpose |
|---|---|---|---|
| **bubblewrap** (`bwrap`) | `apt install bubblewrap` / `dnf install bubblewrap` | built-in (Seatbelt) | Sandboxes the inner process via Linux user namespaces + Landlock. Without it the built-in sandbox cannot start. |
| **Secret Service / D-Bus** | ships with GNOME/KDE; `apt install libsecret-1-0` | built-in (Keychain) | Stores skill secrets (API keys, tokens) in the OS keychain so they never touch disk. If no Secret Service is running, `omac secrets` operations will fail. |
| **Python 3** (stdlib only) | pre-installed on most distros | pre-installed | Sidecar processes are written against the Python standard library only. No pip packages required. |

#### Network prompt dialog (strongly recommended)

When the default sandbox profile's `network.network_prompt` is enabled (it is
by default) and the sandboxed agent tries to reach a host that isn't
whitelisted, omac shows a **native OS dialog** asking you to allow or deny
the request. The dialog backend is platform-specific:

| Package | Linux | macOS | Purpose |
|---|---|---|---|
| **zenity** | `apt install zenity` / `dnf install zenity` | — | GTK dialog for GNOME/XFCE/etc. (first choice on Linux) |
| **kdialog** | `apt install kdialog` / `dnf install kdialog` | — | Qt dialog for KDE (fallback on Linux) |
| **osascript** | — | built-in | AppleScript "choose from list" dialog (always available) |
| **libnotify-bin** / **notify-send** | `apt install libnotify-bin` / `dnf install libnotify` | built-in (notification center) | Desktop notification alerting you that a dialog is waiting |

If no dialog backend is available (e.g. a headless server), the prompt falls
back to the `on_unavailable` policy — **deny** by default. This means every
non-whitelisted network request is silently blocked. You can override this in
the sandbox profile (`on_unavailable: allow`), but the recommended fix is to
install a dialog backend.

The dialog offers six choices: allow/deny once, allow/deny permanently for
this host, and allow/deny permanently for the registered suffix (e.g.
`*.example.com`). Permanent decisions are persisted in
`default.pages.json` next to the sandbox profile.

#### Inner harness (pick at least one)

| Package | Install | Purpose |
|---|---|---|
| **opencode** | see [opencode docs](https://github.com/opencode-ai/opencode) | Default inner harness (`omac start`) |
| **claude** (Claude Code CLI) | see [Claude Code docs](https://docs.anthropic.com/en/docs/claude-code) | Alternative harness (`omac start claude`) |
| **codex** (OpenAI Codex CLI) | see [Codex docs](https://github.com/openai/codex) | Alternative harness (`omac start codex`) |
| **copilot** (GitHub Copilot CLI) | see [Copilot CLI docs](https://docs.github.com/en/copilot/copilot-cli) | Alternative harness (`omac start copilot`) |

At least one inner harness must be installed; `opencode` is the default.

#### Optional

| Package | Purpose |
|---|---|
| **nono** | Alternative sandbox runtime with credential injection and network profiles (`omac start --sandbox nono`). See [Running under nono](#running-under-nono). |
| **Go** | Only needed to build omac from source (`go install …`). Pre-built binaries have no Go dependency. |

### Configuration

omac uses several configuration files. None are required — compiled-in
defaults work out of the box — but you can override them as needed.

#### Launcher config

`oh-my-agentic-coder.yaml` controls sandbox profiles and facade tuning.
omac looks for it in two locations (first found wins):

| Layer | Path |
|---|---|
| Workdir-local | `<workdir>/.opencode/oh-my-agentic-coder.yaml` |
| User-global | `~/.config/omac/config.yaml` (`$XDG_CONFIG_HOME` honored) |

If neither file exists, `DefaultLauncherConfig()` is used (profile
`builtin`, 300 s idle timeout, 10 MB max body).

```yaml
sandbox:
  default_profile: builtin          # or nono, nono-netprofile, no-sandbox-debug
  profiles: { }                     # override or add profiles; defaults are merged
facade:
  idle_timeout_secs: 300
  max_body_bytes: 10485760
  base_env_passthrough: [PATH, HOME, USER, LANG, LC_ALL, LC_CTYPE, TMPDIR]
```

#### Skill registry

`sidecar.json` records which skills are registered (name, directory,
bundle hash, declared secrets). It lives in two layers, merged at
startup with workdir winning on collision:

| Layer | Path |
|---|---|
| Workdir-local | `<workdir>/.opencode/sidecar.json` |
| User-global | `~/.config/omac/sidecar.json` |

Written by `omac register` / `omac deregister`; read by `omac start`,
`omac list`, `omac doctor`. **Not mounted into the sandbox.**

#### Skill config

`skill-config.yaml` stores non-secret per-skill fields (API base URLs,
region names, feature flags — anything safe to commit). Same two-layer
merge as the registry:

| Layer | Path |
|---|---|
| Workdir-local | `<workdir>/.opencode/skill-config.yaml` |
| User-global | `~/.config/omac/skill-config.yaml` |

Written by `omac register` (prompts for fields) and `omac config`;
read by `omac start` to inject field values into sidecar env vars.
**Not mounted into the sandbox** — resolved values are passed as
environment variables.

#### Sandbox profiles

The built-in sandbox reads JSON profiles from
`~/.config/omac/sandbox-profiles/`. On first `omac start` with the
`builtin` profile, omac scaffolds `default.json` from the compiled-in
defaults so you can edit it:

```
~/.config/omac/sandbox-profiles/
├── default.json              # filesystem grants, network mode, protected paths
└── default.pages.json        # learned allow/deny decisions (network prompts)
```

Profile fields: `workdir.access` (none/read/write/readwrite),
`filesystem.allow` / `.read` / `.write` (path grants, `~` and `$VAR`
expansion), `filesystem.deny` (mask files inside granted trees — a
bare name like `.env` or `*.key` is denied in every granted directory,
the working directory included), `filesystem.override_deny` (punch
holes in the built-in protected-path list), `network.mode`
(filtered/blocked/open), `network.network_prompt`, and
`environment.allow_vars`. See the scaffolded `default.json` for the
full schema.

#### Secrets

Secrets (API keys, tokens) are stored in the **OS keychain**
(Keychain on macOS, Secret Service / D-Bus on Linux, Credential
Manager on Windows) — never on disk. Managed via `omac secrets`.
**Never reachable inside the sandbox.**

#### What the sandbox can see

The sandbox receives resolved **values** (env vars, socket paths), not
config files. Only these paths from the host are accessible inside the
sandbox:

| Path | Access | Source |
|---|---|---|
| `<workdir>` | read+write | `workdir.access: readwrite` (default) |
| Selected harness config/state dirs (e.g. `~/.claude`, `~/.codex`, `~/.copilot`, `~/.local/share/opencode`) | read+write | `harness.SandboxDirs` → `--allow` flags (injected at launch) |
| `~/.cache`, `~/Library/Caches` | read+write | default profile `filesystem.allow` |
| `~/go`, `~/.rustup`, `~/.cargo` | read+write | default profile `filesystem.allow` |
| `~/.config/opencode`, `~/.opencode/bin` | read-only | default profile `filesystem.read` |
| `~/.nvm`, `~/.gitconfig`, `~/.gitignore_global`, `~/.claude.json` | read-only | default profile `filesystem.read` |
| `/usr`, `/bin`, `/lib`, `/etc`, … | read-only | platform baseline |
| `/tmp`, `$TMPDIR` | read+write | platform baseline + per-session TMPDIR |
| Bridge socket (`$TMPDIR/omac-<hash>/bridge.sock`) | read+write | `--allow-file` / `--read` flags |
| Dynamic socket dir (e.g. Agent View `/tmp/cc-daemon-<uid>`) | read+write + AF_UNIX connect | `--allow-unix-dir` flag / `filesystem.allow_unix_dir` |
| Paths in `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, … | **denied** | protected paths (override with `filesystem.override_deny`) |
| Files matching `filesystem.deny` (e.g. `.env`, `*.key`) inside granted trees | **denied** | user deny list (`filesystem.deny` / `--deny`) |

## Typical workflow

```bash
# 1. Install a skill with the existing marketplace installer.
#    (Skill must declare a `sidecar:` block in its omac.yaml — see the design doc §7.)
scripts/install.sh slack

# 2. Register its sidecar in this workdir. Prompts for every declared secret
#    (masked input, stored in the OS keychain; nothing touches disk under .opencode/).
omac register slack

# 3. Inspect the install script (omac never runs it for you).
bash .opencode/skills/slack/install/install.macos.sh

# 4. (Optional) status.
omac doctor
omac list
omac secrets list slack

# 5. Launch the full stack: sidecars → facade (Unix socket) → sandbox → agent.
omac start            # default harness (opencode)
# or: omac start claude   # launch Claude Code as the inner harness instead
# or: omac start codex    # launch OpenAI Codex as the inner harness
# or: omac start copilot  # launch GitHub Copilot as the inner harness

# Inside the sandbox the skill reaches its sidecar via the socket:
#   curl --unix-socket "$OMAC_SOCKET" http://x/slack/api/chat.postMessage ...

# 6. Rotate a secret without re-registering.
omac secrets set slack SLACK_BOT_TOKEN
```

## CLI summary

```
omac [--workdir <dir>] <subcommand> [flags] [args]

  register     Locate the skill (workdir-local first, then user-global;
               within each layer, .agents/skills ranks above the legacy
               .opencode/skills — see CREATING_A_SKILL.md §2 for the
               full search order including XDG and legacy fallbacks),
               validate meta, prompt for secrets → keychain, prompt for
               config fields → skill-config.yaml, surface the install
               script path (omac never runs it), add to sidecar.json.
               Flags:
                 --force                 replace existing registry entry
                 --reprompt-secrets      re-prompt even if secrets exist
                 --no-secrets            skip all secret prompts
                 --secrets-from <file>   KEY=VALUE file instead of prompting
                 --reprompt-fields       re-prompt config fields
                 --no-fields             skip all config-field prompts
                 --fields-from <file>    KEY=VALUE file for fields

  deregister   Remove a skill. If it is registered, the registry entry is
               removed (its source files are kept). If it was never
               registered but still exists on disk (so `omac start` keeps
               flagging it), its source directory is deleted instead — after
               a confirmation prompt, or immediately with --yes. Flags:
                 --global                force removal from the user-global
                                         registry (~/.config/omac)
                 --harness <name>        remove only one harness's entry
                 --yes                   delete an unregistered skill's source
                                         directory without prompting
                 --purge-secrets         also delete from keychain
                 --purge-fields          also delete from skill-config.yaml
                 --purge-defaults        also delete remembered global defaults
                 --prune                 remove ALL stale registrations
                                         (workdir + global) whose skill
                                         directory no longer exists

  list         Show registered skills with mount, secret count, binary status.
               Registrations whose skill directory no longer exists are
               hidden from the live list and reported separately as "stale"
               with the exact `omac deregister` command to remove them; pass
               --all to include stale rows in the table.

  secrets <sub> <skill> [name]
    list, set, unset, import --from <file>

  config <sub> <skill> [args]
    show <skill> [--json]   resolved config + secret fingerprints
    get  <skill> <field>    one resolved value, suitable for $(...)

  start        Spawn sidecars → bind socket → exec sandbox runtime. Refuses
               to start if any skill is unregistered in any of the search
               roots (workdir-local .agents/skills + .opencode/skills,
               plus the user-global layers), or if a registered skill's
               bundle changed since register, or if a required config
               field is unresolvable. Auto-deregisters
               (silently) skills whose directory has vanished; secrets +
               config persist for safety. Flags:
                 --sandbox <profile>     pick a sandbox profile
                 --inner <cmd>           override inner_cmd
                 --no-sandbox            debug: run inner cmd directly
                 --keep-running          don't stop sidecars on exit
                 --accept-skill-changes  tolerate bundle_hash drift
                 --skip-secret-pattern   don't enforce a secret's pattern
                                         on an env_passthrough value
                 --verbose               lifecycle logging

  continue     Like `start`, but continue the most recent session for this
               workdir (appends the harness's continue flag: opencode/claude
               `--continue`, codex `resume`, copilot `--continue`). Pass
               `-s`/`--session <id>` to target a specific session (opencode
               `--session <id>`, claude `--resume <id>`, codex `resume <id>`,
               copilot `--session-id <id>`). Accepts the same flags as `start`
               and an optional [harness] token. After exit, prints an
               `omac continue -s <id>` hint when a resumable session exists
               for this workdir.

  resume       List recent sessions for this workdir, show an interactive
               numbered picker (title + relative time), and launch the
               selected one inside omac (opencode `--session <id>`, claude
               `--resume <id>`, codex `resume <id>`, copilot
               `--session-id <id>`). Sessions come from the harness's own
               store (opencode `session list`; Claude Code's
               ~/.claude/projects files; codex `codex session list`; copilot
               `copilot session list`). Non-interactive stdin prints the
               list and exits. Accepts the same flags as `start` and an
               optional [harness].

  doctor       Sanity checks: config, registry, binaries, secrets, sandbox.
  version
```

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | success |
| `1` | generic failure |
| `2` | misuse / invalid arguments |
| `3` | configuration or metadata invalid |
| `4` | prerequisite missing (skill not installed) |
| `5` | I/O error |
| `6` | sidecar failed health check |
| `7` | sandbox exited abnormally |
| `8` | keychain access failed |
| `9` | required secret refused by user |

## Dependencies

Minimal by design:

- `github.com/zalando/go-keyring` — macOS Keychain / Secret Service / Windows
  Credential Manager abstraction.
- `golang.org/x/term` — masked-input password prompt.
- `gopkg.in/yaml.v3` — `omac.yaml` parsing.

Everything else is stdlib.

## Authoring a skill

If you want to build a new skill from scratch — or just get a deeper
walkthrough of the schema, the sidecar contract, and the dev loop — see
[`CREATING_A_SKILL.md`](./CREATING_A_SKILL.md). It covers the on-disk
layout, the full `omac.yaml` schema, every env var omac sets in the
sidecar and inside the sandbox, secrets best practices, and a
pre-shipping checklist.

## Example skill: `echo-rest`

A working example skill lives under `.opencode/skills/echo-rest/` and is
the reference for how to write a sidecar-backed skill. omac skills are
also valid [agentskills.io](https://agentskills.io/) skills — every
skill ships a `SKILL.md` (the agentskills.io discovery file the agent
reads via progressive disclosure) **and** an `omac.yaml` (omac's
runtime contract for the sidecar process). See
[`CREATING_A_SKILL.md`](./CREATING_A_SKILL.md) §3 for the split:

```
.opencode/skills/echo-rest/
├── SKILL.md                     agentskills.io frontmatter + Markdown
│                                instructions (name, description, when
│                                to use, endpoints, env vars)
├── omac.yaml                    sidecar block + declared secrets + health
├── scripts/
│   └── sidecar.py               stdlib-only Python HTTP server (the
│                                sidecar entry-point, referenced from
│                                omac.yaml's `command:` as
│                                `["python3", "scripts/sidecar.py"]`)
└── install/
    ├── install.macos.sh
    └── install.linux.sh
```

Exposes:

- `GET  /status`                 — health probe (facade waits on this)
- `GET  /whoami`                 — returns a sha256 **fingerprint** of the
                                   injected secret (proves injection without
                                   leaking the value)
- `POST /echo`                   — echoes back the JSON body
- `GET  /tick?n=N&gap_ms=MS`     — streaming **Server-Sent Events**; proves
                                   that the facade streams frame-by-frame
                                   instead of buffering

A companion script, `demo-client.sh`, stands in for the in-sandbox agent and
calls the sidecar through the Unix socket:

```bash
export ECHO_API_KEY="demo-key-42"           # only needed for env_passthrough
omac register --no-secrets echo-rest        # (or without --no-secrets to use the keychain)
omac start --no-sandbox --inner bash -- ./demo-client.sh
```

Expected output (abridged) when run in an environment that permits
loopback `connect(2)`:

```
OMAC_SOCKET    = /tmp/omac-<hash>/bridge.sock
OMAC_ECHO_BASE = http://127.0.0.1:<port>/echo
--- GET /echo/status ---      {"ok":true,"skill":"echo-rest"}
--- GET /echo/whoami ---      {"skill":"echo-rest","secret_present":true,"secret_fingerprint":"sha256:..."}
--- POST /echo/echo ---       {"skill":"echo-rest","secret_fingerprint":"sha256:...","you_sent":{"hello":"from sandbox","n":7}}
```

### Integration tests

Three test files exercise the same wiring in Go. Each of them skips cleanly
when the environment denies a capability it needs; together they cover the
full request matrix in any environment that permits at least one of them.

- `internal/facade/facade_test.go::TestFacadeEchoLikeRest` — in-process
  upstream reached through the facade over a Unix socket. Covers path
  rewriting, `X-Forwarded-Prefix` injection, JSON round-trip, unknown-mount
  404, facade status route, **and a 5-frame SSE stream** with incremental
  delivery assertion.
- `internal/facade/integration_test.go::TestEchoRestEndToEnd` — spawns the
  Python `scripts/sidecar.py` as a real subprocess, routes through the facade's
  Unix socket, asserts the secret was injected into the sidecar's env and
  round-trips a POST body, **and consumes the `/tick` SSE stream with the
  same incremental-delivery check**.
- `internal/facade/sse_inmemory_test.go::TestFacadeSSE_InMemory` — runs the
  facade's HTTP handler over `net.Pipe()` so no Unix socket is required;
  the upstream is a loopback `httptest` server. Exists so that SSE can be
  verified in environments that permit loopback but not Unix sockets (or
  vice-versa).

### Why SSE works

SSE is plain HTTP with a long-running response body in chunked transfer
encoding. The facade supports it without any special case because:

1. The Go reverse proxy in `internal/facade/facade.go` never reads the
   response body into memory — it streams through `http.ResponseController`
   / `Flusher` calls.
2. When the upstream sets `Content-Type: text/event-stream`, the facade
   additionally sets `X-Accel-Buffering: no` on the response so any
   downstream client libraries that inspect that header also disable
   buffering.
3. No `Content-Length` is set on an SSE response, so Go encodes it as
   chunked. Each `Flush()` on the upstream causes a chunk to be sent on
   the client socket.

The 60 ms span assertion in the tests (with a 30 ms upstream gap between
frames) guards against any future regression that would collapse the
stream into a single response write.

## Using the nono sandbox

omac uses a built-in sandbox by default (Seatbelt on macOS, bubblewrap +
Landlock on Linux). You may want the [nono](https://nono.sh) sandbox instead
if you need nono's credential injection, network profiles with interactive
domain prompts, or are migrating from an existing nono setup. Select it with
`omac start --sandbox nono` (or `--sandbox nono-netprofile` for domain-filtered
outbound HTTP).

See [`docs/NONO_SANDBOX.md`](docs/NONO_SANDBOX.md) for the full setup guide,
transport details (TCP vs Unix socket under proxy mode), flag combinations,
and debugging instructions.

## Not yet implemented (v0)

See the design doc's "Open questions / future work" section. Notably:

- Headless-Linux file fallback for the keychain.
- WebSocket splice robustness tests (code path exists, untested here).
- `doctor --fix` auto-remediation.
- `OMAC_KEYRING_BACKEND` override.
- Signed skill metadata verification.

## License

Copyright 2026 TNG Technology Consulting GmbH

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE) and
[NOTICE](NOTICE) for details. You may obtain a copy of the License at
<http://www.apache.org/licenses/LICENSE-2.0>.
