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
| `--cache-scope <scope>` | workdir | Which cache to use for the agent's package downloads (npm, pip, cargo, etc.). `global` shares one cache across all your projects, `config` shares it across projects using the same config file, `workdir` gives each project its own isolated cache. See [Cache](../advanced/cache.md). |
| `--open-port <port>` | — | Allow the sandboxed process to bind and connect on this local TCP port (repeatable) for this session only. Useful for a local dev server or an MCP server the harness talks to. To make it permanent, use `network.open_port` in the grants file — see [Opening a port](../configuration.md#opening-a-port). |
| `--vm[=<name>]` | false | Attach this session to an omac VM (a long-lived Linux VM whose Docker runs without root rights, see [omac vm](#omac-vm)). Bare `--vm` uses the VM that [`vm-projects.yaml`](#several-vms-and-the-project-map) assigns to the project, else the default VM `omac`; `--vm=<name>` picks that VM (write it with `=`, `--vm <name>` is not accepted). Sets `OMAC_VM_HOST`, `OMAC_VM_SSH_PORT`, `OMAC_VM_SSH_KEY`, `OMAC_VM_USER`, `OMAC_VM_NAME`, `OMAC_VM_PORT_FIRST` and `OMAC_VM_PORT_LAST` inside the sandbox, which give the agent an SSH login to the VM as an unprivileged user. Docker, Gradle and tests then run in the VM through `omac vm exec`; the sandbox itself gets no Docker endpoint. Requires `omac vm up` first. Refused together with `--no-sandbox`, and with launcher profiles that run a sandbox other than omac's own (`nono`, `no-sandbox-debug`, custom wrappers), because omac can open the VM's SSH port only in its own sandbox. Also refused with a launcher profile that runs omac's sandbox with `--learn`, which would expose the VM's admin key. Your own policy file under `sandbox-profiles/` is fine, unless it fails to load, its `environment.deny_vars` removes one of these variables, or its `filesystem.override_deny` unblocks the VM's state directory. |
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
| `--vm[=<name>]` | false | Attach to an omac VM, as with `omac start --vm`. Refused together with `--no-sandbox`, `--no-inner`, `--for-opencode-desktop` or `--learn`, and with launcher profiles that run a sandbox other than omac's own (see `omac start --vm`). |

| Environment variable | Description |
|---|---|
| `OMAC_SERVE_PASSWORD` | Password for the OpenCode server that `omac serve` starts. Needs omac's own sandbox (or `--no-sandbox`) and can't be combined with `--no-inner`. Without it, OpenCode picks a new random password on every start. See [Serve mode](../advanced/serve-mode.md#how-to-use-it-today). |

#### OpenCode v2 project discovery on macOS

OpenCode 2.0.x treats denied configuration-discovery probes as fatal. In the
native macOS sandbox, `omac start` and `omac serve` therefore set
`OPENCODE_CONFIG_PROJECT_DISABLE=1` for confirmed 2.0.x versions. This avoids
startup failures and HTTP 500 responses from discovery attempts outside granted
directories.

OpenCode 2.1.0 and later tolerate denied discovery probes, so project-local
config, skills, agents, plugins, and instructions load normally there and omac
does not set the variable. A wrapper spec pinned only to `@2` cannot prove the
minor version, so omac treats it conservatively like 2.0.x and prints a
warning at launch; pin `@2.1` or newer to get project sources back.

When the sandbox profile cannot read `~/.agents`, omac warns as well: OpenCode
2.0.x may fail to start there, and OpenCode 2.1+ silently skips the global
skills stored in that directory. Add `~/.agents` to the profile's
`filesystem.read` to load them.

### omac continue

Re-enters the most recent session for this workdir or a session with a certain ID (omac prints an `omac continue -s <id>` hint when a session ends).

```bash
omac continue              # most recent session
omac continue -s <id>      # specific session by ID
```

`omac continue` accepts `--vm[=<name>]`, as `omac start` does.

### omac resume

Use this when you want to choose from a list of past sessions. Shows a numbered picker of recent sessions for the active workdir. `omac resume` accepts `--vm[=<name>]`, as `omac start` does.

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

The omac VM is a long-lived Linux virtual machine, which omac creates and manages with [Lima](https://lima-vm.io). Docker runs inside it, and a session started with `--vm` reaches that Docker through `omac vm exec`. The VM does not mount your host files. Only `omac vm exec` copies the current worktree (the git top level) into it, and those copies stay in the VM. The VM has no network access, except while `omac vm up` or `omac vm up --reprovision` sets it up and while it is online.

```bash
omac vm up                     # create, provision and switch offline (needs network once); later: start the VM in its last mode
omac vm status                 # show state, mode, SSH port, images, sessions, packages, JDKs, copied worktrees, resources from vm.yaml or defaults
omac vm image add postgres:16  # import an image; the VM never pulls itself
omac vm image list             # list the VM's images
omac vm image rm postgres:16   # remove an image from the VM
omac start --vm                # the session runs Docker and builds in the VM with omac vm exec
omac vm exec -- ./gradlew test --offline   # copy the worktree into the VM and run there
omac vm online                 # restart the VM with network access, e.g. to download build dependencies
omac vm offline                # restart the VM without network access, the mode sessions need
omac vm down                   # stop the VM
omac vm reset                  # delete the VM and create it anew
```

You need Lima; omac is tested with Lima 2.1 (`brew install lima` on macOS; on Linux the release tarball from the Lima GitHub releases or Homebrew on Linux, plus QEMU).
Because the VM has no network, `docker pull` inside it fails. Instead, `omac vm image add` downloads the image on your host and loads it into the VM, once per image.

Each VM has its own block of 100 ports: 30000 to 30099, 30100 to 30199, and so on up to 10 VMs. A new VM gets the lowest block that no other VM holds and no host process listens in (see [Several VMs](#several-vms-and-the-project-map)). `omac vm list` shows the block; a session gets it as `OMAC_VM_PORT_FIRST` and `OMAC_VM_PORT_LAST`. Container ports published in the block (for example `docker run -p 30000:5432 postgres:16` in the VM with block 30000 to 30099) are reachable from your host on `127.0.0.1` at the same port, not from inside the sandbox. Ports outside the block stay inside the VM; use `omac vm exec` to reach them from the VM side.

The VM is either `offline` (the default) or `online`. Sessions attach only while it is offline. `omac vm online`, `omac vm up --reprovision` and `omac vm reset` take the VM online, so they are refused while a session is attached.

`omac vm up --reprovision` reinstalls the extra packages and JDKs listed in `vm.yaml` (`vms/<name>.yaml` with `--name <name>`) and runs its setup script again, and `omac vm reset` deletes the VM and creates it anew. For CPUs, memory, disk, packages, Java versions and a setup script, see [omac VM (vm.yaml)](../configuration.md#omac-vm-vmyaml).

#### Several VMs and the project map

The VM called `omac` is the default. To keep projects apart, create more VMs, for example one per client or per team. The verbs `up`, `down`, `reset`, `status`, `online`, `offline`, `image` and `exec` take `--name <name>`; without it they act on the default VM. A name is a lowercase letter followed by up to 15 lowercase letters, digits or `-`; `true` and `false` are reserved. Each VM has its own state, images, build caches, copied worktrees and port block, and at most 10 VMs can exist.

```bash
omac vm up --name acme                  # create and provision a second VM
omac vm image add --name acme postgres:16
omac vm list                            # one line per VM
omac start --vm=acme                    # attach this session to it
```

`omac vm list` prints a table with these columns:

| Column | Meaning |
|---|---|
| `NAME` | The VM's name. |
| `STATE` | `offline` or `online`; `not created` when the VM has no state yet; `error` when its state file cannot be read. STATE shows the stored mode, not whether the VM is running; use `omac vm status` for that. |
| `SSH PORT` | The loopback port of the VM's SSH, or `-` if the VM was never created. |
| `PORTS` | The VM's block of published ports, for example `30100-30199`, or `-` if the VM was never created. |

Instead of typing `--vm=acme` every time, assign projects to VMs in `~/.config/omac/vm-projects.yaml` (or under `$XDG_CONFIG_HOME/omac`). Each key is the absolute path of a project and each value the name of its VM:

```yaml
/Users/alex/work/acme-backend: acme
/Users/alex/work/acme-frontend: acme
/Users/alex/private/side-project: hobby
```

A session picks its VM in this order:

1. `--vm=<name>` on the command line.
2. The entry for the project in `vm-projects.yaml`.
3. The default VM `omac`.

A bare `--vm` skips step 1. `omac vm exec` follows the same order, with `--name <name>` as step 1, except inside a session, where it always uses the session's VM and refuses a different `--name`.

`omac serve --vm` picks the VM once, for the directory it starts in (or `--workdir`), and every project the server opens uses that VM. To keep projects of different VMs apart, run one server per VM.

An entry applies to its directory and everything below it; when several entries apply, the one with the longest path wins. For example:

- A repository's entry also covers its subdirectories, its submodules and the git worktrees inside it, such as under `.worktrees/`.
- A git worktree outside the repository's directory uses the repository's entry, unless an entry covers the worktree's own path.
- Worktrees of a bare repository (one without a working directory of its own) need an entry that covers their own path.

Keys must be absolute paths. A relative key, an invalid VM name, two keys for the same directory (for example a path and a symlink to it) or a malformed file stops omac, and the message names the entry. If the mapped VM was never created, the session does not start and the message says to run `omac vm up --name <name>`.

Projects in the same VM can read each other's copies, see [The omac VM](../security.md#the-omac-vm). Put projects that must not see each other into different VMs.

omac has no command that deletes a VM. To delete the VM `acme`, for example to free its port block, stop it and remove its state directory, which holds the VM's disk:

```bash
omac vm down --name acme
LIMA_HOME=~/.local/state/omac/vms/acme/lima limactl delete -f omac
rm -rf ~/.local/state/omac/vms/acme ~/.config/omac/vms/acme.yaml
```

If `XDG_STATE_HOME` is set, the state directory is `$XDG_STATE_HOME/omac/vms/acme` instead. The default VM `omac` keeps its state in `~/.local/state/omac/vm`, so its Lima home is `~/.local/state/omac/vm/lima`. Remove the VM's entries from `vm-projects.yaml` too.

`omac vm exec` runs a command inside the VM on a copy of your worktree (the git top level), for example a Gradle build that needs more memory or tools than the sandbox has. It works from your host and from inside a session started with `--vm`. The built-in skill `omac-vm` (see [Built-in skills](./skills.md#built-in-skills)) tells the agent in such a session how to use it and when to ask you for help.

```bash
omac vm exec -- ./gradlew test --offline
omac vm exec --cwd backend -- ./gradlew test --offline
omac vm exec -- cat backend/build/reports/tests/test/index.html   # results stay in the VM; read them back like this
```

Docker commands run in the VM, so paths in `docker run -v` and `docker build` refer to the VM, for example the worktree copy that `omac vm exec` runs in: `omac vm exec -- docker build -t app .`, or `omac vm exec -- sh -c 'docker run --rm -v "$PWD":/src -w /src <image> ls'`, where `$PWD` is the directory the command runs in (see below).

The command runs in the same place inside the copy as your current directory is in the worktree. `--cwd <dir>` runs it in `<dir>`, relative to the current directory. Absolute paths and `..` are refused; to reach a sibling directory, run from the top level. So `omac vm exec --cwd backend -- …` from the repository root and `omac vm exec -- …` from `backend/` both run in `backend`. Outside a git repository, omac copies the current directory.

omac refuses to copy in two cases:

- The directory it would copy overlaps a path the sandbox protects. For example, outside a git repository in your home directory, the copy would include `~/.ssh`. Run it from a project that is its own git repository.
- The current directory is below the top level of a git repository, but git tracks no file in it, for example a new directory you have not committed yet. Run it from the top level with `--cwd <dir>` instead.

`--env NAME` passes the variable `NAME` with the value from your shell. The variable must be set there, possibly to an empty value, otherwise the command fails before anything runs. `--env NAME=VALUE` sets the value explicitly. Repeat `--env` for more variables. `--env` is meant for priming, see below. `--verbose` prints which directory omac copies and where it goes in the VM.

omac returns the command's exit code as is, with two exceptions. ssh exits with 255 when the connection is lost or the command is killed by a signal, and omac reports that as 5. With a terminal, Ctrl-C goes to the command, so a command that dies from it also ends with 5; without a terminal, Ctrl-C stops omac, which exits with 130. The command's code can coincide with omac's own exit codes 2 (wrong usage), 4 (VM not created) and 5 (omac could not or would not copy, or the connection failed). For example, a command that ends with `exit 5` looks like a failed copy, so check the error message on stderr. A command's own exit 255 is also reported as 5, and its own 130 looks like an interruption.

When stdin, stdout and stderr are all a terminal, the command gets one in the VM too, and Ctrl-C stops it there. Otherwise, for example when you pipe input into `omac vm exec` or redirect its output or errors to a file, Ctrl-C stops only omac and the command keeps running in the VM.

The copy goes one way only:

- `.git`, the `.env` files the sandbox hides, and the paths git ignores, also inside submodules, are not copied. `build/`, `.gradle/` and `node_modules/` stay out only if `.gitignore` lists them.
- Nothing on your host is deleted or changed.
- Files you delete on the host are not removed from the copy in the VM. If a stale file gets in the way, run `omac vm exec -- rm -rf <path>` or `omac vm reset`.
- Nothing is copied back from the VM. Build output stays there, so read it with another `omac vm exec`.

`omac vm status` lists the worktrees copied so far. It runs only on your host, because the sandbox blocks the VM's state; inside a session, `omac vm exec -- true` checks that the VM is reachable.

Since the VM is offline, a build that downloads dependencies has to fill its caches once while the VM is online. Run this from your host. omac accepts `--env` only there and only while the VM is online, which also means no session is attached. `--env` passes a token the build needs for its downloads.

The priming build runs the VM's copy of your project online and with the token. That copy can hold files an agent wrote only in the VM, or deleted on your host after they were copied, so `git status` on your host does not show everything. Prime from a fresh clone of a commit you trust, in a new folder. That commit must contain the build change that needs the new dependency, so review the change, commit it, for example on a branch, and clone that commit. The fresh clone keeps unreviewed files out on your side only: earlier sessions may have prepared the VM's copy of any folder, or left files in the VM user's home. If you do not trust every earlier session, run `omac vm reset` first and import the images again, see [What `omac vm exec` shares with the VM](../security.md#the-omac-vm).

```bash
omac vm online
omac vm exec --env MY_REPO_TOKEN --cwd backend -- ./gradlew check --no-daemon
omac vm offline
```

For Maven, the middle line is `omac vm exec --env MY_REPO_TOKEN --cwd backend -- ./mvnw verify`. Maven reads a token only from a `<server>` entry in a settings file, and sends it only to a mirror or repository with the same `<id>`. So commit a settings file such as `.mvn/settings.xml` with a `<mirror>` and a `<server>` that share an `<id>`, and `<password>${env.MY_REPO_TOKEN}</password>`, and a `.mvn/maven.config` with exactly the line `--settings=${maven.multiModuleProjectDirectory}/.mvn/settings.xml` (Maven 3.9 or later). Then priming and later offline builds use the same settings; a build without the settings' mirror fails offline although its cache is complete. This applies to every Maven run of the project, also outside the VM: the project's settings file replaces `~/.m2/settings.xml`. Every online Maven run of the project then needs `MY_REPO_TOKEN` in its environment, on developer machines and in CI too.

After that, the build finds its dependencies in the VM when it runs offline, for example `./gradlew test --offline` or `./mvnw -o test`. Keep Gradle's `--offline`: without it, Gradle checks version ranges and `-SNAPSHOT` dependencies online again after 24 hours and fails in the VM. If the build fails, still run `omac vm offline`: omac refuses `--vm` sessions while the VM is online. Prime with the build step that runs all the checks you need (`check` for Gradle, `verify` for Maven), not only the tests: some tools, such as Checkstyle or Spotless, are downloaded only when the step that uses them runs.

The priming run executes as the same VM user that every earlier `--vm` session controlled, and it trusts everything those sessions could leave in that user's home. Examples are a Gradle init script in `~/.gradle/init.d/`, a Maven `~/.mavenrc`, an edited build file in the copy, or a process still running. Use a short-lived, read-only token, because anything a session left behind runs online with it. That build can also store the token in the VM, where later sessions can read it; revoke the token after priming. Use `--no-daemon` for the priming build, so that no Gradle daemon keeps the token after the run. Each time you use `--env`, `omac vm exec` names the variables the build can read and reminds you of the reset. When stdin and stderr are a terminal, it then waits before it copies anything: Enter on an empty line continues. Anything else, such as typing `n` before Enter, Ctrl-C or Ctrl-D on an empty line, stops it with exit code 130. In some shells, pasting the three lines at once gives `omac vm offline` to that prompt as its answer: the build stops and the VM stays online. So run the lines one at a time, and run `omac vm offline` if the build stopped.

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
