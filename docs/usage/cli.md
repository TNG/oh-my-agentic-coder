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
| `--ephemeral-docker` | false | Boot a session-private throwaway Docker VM and export `DOCKER_HOST` to the sandboxed agent. See [Ephemeral Docker sessions](#ephemeral-docker-sessions). |
| `--cache-scope <scope>` | global | Which cache to use for the agent's package downloads (npm, pip, cargo, etc.). `global` shares one cache across all your projects, `config` shares it across projects using the same config file, `workdir` gives each project its own isolated cache. See [Cache](../advanced/cache.md). |
| `--open-port <port>` | — | Allow the sandboxed process to bind and connect on this local TCP port (repeatable) for this session only. Useful for a local dev server or an MCP server the harness talks to. To make it permanent, use `network.open_port` in the grants file — see [Opening a port](../configuration.md#opening-a-port). |
| `--no-audit` | false | Disable the security audit trail for this session. |

For all flags: `omac start --help`.

#### Ephemeral Docker sessions

`omac start --ephemeral-docker` boots a short-lived Linux VM ([Lima](https://github.com/lima-vm/lima) with QEMU) that has its own Docker daemon, and exports `DOCKER_HOST` into the sandbox. This lets the agent run container-based tests (Testcontainers, docker-compose) without a shared host daemon. The flag is also accepted by `omac continue` and `omac resume`.

- The VM has no host filesystem mounts, host sockets, or credentials. Its Docker endpoint is forwarded to a deterministic loopback port that the sandbox is granted access to; published container ports are also forwarded to loopback.
- The guest runs the omac-vmguard firewall: image pulls and published container ports work, but new outbound connections from containers to the outside are dropped.
- The Alpine base image is digest-pinned and shared between sessions of the same cache scope (see [Cache](../advanced/cache.md)). With `--ephemeral-cache`, the image is downloaded anew each session.
- When the session ends, the VM and its state are deleted. A unit left behind by a crash is reaped by the next session's orphan sweep.

Limitations and failure behavior:

- The verified host is an Apple Silicon Mac. Linux and WSL users can try the same Lima/QEMU path and contribute platform-specific setup and test results. Install `limactl`, QEMU, and the firmware needed by the guest architecture. For x86_64 hosts, add a verified image digest in `internal/ephemeraldocker/limaconfig.go` and update its image-pin tests; unpinned images remain rejected.
- A missing `limactl` prints an install hint and exits with code 4; any boot failure (endpoint timeout, firewall verification) deletes the VM, exits with code 1, and names the VM so leftovers stay traceable.

For projects with private Gradle dependencies, run the project's wrapper on the host first with `GRADLE_USER_HOME` pointing to a project-local ignored directory, using your normal registry credentials and host Docker if the build needs it. Then run the same task inside `omac start --ephemeral-docker` with that directory as `GRADLE_USER_HOME` and Gradle's `--offline` flag. Allow `GRADLE_USER_HOME` through the sandbox profile's environment filter and grant the required JDK path; projects with dynamic local ports also need `network.open_port: [0]` on macOS. omac does not prime Gradle dependencies or provide private Maven credentials.

For Linux/WSL validation, start with `omac start --ephemeral-docker --verbose --inner /bin/bash`. Check VM boot, the Docker endpoint, published container ports, blocked container egress, and teardown before running the full test suite. Linux's Landlock filter grants TCP access by port across all destinations, so use explicit port grants and retain kernel enforcement. The macOS loopback wildcard `[0]` is not a Linux equivalent. Record the working profile and commands with your test results.

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

### omac resume

Use this when you want to choose from a list of past sessions rather. Shows a numbered picker of recent sessions for the active workdir.

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
