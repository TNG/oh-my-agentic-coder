# Installation

This document covers every install path, the update mechanism, checksum
verification, and the full prerequisite matrix. The
[README quickstart](../README.md#quickstart) is enough for the common case;
come here for the details.

## Release artifacts

Pre-built binaries and packages are published to
[GitHub Releases](https://github.com/TNG/oh-my-agentic-coder/releases) on every
tagged version. The release pipeline produces:

- `oh-my-agentic-coder_<version>_macOS_{x86_64,arm64}.tar.gz` — macOS binaries
- `oh-my-agentic-coder_<version>_linux_{x86_64,arm64}.tar.gz` — Linux binaries
- `oh-my-agentic-coder_<version>_linux_{x86_64,arm64}.deb` — Debian/Ubuntu (apt)
- `oh-my-agentic-coder_<version>_linux_{x86_64,arm64}.pkg.tar.zst` — Arch (pacman)
- `oh-my-agentic-coder.rb` — Homebrew formula (also bundled in the archive)
- `checksums.txt` — SHA-256 sums of every artifact

## macOS (Homebrew)

Releases are auto-published to the
[TNG-release/homebrew-tap](https://github.com/TNG-release/homebrew-tap) tap.

```sh
brew tap TNG-release/tap
brew trust tng-release/tap   # Homebrew normalizes tap names to lowercase; refuses untrusted taps by default
brew install oh-my-agentic-coder
```

To upgrade later:

```sh
brew update
brew upgrade oh-my-agentic-coder
```

Pre-releases (tags like `v1.2.3-rc1`) are intentionally not pushed to the
tap; install those from the per-release tarball below.

## Debian / Ubuntu (apt)

```sh
ARCH=$(dpkg --print-architecture)   # amd64 or arm64
curl -L -O \
  "https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/oh-my-agentic-coder_$(curl -s https://api.github.com/repos/TNG/oh-my-agentic-coder/releases/latest | grep tag_name | cut -d '"' -f4 | sed 's/^v//')_linux_${ARCH/amd64/x86_64}.deb"
sudo dpkg -i oh-my-agentic-coder_*_linux_*.deb
```

`-O` (not `-o omac.deb`) keeps the file's original release name, which
matters for the checksum step below: `checksums.txt` lists artifacts by
that name, so renaming the download makes `sha256sum -c` silently verify
nothing instead of failing loudly.

Or, more simply, download the `.deb` matching your architecture from the
[releases page](https://github.com/TNG/oh-my-agentic-coder/releases) and run
`sudo dpkg -i <file>.deb`.

## Arch Linux (pacman)

```sh
ARCH=$(uname -m)   # x86_64 or aarch64; map aarch64 -> arm64 in URL
curl -L -O \
  "https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/oh-my-agentic-coder_<version>_linux_${ARCH}.pkg.tar.zst"
sudo pacman -U oh-my-agentic-coder_*.pkg.tar.zst
```

## Updating

However you installed omac, `omac update` detects the right method for this
host and does it for you:

```sh
omac update            # prompts for confirmation before installing
omac update --yes      # skip the confirmation prompt (scripting/CI)
```

It checks GitHub's latest release, and:

- on macOS with a Homebrew-managed install, runs `brew upgrade
  oh-my-agentic-coder` (equivalent to the `brew upgrade` above);
- on Linux, detects `dpkg`/`rpm`/`pacman`/`apk` (in that priority) and
  installs the matching package with `sudo`, same as the manual commands
  above, after verifying its SHA-256 against `checksums.txt`;
- otherwise (macOS without brew, or a Linux host with none of the above),
  downloads the tarball and replaces the running binary in place.

Declining the confirmation prompt does nothing — that's the dry run.

## Verifying downloads

Every release includes `checksums.txt`:

```sh
curl -L -O https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing
```

## From source

`go.mod`'s declared module path (`github.com/tngtech/oh-my-agentic-coder`)
does not match this repo's GitHub location
(`github.com/TNG/oh-my-agentic-coder`), so a path-based
`go install github.com/.../cmd/omac@latest` cannot resolve it either way —
clone and build locally instead, which uses local module resolution and
avoids the mismatch:

```sh
git clone https://github.com/TNG/oh-my-agentic-coder.git
cd oh-my-agentic-coder
go build -o "$(go env GOPATH)/bin/omac" ./cmd/omac   # or any dir on your $PATH
```

For the project layout, build instructions (dev and release), and test
details, see [`DEVELOP.md`](./DEVELOP.md).

## Prerequisites

omac depends on a few system-level packages. `omac doctor` checks all of
them; this section explains what each one does and what happens when it's
missing.

### Core (required)

bubblewrap, a Secret Service provider, and Python 3 — with the Ubuntu
AppArmor exception and the WSL gnome-keyring setup — are documented in
[README → Prerequisites](../README.md#prerequisites). They live there rather
than here because the README has to stand alone for a first install. The one
addition: there is no file-based keychain fallback yet (see design doc
§16.2), so a running Secret Service provider is required on Linux/WSL.

### Network prompt dialog (strongly recommended)

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

The dialog offers seven choices: allow/deny once, allow/deny permanently for
this host, allow/deny permanently for the registered suffix (e.g.
`*.example.com`), and **Explain more**, which denies the request for now and
points the agent at the intent endpoint so it declares why it wants the host —
you decide on the retry. That choice is never persisted; the permanent
decisions are, in `default.pages.json` next to the sandbox profile.

### Inner harness (pick at least one)

| Package | Install | Purpose |
|---|---|---|
| **opencode** | see [opencode docs](https://github.com/anomalyco/opencode) | Default inner harness (`omac start`) |
| **claude** (Claude Code CLI) | see [Claude Code docs](https://code.claude.com/docs) | Alternative harness (`omac start claude`) |
| **codex** (OpenAI Codex CLI) | see [Codex docs](https://github.com/openai/codex) | Alternative harness (`omac start codex`) |
| **copilot** (GitHub Copilot CLI) | see [Copilot CLI docs](https://docs.github.com/en/copilot/how-tos/copilot-cli/set-up-copilot-cli/install-copilot-cli) | Alternative harness (`omac start copilot`) |
| **pi** (Pi coding agent) | see [Pi docs](https://pi.dev) | Alternative harness (`omac start pi`) |
| **codewhale** (CodeWhale CLI) | npm package `codewhale` | Alternative harness (`omac start codewhale`), bring-your-own-model |

At least one inner harness must be installed; `opencode` is the default.

### Optional

| Package | Purpose |
|---|---|
| **nono** | Alternative sandbox runtime with credential injection and network profiles (`omac start --sandbox nono`). See [`NONO_SANDBOX.md`](./NONO_SANDBOX.md). |
| **Go** | Only needed to build omac from source (`go install …`). Pre-built binaries have no Go dependency. |

