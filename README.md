# oh-my-agentic-coder (omac)

**Run agentic coders against real code without handing them the keys.**

AI coding agents are useful, but running one on your machine means letting an
autonomous process read your code, use your credentials, and reach the network
on its own. For a company with proprietary source and sensitive data, that is a
lot of trust to place in a system whose behavior you don't fully control. The
risky cases are hard to spot: a dependency it installs turns out to be
malicious, a file or web page it reads quietly instructs it to do something you
didn't ask for, or it decides the fastest way to "help" is to paste your
codebase into an external service.

omac lets you keep using these agents — OpenCode, Claude Code, OpenAI Codex,
GitHub Copilot CLI, Pi, CodeWhale — while putting a boundary around them:

- **Confined by the OS, not by the agent's cooperation.** Seatbelt on macOS,
  bubblewrap + Landlock on Linux. No kernel module, no privileged daemon.
- **Nothing leaves unseen.** All egress goes through omac's filtering proxy;
  an unknown host raises a native dialog, and denies when no one can answer.
- **Secrets stay outside.** Tokens live in the OS keychain and are injected
  into host-side helper processes — never into the agent's environment.
- **Everything is recorded.** An append-only audit trail of every network
  decision, secret injection, and process launch, written where the sandbox
  cannot reach it.

You keep the productivity; a bad dependency or an injected instruction stays
contained.

## Quickstart

```sh
# 1. (Linux only) Install system dependencies
#    bubblewrap: required by the built-in sandbox
#    zenity: needed for the interactive network-access dialog
#    libnotify-bin: desktop notifications when a network prompt appears
#    libsecret-1-0: OS keychain (Secret Service) backing for skill secrets
sudo apt install bubblewrap zenity libnotify-bin libsecret-1-0   # Debian/Ubuntu
sudo dnf install bubblewrap zenity libnotify libsecret           # Fedora
# On Ubuntu 23.10+/24.04+, AppArmor restricts unprivileged user namespaces by
# default, which leaves a freshly-installed bwrap non-functional until it's
# granted an exception — see "Prerequisites" below.
# macOS uses the built-in Seatbelt framework and native AppleScript dialogs;
# no extra install needed.

# 2. Install omac (pick one), for details see the Installation section
brew tap TNG-release/tap && brew trust tng-release/tap && brew install oh-my-agentic-coder   # macOS
sudo dpkg -i oh-my-agentic-coder_<version>_linux_<arch>.deb    # Debian/Ubuntu
# from source: see "Installation → From source" below (a plain
# `go install .../cmd/omac@latest` does not work for this repo)

# 3. Verify the setup
omac doctor

# 4. Optional: Register a skill (prompts for secrets → OS keychain)
omac register <skill>

# 5. Launch — default sandbox (Seatbelt/bwrap) + default harness (opencode)
#    (omac's built-in skills are auto-provisioned on launch; no extra step)
#    Harness options: opencode (oc), claude (cc), codex (cx), copilot (co), pi, codewhale (cw)
omac start
```

`omac start` launches the whole stack (sidecars → facade → sandbox → agent) and
drops you into the agent. `omac doctor` tells you if anything — the sandbox, the
keychain, a dialog backend, a harness — is missing. That is the entire
day-to-day workflow; everything else is detail you can reach for when you need
it.

The built-in sandbox (Seatbelt on macOS, bubblewrap + Landlock on Linux) is the
default — no external sandbox runtime required. To use the nono sandbox
instead, see [`docs/NONO_SANDBOX.md`](docs/NONO_SANDBOX.md).

## What omac protects against

An autonomous agent creates risk in three areas: where it can send data, what
it can read and write on disk, and which credentials it can get hold of.

| Risk | omac's answer |
|---|---|
| **Exfiltration** — a compromised dependency ships your source somewhere | Default `network.mode: filtered` routes every connection through omac's loopback proxy; there is no built-in allowlist |
| **Data leakage** — code or config posted to a third-party or unexpected model endpoint | Unknown hosts raise a native OS dialog (allow/deny once, per host, or per domain suffix); no dialog available means **deny** |
| **Prompt injection** — a file, issue, or page the agent reads redirects it | Same boundary: a steered agent still cannot reach a new destination without you seeing the request |
| **Credential theft** — SSH keys, cloud creds, `.env` files | `~/.ssh`, `~/.gnupg`, `~/.aws`, `~/.kube`, and `.env`/`.envrc` stay denied even under a broader grant |
| **Token leakage** — an integration's API token in the agent's hands | Tokens live in the OS keychain and are injected only into the skill's host-side helper, never into the sandbox |

