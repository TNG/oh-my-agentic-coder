
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

Fix: follow the WSL2 keychain setup in [Installation → WSL2 (Ubuntu)](./getting-started/quick-start.md#wsl2-ubuntu).

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

Fix: grant a one-time AppArmor exception — see [Installation → Linux (Debian / Ubuntu)](./getting-started/quick-start.md#linux-debian--ubuntu).

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

Fix: install the missing harness — see [Supported harnesses](README.md#supported-harnesses-and-os).

### Harness hangs or BunInstallFailedError on WSL2

```
omac start: BunInstallFailedError
```

Symptoms on WSL2: `omac start` fails with `BunInstallFailedError`, or the harness hangs on start or during its login flow. `omac doctor` reports the harness as found, so the binary looks fine.

Cause: WSL2 is running a Windows-installed harness instead of a Linux one. If you also develop on Windows, your Windows `PATH` is visible inside WSL, so a harness installed on Windows (for example under `/mnt/c/Users/.../AppData/Roaming/npm/`) can be picked up by mistake. `omac doctor` cannot detect this — it only checks that the binary is on `PATH`, not which platform it was built for.

Fix: check where the harness resolves, and reinstall it natively inside WSL if it points into `/mnt/c/`.

### Agent cannot authenticate with its AI provider

The harness starts but every model call fails with an authentication or missing-API-key error.

Cause: the sandbox only receives environment variables on the `allow_vars` list. Two common misconfigurations:

- `allow_vars` is empty — this fails **closed** (nothing passes through), not open.
- The harness reads its API key from an environment variable that is not allow-listed. claude-code, codex, and copilot auto-forward their provider keys automatically; the multi-provider harnesses — opencode, pi, and codewhale — do not, so you must add the variable yourself.

Fix: add the variable to `allow_vars` in `~/.config/omac/sandbox-profiles/default.json`. See [Configuration](./configuration.md).

### An MCP server or other harness-launched tool cannot reach its token or open its port

The harness (opencode, claude-code, …) launches MCP servers **inside the sandbox**, so the MCP server is limited by the sandbox restrictions. Two things commonly need granting:

- **A missing token.** The tool reads an API token from an environment variable that is not allow-listed. Fix: add the variable name to `environment.allow_vars` and export it before `omac start`. **This means that the agent can also access your token!** Alternatively, you can define a skill for MCP access.
- **A blocked local port.** The tool opens a local port that the sandbox blocks by default. Fix: add the port to `network.open_port`, or pass `omac start --open-port <port>` (if you want to open the port only for the current session). A blocked loopback port is enforced by the kernel and leaves no entry in the audit trail, so it does not show up as a denied connection in `omac diagnose`. To check a specific port, run `omac diagnose --probe 127.0.0.1:<port>`, which reports whether `open_port` currently allows it.

See [Running an MCP server the harness launches](./configuration.md#running-an-mcp-server-the-harness-launches) for a combined example.

### Gradle build hangs or cannot reach its daemon

Cause: the Gradle daemon talks to its client over a random loopback port, which the sandbox's default kernel network enforcement blocks.

Fix: run Gradle without the daemon — `./gradlew --no-daemon` (or set `org.gradle.daemon=false`). This is the recommended fix.

On macOS only, if you must keep the daemon, you can grant loopback with `"network": { "open_port": [0] }` in the sandbox grants file (`~/.config/omac/sandbox-profiles/default.json`) — `0` means "any loopback port" and external egress stays kernel-blocked. On Linux there is no equivalent that keeps kernel enforcement, so use `--no-daemon`.

### Undo a network allow/deny decision

When you answer the network prompt, "session" decisions are held in memory only, while "permanent" decisions are written to `sandbox-profiles/default.pages.json`.

- Session decision: restart `omac start` — session decisions never persist and cannot be edited from a file, so a restart clears them.
- Permanent decision: remove the entry from `~/.config/omac/sandbox-profiles/default.pages.json`.

## Reporting a bug

If `omac doctor` and the fixes above don't resolve your problem, please report it
as a GitHub issue at
[github.com/TNG/oh-my-agentic-coder/issues](https://github.com/TNG/oh-my-agentic-coder/issues).

Before opening one:

1. **Search first.** Look through the [existing issues](https://github.com/TNG/oh-my-agentic-coder/issues?q=is%3Aissue)
   for your problem. If you find a match, add your details there instead of
   opening a duplicate.
2. **Use the issue template.** The "New issue" form guides you through the fields
   we need — a short summary, what went wrong, and how to reproduce it.
3. **Include your environment.** Paste the output of `omac doctor`, your OS and
   version, and the harness you were running (for example `claude-code` on
   Ubuntu 24.04).

Keep it concise. The [collaboration guide](https://github.com/TNG/oh-my-agentic-coder/blob/main/COLLABORATION.md#issue-rules) has
the full set of expectations for issue formatting.