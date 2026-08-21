---
title: Quick start
description: Install omac, set up prerequisites, and launch your first session.
---

## Prerequisites

omac needs three system components:

- **Sandbox**: isolates the agent from your files and network. On Linux this is [bubblewrap](https://github.com/containers/bubblewrap); on macOS, Seatbelt is built in.
- **Secret storage**: stores skill API tokens in the OS keychain. On Linux this is the Secret Service (D-Bus); on macOS, Keychain is built in.
- **Network prompt dialog**: shows a confirmation dialog when the agent tries to reach an unknown host. On macOS, AppleScript handles this automatically.

You also need at least one harness installed — see [Supported harnesses](../intro.md#supported-harnesses).

### macOS

All three components are built into macOS. No additional install needed — proceed to [Install](#install).

### Linux (Debian / Ubuntu)

```bash
sudo apt install bubblewrap libsecret-1-0 zenity
```

Replace `zenity` with `kdialog` if you use KDE.

**AppArmor note (Ubuntu 23.10+):** On Ubuntu 23.10 and later, unprivileged user namespaces may be restricted by default, causing `bwrap` to fail with "Permission denied".
Bubblewrap needs user namespaces to build the sandbox without root privileges. 
Run `omac doctor` first — if it reports a sandbox failure, grant bubblewrap a permanent AppArmor exception. You only need to run these commands once; the exception survives reboots because AppArmor loads everything in `/etc/apparmor.d/` on startup. The second command activates it immediately:

```bash
sudo tee /etc/apparmor.d/bwrap > /dev/null <<'EOF'
abi <abi/4.0>,
/usr/bin/bwrap flags=(unconfined) {
  userns,
}
EOF
sudo apparmor_parser -r /etc/apparmor.d/bwrap
```

### Linux (Fedora)

```bash
sudo dnf install bubblewrap libsecret zenity
```

### WSL2 (Ubuntu)

WSL2 does not run a keychain daemon by default, so the secret storage setup needs extra steps compared to native Linux.

```bash
sudo apt install -y bubblewrap gnome-keyring libsecret-tools zenity
```

The keychain daemon must be running before omac can store or read secrets. Add these two lines to your `~/.bashrc` so they start automatically each time you open a WSL2 terminal:

```bash
eval "$(dbus-launch --sh-syntax)"
gnome-keyring-daemon --start --components=secrets
```

After editing `~/.bashrc`, either restart your terminal or run those two lines now to start the daemon for the current session.

Then create the default keyring once. omac can only unlock an existing keyring — it cannot create one. Run this command once and enter a passphrase when prompted:

```bash
secret-tool store --label="bootstrap" service omac account init
```

**Workdir:** Keep your repo on the native WSL2 filesystem (`~/projects/…`, not `/mnt/c/…`). The Windows NTFS mount can cause permission errors and is significantly slower.

## Install

### macOS (Homebrew)

```bash
brew tap TNG-release/tap
brew trust tng-release/tap
brew install oh-my-agentic-coder
```

### Debian / Ubuntu (apt)

The command below detects your architecture automatically (`amd64` or `arm64`) and downloads the matching release:

```bash
ARCH=$(dpkg --print-architecture)
curl -L -O \
  "https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/oh-my-agentic-coder_$(curl -s https://api.github.com/repos/TNG/oh-my-agentic-coder/releases/latest | grep tag_name | cut -d '"' -f4 | sed 's/^v//')_linux_${ARCH/amd64/x86_64}.deb"
sudo dpkg -i oh-my-agentic-coder_*_linux_*.deb
```

Or download the `.deb` for your architecture from the [releases page](https://github.com/TNG/oh-my-agentic-coder/releases) and run `sudo dpkg -i <file>.deb`.

### Arch (pacman) (experimental)

Download the `.pkg.tar.zst` for your architecture from the [releases page](https://github.com/TNG/oh-my-agentic-coder/releases), then:

```bash
sudo pacman -U oh-my-agentic-coder_*.pkg.tar.zst
```

### From source

Building from source is intended for contributors developing omac, or users who need an unreleased commit. For all other cases, use a pre-built package above.

Requires Go. Go's standard install shortcut (`go install`) requires the module path in the code to exactly match the repository URL. This repo follows Go's lowercase module path convention (`github.com/tngtech/...`) while the GitHub organisation uses uppercase (`TNG`), so the shortcut does not work. Clone and build locally instead:

```bash
git clone https://github.com/TNG/oh-my-agentic-coder.git
cd oh-my-agentic-coder
go build -o "$(go env GOPATH)/bin/omac" ./cmd/omac
```

## First run

After installing omac and its prerequisites, run these commands to verify your setup and launch your first session.

**1. Check prerequisites:**

```bash
omac doctor
```

`omac doctor` checks the sandbox, keychain, dialog backend, and harness, and prints an actionable fix for anything it flags.

**2. Install and configure a harness**: at least one is required. Each harness needs to be installed and authenticated with your AI provider (API key, login flow, etc.) before omac can use it. See [Supported harnesses](../intro.md#supported-harnesses) for the list of harnesses and links to their own documentation.

**3. Register a skill** (optional): if you want to use a skill, register it before launching — see [Skills](../usage/skills.md).

**4. Launch:**

```bash
omac start
```

Run `omac setup` after your first start to provision the built-in `omac-write-a-skill` skill, which lets the agent create new skills.

## Update

`omac update` detects your install method and upgrades accordingly.

```bash
omac update        # prompts before installing
omac update --yes  # skip confirmation (CI/scripting)
```

## Verify downloads

```bash
curl -L -O https://github.com/TNG/oh-my-agentic-coder/releases/latest/download/checksums.txt
sha256sum -c checksums.txt --ignore-missing
```