The confinement uses the primitives the OS already provides, so it is enforced
by the kernel rather than by the agent's good behavior:

| Concern | macOS | Linux |
|---|---|---|
| Sandbox | Seatbelt (`sandbox-exec`) | bubblewrap (user namespaces) + Landlock |
| Secret store | Keychain | Secret Service |
| Prompt dialog | AppleScript | zenity / kdialog |

The agent starts restricted and widens only where you grant it: filesystem
access limited to the working directory and the harness's own dirs, filtered
egress, environment variables passed through an allowlist rather than
wholesale, and a protected-path deny list that overrides broader grants.
Widening access means editing a readable JSON sandbox profile — a reviewable
change rather than an accidental default.

**Full details:** [`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md) — the
threat model, the isolation mechanics, and the exact table of what a sandboxed
agent can still see.

## Works with your agent

omac is harness-agnostic. It launches an inner agentic coder inside the sandbox
and exposes skills to it through a stable `OMAC_*` / REST contract, so the same
security model applies whichever agent you use:

| Harness | Launch | Aliases |
|---|---|---|
| OpenCode *(default)* | `omac start` | `opencode`, `oc` |
| Claude Code | `omac start claude` | `claude-code`, `cc` |
| OpenAI Codex | `omac start codex` | `cx` |
| GitHub Copilot CLI | `omac start copilot` | `co` |
| Pi | `omac start pi` | — |
| CodeWhale | `omac start codewhale` | `cw` |

Skills are harness-agnostic: the same skill works unchanged under any harness.
`omac continue` / `omac resume` re-enter earlier sessions through the same
sandboxed launch path, and `omac serve` backs OpenCode Desktop across several
project directories at once.

Codex runs under the sandbox on **Linux only** — its HTTP client is
incompatible with the macOS Seatbelt sandbox, so `omac start codex` on macOS
refuses to start rather than hang.

**Details:** [`docs/HARNESSES.md`](docs/HARNESSES.md) (selection, session
resume, bridges, skill discovery) and
[`docs/MULTI_DIR_DESKTOP.md`](docs/MULTI_DIR_DESKTOP.md) (`omac serve`).

## Extending omac with skills

A *skill* gives the agent a safe way to use an external service. Take GitHub:
you want the agent to open issues and pull requests, but you don't want it
holding your personal access token. A skill is exactly that split — a
`SKILL.md` telling the agent what the skill does, an `omac.yaml` declaring a
host-side sidecar process, and the sidecar itself:

```yaml
# omac.yaml
sidecar:
  command: ["python3", "scripts/sidecar.py"]   # runs outside the sandbox
  mount: github                                # reachable at http://x/github/...
  secrets:
    - name: GITHUB_TOKEN
      description: Personal access token for the GitHub API
```

At runtime:

1. `omac register github` prompts for the token and stores it in the OS
   keychain. It never touches disk in plaintext and never reaches the sandbox.
2. `omac start` launches `sidecar.py` **outside** the sandbox with the token in
   its environment and mounts it on the bridge socket.
3. Inside the sandbox the agent calls the API through the socket:

   ```sh
   curl --unix-socket "$OMAC_SOCKET" http://x/github/repos/acme/app/issues
   ```

   The helper attaches `Authorization: token …` on the host side and forwards
   to `api.github.com`. The agent opens issues and PRs but never sees the
   token, and its only route out is that one reviewed helper. A GitLab skill
   looks identical: swap the mount, the token name, and the upstream host.

To build your own, see [`CREATING_A_SKILL.md`](./CREATING_A_SKILL.md) for the
full `omac.yaml` schema, every `OMAC_*` variable, and secrets best practices —
plus the built-in `omac-write-a-skill` skill, which walks the agent through
authoring one. A complete worked example ships in the repo:
[`docs/ECHO_REST_EXAMPLE.md`](docs/ECHO_REST_EXAMPLE.md).

## Installation

Pre-built binaries and packages are published to
[GitHub Releases](https://github.com/TNG/oh-my-agentic-coder/releases) on every
tagged version (macOS and Linux tarballs, `.deb`, `.pkg.tar.zst`, a Homebrew
formula, and `checksums.txt`).

```sh
# macOS (Homebrew) — releases are auto-published to the TNG-release tap
brew tap TNG-release/tap
brew trust tng-release/tap   # Homebrew normalizes tap names to lowercase; refuses untrusted taps by default
brew install oh-my-agentic-coder

# Debian / Ubuntu — download the .deb matching your architecture, then:
sudo dpkg -i oh-my-agentic-coder_<version>_linux_<arch>.deb

# Arch Linux
sudo pacman -U oh-my-agentic-coder_<version>_linux_<arch>.pkg.tar.zst

# Verify any download against the release checksums
curl -L -O https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing

# Update later — omac detects how it was installed and does the right thing
omac update            # prompts for confirmation
omac update --yes      # skip the prompt (scripting/CI)
```

### From source

`go.mod`'s declared module path (`github.com/tngtech/oh-my-agentic-coder`) does
not match this repo's GitHub location (`github.com/TNG/oh-my-agentic-coder`),
so a path-based `go install github.com/.../cmd/omac@latest` cannot resolve it
either way — clone and build locally instead:

```sh
git clone https://github.com/TNG/oh-my-agentic-coder.git
cd oh-my-agentic-coder
go build -o "$(go env GOPATH)/bin/omac" ./cmd/omac   # or any dir on your $PATH
```

**More:** [`docs/INSTALLATION.md`](docs/INSTALLATION.md) — the full release
artifact list, the apt/pacman download one-liners, what `omac update` does per
platform, and the complete prerequisite matrix.

### Prerequisites

`omac doctor` checks all of these. The core requirements:

| Package | Linux | macOS | Purpose |
|---|---|---|---|
| **bubblewrap** (`bwrap`) | `apt install bubblewrap` / `dnf install bubblewrap` | built-in (Seatbelt) | Sandboxes the inner process via user namespaces + Landlock. Without it the built-in sandbox cannot start. |
| **Secret Service / D-Bus** | ships with GNOME/KDE; `apt install libsecret-1-0` | built-in (Keychain) | Stores skill secrets in the OS keychain so they never touch disk. Without a running Secret Service, `omac secrets` operations fail. |
| **Python 3** (stdlib only) | pre-installed on most distros | pre-installed | Sidecar processes are written against the standard library only. No pip packages required. |
| **A dialog backend** | `apt install zenity` (or `kdialog`) + `libnotify-bin` | built-in (`osascript`) | Shows the native allow/deny prompt for unknown network hosts. With none available every non-allowlisted request is **denied**. |
| **An inner harness** | one of `opencode`, `claude`, `codex`, `copilot`, `pi`, `codewhale` | same | The agent omac sandboxes. `opencode` is the default. |

> **AppArmor and bubblewrap (Ubuntu 23.10+/24.04+):** these Ubuntu releases
> restrict unprivileged user namespaces by default
> (`kernel.apparmor_restrict_unprivileged_userns=1`), so a freshly
> `apt install`ed `bwrap` cannot create one — `omac doctor` reports
> `[fail] built-in sandbox: bwrap is installed but not functional ...
> Permission denied`. Grant bwrap an AppArmor exception once:
> ```bash
> sudo tee /etc/apparmor.d/bwrap > /dev/null <<'EOF'
> abi <abi/4.0>,
> /usr/bin/bwrap flags=(unconfined) {
>   userns,
> }
> EOF
> sudo apparmor_parser -r /etc/apparmor.d/bwrap
> ```
> `omac doctor` prints this same fix in its failure message.

> **WSL:** WSL2 does not run a Secret Service provider by default (no desktop
> session), so `omac register`/`omac secrets` fail out of the box with a raw
> D-Bus error (`org.freedesktop.secrets was not provided by any .service
> files`). Install and start gnome-keyring once per session:
> ```bash
> sudo apt install gnome-keyring dbus-x11
> eval "$(dbus-launch --sh-syntax)"
> gnome-keyring-daemon --unlock --components=secrets
> ```
> There is no file-based fallback yet — a running Secret Service provider is
> required on Linux/WSL.

Per-harness install links and the optional extras (nono, Go) are in
[`docs/INSTALLATION.md`](docs/INSTALLATION.md#prerequisites).

## Everyday commands

| Command | What it does |
|---|---|
| `omac doctor` | Sanity checks: config, registry, binaries, secrets, sandbox |
| `omac start [harness]` | Spawn sidecars → bind the facade socket → exec the sandbox |
| `omac continue` / `omac resume` | Re-enter the last session, or pick one interactively |
| `omac register <skill>` | Validate a skill, prompt for secrets → keychain, record it |
| `omac list` | Registered skills with mount, secret count, binary status |
| `omac secrets <sub> <skill>` | `list`, `set`, `unset`, `import --from <file>` |
| `omac provenance [--check]` | Effective allow/deny entries and the resolved cache scope |
| `omac cache clear [--all]` | Remove the active (or every inactive) tool-cache scope |
| `omac serve [harness]` | Multi-directory server backing OpenCode Desktop |
| `omac update` | Upgrade omac using the method it was installed with |

Every flag, the end-to-end workflow, and the exit codes are in
[`docs/CLI.md`](docs/CLI.md).

## Configuration

None is required — compiled-in defaults work out of the box. When you do need
to change something:

| File | Purpose |
|---|---|
| `~/.config/omac/config.yaml` (or `<workdir>/.opencode/oh-my-agentic-coder.yaml`) | Sandbox profile selection, facade tuning, audit settings, cache scope |
| `~/.config/omac/sandbox-profiles/default.json` | Filesystem grants, network mode, protected paths, env allowlist |
| `~/.config/omac/sidecar.json` | Skill registry (written by `omac register`) |
| `~/.config/omac/skill-config.yaml` | Non-secret per-skill fields |

**Details:** [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) — every field, the
audit trail format and flags, sandbox profile schema, proxy injection for
JVM/Node toolchains, and corporate-proxy chaining.
[`docs/CACHE_ISOLATION.md`](docs/CACHE_ISOLATION.md) covers the per-scope tool
caches (`cache.scope`, `--ephemeral-cache`, `omac cache clear`).

## Documentation

| Document | Contents |
|---|---|
| [`docs/INSTALLATION.md`](docs/INSTALLATION.md) | Every install path, updating, checksum verification, full prerequisites |
| [`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md) | Threat model, isolation mechanics, what the sandbox can see |
| [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) | Launcher config, audit trail, sandbox profiles, corporate proxy |
| [`docs/CACHE_ISOLATION.md`](docs/CACHE_ISOLATION.md) | Tool cache scopes, modes, cleanup, private Cargo registries |
| [`docs/CLI.md`](docs/CLI.md) | Full subcommand and flag reference, typical workflow, exit codes |
| [`docs/HARNESSES.md`](docs/HARNESSES.md) | Harness selection, session resume, bridges, skill discovery |
| [`docs/HARNESS_COMPAT.md`](docs/HARNESS_COMPAT.md) | Upstream CLI drift monitoring per harness |
| [`docs/MULTI_DIR_DESKTOP.md`](docs/MULTI_DIR_DESKTOP.md) | `omac serve` and OpenCode Desktop across directories |
| [`docs/NONO_SANDBOX.md`](docs/NONO_SANDBOX.md) | Using the external nono sandbox instead of the built-in one |
| [`CREATING_A_SKILL.md`](./CREATING_A_SKILL.md) | Authoring guide: `omac.yaml` schema, env vars, secrets, checklist |
| [`docs/ECHO_REST_EXAMPLE.md`](docs/ECHO_REST_EXAMPLE.md) | The reference `echo-rest` skill, its tests, and streaming (SSE) |
| [`docs/DEVELOP.md`](docs/DEVELOP.md) | Layout, build, test, dependencies, serve-mode dev loop |
| [`oh-my-agentic-coder.md`](./oh-my-agentic-coder.md) | The design document this implementation follows |
| [`COLLABORATION.md`](./COLLABORATION.md) | How work is tracked, branched, reviewed, and merged |

## Development

CI (`.github/workflows/ci.yml`) gates on `gofmt`, `go vet`, `staticcheck`,
build, and `go test -race`. To avoid pushing gofmt drift, install the local
pre-commit hook — it auto-formats staged `.go` files and re-stages them:

```sh
scripts/install-hooks.sh
```

Re-run after a fresh clone or after pulling hook changes. The hook only runs
`gofmt`; vet, staticcheck, build, and tests stay in CI to keep the hook fast.
For the project layout, dev and release builds, and the test matrix, see
[`docs/DEVELOP.md`](docs/DEVELOP.md).

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
