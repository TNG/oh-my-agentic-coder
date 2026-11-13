# Setting up the omac VM

Use this guide to walk a human through setting up the omac VM. The human runs
every command below in a terminal on their computer (the host), outside any
omac session. You never run host steps yourself; give them the exact commands
and explain what each one does.

## 1. What the VM is for

The omac VM is one Linux virtual machine per computer that stays around
between sessions. It runs Docker and builds that the sandbox cannot run, for
example Testcontainers tests. It has no internet access by default, so the
human brings in Docker images and build dependencies. A session started with
`--vm` can then use it.

## 2. Prerequisites

- **Lima**, which runs the VM. omac is tested with Lima 2.1. On macOS:
  `brew install lima`. On Linux: the release tarball from the Lima GitHub
  releases or Homebrew on Linux, plus QEMU. WSL2 counts as Linux; QEMU is
  very slow there unless `/dev/kvm` exists (nested virtualization).
- **rsync** on the host, which copies the project into the VM. macOS ships
  one; on Linux install the `rsync` package.
- **Resources**: by default the VM takes half the host's CPU cores (at least
  4), a quarter of its RAM (at least 8 GiB) and 60 GiB of disk.

## 3. Optional: `~/.config/omac/vm.yaml`

Only needed to change the size or to install Java or extra packages. Every
field is optional. Example for a Java 21 project:

```yaml
cpus: 6
memory: 12GiB
jdks: [temurin-21]
packages: [jq]
```

`jdks` takes `temurin-<version>`; `packages` are Ubuntu package names.

## 4. Create the VM

```sh
omac vm up
omac vm status
```

`omac vm up` creates the VM with internet access, installs Docker and the
`vm.yaml` extras, then switches it offline. It takes a few minutes the first
time. `omac vm status` shows whether it runs, its mode (should be `offline`),
its images and JDKs. `status` works only on the host, never in a session.

## 5. Import the Docker images the tests need

The VM cannot pull images, so the human imports each one:

```sh
omac vm image add postgres:16-alpine
```

You can prepare that list. Search the project for image names, for example
`DockerImageName.parse(`, `new PostgreSQLContainer(`, `GenericContainer(`,
`image:` lines in docker-compose files and image strings in test
configuration. Then give the human one `omac vm image add` line per image,
with the full name and tag. Testcontainers also starts its own helper image
`testcontainers/ryuk:<version>`; the version depends on the Testcontainers
library, and a failed test names the exact tag. Images that need a registry
login work if `docker login` was done on the host.

## 6. Prime the build caches

Builds run with `--offline` in the VM, so their dependencies must be
downloaded once while the VM is online. No session may be attached, because
omac refuses `omac vm online` while one is.

```sh
omac vm online
omac vm exec --env MY_REPO_TOKEN --cwd backend -- ./gradlew check --no-daemon
omac vm offline
```

- Run it from the project directory. `--cwd` is the module with the build,
  relative to the current directory.
- Use the task that runs all checks (`check` or the project's own), not only
  `test`: Gradle downloads the tools behind tasks such as Checkstyle or
  Spotless only when those tasks run.
- `--env NAME` passes a token from the host's shell. Leave it out if the
  downloads need none. Use a short-lived token, because anything a session
  left behind in the VM runs online with it. `--no-daemon` makes sure no
  Gradle daemon keeps the token afterwards.

## 7. Sandbox grants

The session must be able to read `~/.gitconfig`, otherwise `omac vm exec`
fails at `git rev-parse`. The default grants allow it. If the human edited the
sandbox grants file `~/.config/omac/sandbox-profiles/default.json`,
`filesystem.read` must list `~/.gitconfig`, its symlink target if it is a
symlink, and every file it `[include]`s.

## 8. Start a session with the VM

```sh
omac start --vm
```

`omac continue`, `omac resume` and `omac serve` accept `--vm` as well. omac
refuses `--vm` while the VM is online, with `--no-sandbox`, and with launcher
profiles that use a sandbox other than omac's own. Once the human has started
the session, offer to check the connection with `omac vm exec -- true`.

## 9. Day-to-day

- New dependency in the build: end the session and prime again (step 6).
- New image: `omac vm image add <image>`; this works while sessions run.
- New JDK or package: add it to `vm.yaml`, end the session, run
  `omac vm up --reprovision`.
- Free the resources: `omac vm down` stops the VM and keeps everything;
  `omac vm up` starts it again.
- Start over: `omac vm reset` deletes the VM with its images, keys, copied
  projects and caches, then creates it anew. Import images and prime again.
- `omac vm online`, `omac vm up --reprovision` and `omac vm reset` are
  refused while a session is attached.
