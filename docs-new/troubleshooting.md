
---
title: Troubleshooting
description: Diagnosing and fixing common omac errors
---

Run `omac doctor` first — it checks all prerequisites and prints the fix for whatever it finds.

## omac doctor

- **OS keychain**: pings the keychain backend (Keychain on macOS, Secret Service on Linux)
- **Launcher config**: validates the launcher config file if one exists
- **Skill registry**: loads registered skills with sidecars and checks each one: is a binary/script present, are required secrets present?
- **Inner harnesses**: reports which harness CLIs (`opencode`, `claude`, …) are on `PATH`
- **Built-in skills**: checks whether `omac-write-a-skill` is provisioned for each installed harness
- **Sandbox backend**: checks the platform sandbox binary (`sandbox-exec` on macOS, `bwrap` on Linux) and warns if the network prompt dialog (osascript / zenity / kdialog) is missing
- **Sandbox profile**: warns about broad filesystem grants that weaken isolation

## Common errors

### dial unix /run/user/1000/bus — no Secret Service provider found  (WSL2)

Secret Service is the Linux standard for secure credential storage, accessed over D-Bus. WSL2 does not run the required daemon by default.

```
dial unix /run/user/1000/bus: connect: no such file or directory
```
```
no Secret Service provider found
```

Cause: gnome-keyring is not running; WSL2 has no Secret Service by default.

Fix: follow the WSL2 keychain setup in [Installation → Platform notes](./getting-started/quick-start.md#platform-notes).

### chmod temp: operation not permitted (WSL2)

```
chmod <tmpdir>: operation not permitted
```

Cause: the working directory is on an NTFS mount (`/mnt/c/`), which does not support Unix permissions.

Fix: move your repo to the native WSL2 filesystem.

```bash
mv /mnt/c/path/to/repo ~/projects/repo
cd ~/projects/repo
```

### bubblewrap (bwrap): permission denied (AppArmor, Ubuntu 23.10+)

```
bwrap: No permissions to creating new namespace, likely because the kernel does not allow non-privileged user namespaces
```

Cause: Ubuntu 23.10+ restricts unprivileged user namespaces by default (`kernel.apparmor_restrict_unprivileged_userns=1`).

Fix: grant a one-time AppArmor exception — see [Installation → Platform notes](./getting-started/quick-start.md#platform-notes).

### unregistered skills found

```
omac start: unregistered skills found in this workdir:
  <skill-name> — register with: omac register <skill-name>
```

Cause: the skill directory exists but `omac register` was never run for it.

Fix:

```bash
omac register <skill-name>
```

### exec: node: executable file not found in $PATH

```
exec: "node": executable file not found in $PATH
```
```
exec: "<harness>": executable file not found in $PATH
```

Cause: the inner harness binary is not installed or not on `PATH`.

Fix: install the missing harness — see [Harnesses](usage/harnesses.md).