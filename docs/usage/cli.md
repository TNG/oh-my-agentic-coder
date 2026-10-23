---
title: CLI reference
description: Commonly used omac subcommands and flags
---

This page covers the most commonly used subcommands and flags. Run `omac --help` for a list of all subcommands, and `omac <subcommand> --help` for the full flag list of any subcommand.

## Global flags

| Flag | Description |
|---|---|
| `--workdir <dir>` | Set the working directory (default: cwd). |

---

## Sessions

### omac start

Starts skill sidecars and launches the harness (agent CLI) inside the OS sandbox. After the session ends, prints a hint with a session ID you can use to resume with `omac continue -s <id>`.

Refuses to start if any skill in the workdir is unregistered, its source files have changed since registration (review and run `omac register` again to fix), or a required config field cannot be resolved.

```bash
omac start              # default harness (opencode)
omac start claude       # specific harness
```

| Flag | Default | Description |
|---|---|---|
| `--sandbox <profile>` | builtin | Sandbox profile: `builtin` (recommended), `nono` (deprecated), `nono-netprofile` (deprecated), `no-sandbox-debug` (removes all isolation, debug only). |
| `--no-sandbox` | false | Run the harness without sandboxing. Removes all isolation — debug use only. |
| `--ephemeral-cache` | false | Use a temporary cache deleted when the session ends. Use this for a clean build environment or when you do not want the agent's package downloads to persist. |
| `--cache-scope <scope>` | global | Which cache to use for the agent's package downloads (npm, pip, cargo, etc.). `global` shares one cache across all your projects, `config` shares it across projects using the same config file, `workdir` gives each project its own isolated cache. See [Cache](../advanced/cache.md). |
| `--open-port <port>` | — | Allow the sandboxed process to bind and connect on this local TCP port (repeatable) for this session only. Useful for a local dev server or an MCP server the harness talks to. To make it permanent, use `network.open_port` in the grants file — see [Opening a port](../configuration.md#opening-a-port). |
| `--vm` | false | Attach this session to the omac VM (a long-lived Linux VM whose Docker runs without root rights, see [omac vm](#omac-vm)). Sets `DOCKER_HOST` and `DOCKER_CONFIG` inside the sandbox, plus `OMAC_VM_HOST`, `OMAC_VM_SSH_PORT`, `OMAC_VM_SSH_KEY` and `OMAC_VM_USER`, which give the agent an SSH login to the VM as an unprivileged user. Requires `omac vm up` first. Refused together with `--no-sandbox`, and with launcher profiles that run a sandbox other than omac's own (`nono`, `no-sandbox-debug`, custom wrappers), because omac can open the VM's ports only in its own sandbox. Your own policy file under `sandbox-profiles/` is fine. |
| `--no-audit` | false | Disable the security audit trail for this session. |

For all flags: `omac start --help`.

### omac serve

Use this instead of `omac start` when running **OpenCode Desktop**. Unlike `omac start`, which handles one project at a time and requires a restart to switch projects, `omac serve` is designed to stay running and serve multiple projects. Use `--workdir` to specify which project to activate at startup — automatic project switching from OpenCode Desktop is not yet implemented.

```bash
omac serve
omac serve --workdir ~/my-project
```

| Flag | Default | Description                                                                                                                                                                                                                                    |
|---|---|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--workdir <dir>` | — | Activate this directory immediately at startup, so skills are ready as soon as OpenCode Desktop connects. Without this flag, no skills will be active — OpenCode Desktop does not yet automatically tell omac which project is currently open. |
| `--root <dir>` | — | Only allow directories under this path to be activated (repeatable). For example, `--root ~/work --root ~/personal` restricts activation to those two trees. If not set, any directory can be activated.                                       |
| `--vm` | false | Attach to the omac VM, as with `omac start --vm`. Refused together with `--no-sandbox` or `--no-inner`, and with launcher profiles that run a sandbox other than omac's own (see `omac start --vm`). |

#### OpenCode v2 project discovery on macOS

For confirmed OpenCode v2+ versions, `omac start` and `omac serve` set
`OPENCODE_CONFIG_PROJECT_DISABLE=1` in the native macOS sandbox. This avoids
discovery attempts outside granted directories. Project and parent-directory
sources such as `opencode.json`, `.opencode`, `.claude`, and `.agents` are not
loaded automatically, even inside the project. Other versions and sandbox
backends are unchanged. If `environment.deny_vars` blocks this variable, omac
stops the launch instead of silently dropping the workaround.

### omac continue

Re-enters the most recent session for this workdir or a session with a certain ID (omac prints an `omac continue -s <id>` hint when a session ends).

```bash
omac continue              # most recent session
omac continue -s <id>      # specific session by ID
```

`omac continue` accepts `--vm`, as `omac start` does.

### omac resume

Use this when you want to choose from a list of past sessions rather. Shows a numbered picker of recent sessions for the active workdir. `omac resume` accepts `--vm`, as `omac start` does.

---

## Skill management

### omac register

Prompts for secrets (API keys) and config fields (non-secret settings such as base URLs), stores them securely, and adds the skill to the registry. If the skill includes installation scripts for dependencies, omac prints their paths but never runs them.

```bash
omac register <skill>
omac register <skill> --defaults   # use previously stored values, prompt only for new ones
```

| Flag | Default | Description |
|---|---|---|
| `--defaults` | false | Use previously remembered values without prompting. omac saves values from every registration globally, matched by skill and secret name. The first registration still prompts; after that, `--defaults` reuses them silently in any project. |
| `--no-secrets` | false | Skip secret prompts. Use together with `--secrets-from`, or when you already stored the secrets in a prior step via `omac secrets import` and don't want to be prompted again. |
| `--secrets-from <file>` | — | Read secrets from a `KEY=VALUE` file instead of prompting. Each line must match a secret name declared in the skill's `omac.yaml`. |

**Injecting secrets in CI:**

```bash
# Write CI environment variables to a file, then register non-interactively
echo "API_KEY=$MY_CI_SECRET" > /tmp/secrets.env
omac register <skill> --secrets-from /tmp/secrets.env --no-fields

# Or import into an already-registered skill
omac secrets import <skill> --from /tmp/secrets.env
```

Note: the OS keychain must be running even in CI. On Linux this requires gnome-keyring or an equivalent Secret Service provider — see [Quick start](../getting-started/quick-start.md) for setup.

### omac deregister

Removes a skill's registration. Source files are kept on disk. After deregistering, `omac start` will refuse to run until you re-register the skill or delete its directory.

```bash
omac deregister <skill>
omac deregister <skill> --purge-secrets   # also remove secrets from the keychain
```

| Flag | Default | Description |
|---|---|---|
| `--purge-secrets` | false | Also delete the skill's secrets from the keychain. |

For all flags: `omac deregister --help`.

### omac list

Shows registered skills, their mount points, and whether the sidecar binary is present.

```bash
omac list
omac list --all   # include stale registrations whose directory has been deleted
```

### omac secrets

Manage skill secrets in the OS keychain without re-running the full registration.

```bash
omac secrets list <skill>                      # show which secrets are stored
omac secrets set <skill> <NAME>                # set one secret (prompts for value)
omac secrets unset <skill> <NAME>              # remove one secret from the keychain
omac secrets import <skill> --from <file>      # set multiple secrets from a KEY=VALUE file
```

`omac secrets import` is the recommended way to pre-seed secrets in CI before running `omac register --no-secrets`.

### omac config

Show or retrieve resolved config values for a skill.

```bash
omac config show <skill>          # all config + secret fingerprints
omac config get <skill> <field>   # one value, suitable for $(...)
```

---

## Operations

### omac setup

Provisions omac's built-in skills into each installed harness's skills directory. Scans `PATH` to find which harnesses are installed. Run once after installing a new harness.

```bash
omac setup              # all harnesses found on PATH
omac setup opencode     # one harness only
```

### omac doctor

Checks that omac and its prerequisites are correctly set up. Prints a specific fix for anything it finds wrong.

### omac diagnose

Reads the audit trail from a previous session and explains what the sandbox blocked and why. Use this when a run fails due to a network or filesystem denial. Does not use an LLM — purely deterministic analysis.

```bash
omac diagnose
```

Requires the audit trail from a previous `omac start` or `omac serve` run. The audit trail is written automatically unless `--no-audit` was passed.

### omac update

```bash
omac update        # prompts before installing
omac update --yes  # skip confirmation
```

### omac cache

The cache stores downloaded packages (npm, pip, cargo, etc.) between sessions, isolated from your regular package caches. Use `omac start --ephemeral-cache` if you want a session with a clean cache that is deleted afterwards.

```bash
omac cache clear        # remove the cache for the current scope
omac cache clear --all  # remove all caches not currently in use
```

### omac vm

The omac VM is one long-lived Linux virtual machine per computer, which omac creates and manages with [Lima](https://lima-vm.io). Docker runs inside it, and a session started with `--vm` uses that Docker instead of one on your host. The VM sees none of your host files and has no network access, except while `omac vm up` or `omac vm up --reprovision` sets it up and while it is online.

```bash
omac vm up                     # create and provision once, then switch offline (needs network once)
omac vm status                 # show state, mode, ports, images, sessions and the resources set in vm.yaml
omac vm image add postgres:16  # import an image; the VM never pulls itself
omac vm image list             # list the VM's images
omac vm image rm postgres:16   # remove an image from the VM
omac start --vm                # DOCKER_HOST inside the sandbox points at the VM
omac vm online                 # restart the VM with network access, e.g. to download build dependencies
omac vm offline                # restart the VM without network access, the mode sessions need
omac vm down                   # stop the VM
omac vm reset                  # delete the VM and create it anew
```

You need Lima; omac is tested with Lima 2.1 (`brew install lima` on macOS; on Linux the release tarball from the Lima GitHub releases or Homebrew on Linux, plus QEMU).
Because the VM has no network, `docker pull` inside it fails. Instead, `omac vm image add` downloads the image on your host and loads it into the VM, once per image.

Container ports published in the range 30000 to 30099 (for example `docker run -p 30000:5432 postgres:16`) are reachable from your host on `localhost` at the same port. Ports outside that range stay inside the VM.

The VM is either `offline` (the default) or `online`. Sessions attach only while it is offline. `omac vm online`, `omac vm up --reprovision` and `omac vm reset` take the VM online, so they are refused while a session is attached.

`omac vm up --reprovision` reinstalls the extra packages listed in `vm.yaml`, and `omac vm reset` deletes the VM and creates it anew. For CPUs, memory, disk and packages, see [omac VM (vm.yaml)](../configuration.md#omac-vm-vmyaml).

### omac provenance

Shows what your current sandbox configuration allows and blocks — useful for verifying your effective policy before a run.

```bash
omac provenance          # show full policy
omac provenance --check  # report risky grants only
```

### omac audit

The audit trail is written automatically during `omac start` and `omac serve` — there is nothing to run manually. Use `omac diagnose` to read and interpret the trail. To disable or redirect audit logging, pass `--no-audit` or `--audit-log <path>` to `omac start` or `omac serve`.

---

## Low-level

### omac sandbox

Runs any command inside the OS sandbox without a full agent session — no skills, no facade. Use this to test your sandbox configuration or debug what a sandboxed process can access.

```bash
omac sandbox run -- ls ~                       # check which home directory files are visible
omac sandbox run -- curl https://example.com   # test network access
```

For all available flags: `omac sandbox run --help`.

### omac version

```bash
omac version
```
