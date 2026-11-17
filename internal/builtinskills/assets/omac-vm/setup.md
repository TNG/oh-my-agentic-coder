# Setting up the omac VM

Use this guide to walk a human through setting up the omac VM. The human runs
every command below in a terminal on their computer (the host), outside any
omac session. You never run host steps yourself; give them the exact commands
and explain what each one does.

## 1. What the VM is for

The omac VM is a Linux virtual machine that stays around between sessions.
It runs Docker and builds that the sandbox cannot run, for example
Testcontainers tests. It has no internet access by default, so the human
brings in Docker images and build dependencies. A session started with `--vm`
runs Docker and builds there through `omac vm exec`; the sandbox itself has
no Docker.

## 2. Prerequisites

- **Lima**, which runs the VM. omac is tested with Lima 2.1. On macOS:
  `brew install lima`. On Linux: the release tarball from the Lima GitHub
  releases or Homebrew on Linux, plus QEMU. WSL2 counts as Linux; QEMU is
  very slow there unless `/dev/kvm` exists (nested virtualization).
- **rsync** on the host, which copies the project into the VM. macOS ships
  one; on Linux install the `rsync` package.
- **Resources**: by default the VM takes half the host's CPU cores (at least
  4), a quarter of its RAM (at least 8 GiB, at most 12 GiB) and 60 GiB of disk.

## 3. Optional: `~/.config/omac/vm.yaml`

Only needed to change the size or to install Java or extra packages. Every
field is optional. Example for a Java 21 project:

```yaml
cpus: 6
memory: 12GiB
jdks: [temurin-21]
packages: [jq]
```

`jdks` takes `temurin-<version>`; `packages` are Ubuntu package names. A
`setup` field holds a shell script for anything else, for example
`npm install --global pnpm`. It runs as root with network access at
provisioning (the first `omac vm up` and every `--reprovision`), so it must be
safe to run again.

Projects that must not see each other belong in separate VMs. Each extra VM
has its own name and its own file `~/.config/omac/vms/<name>.yaml`; the human
adds `--name <name>` to every `omac vm` command below and starts sessions with
`omac start --vm=<name>`. Without a name, everything uses the default VM `omac`,
except that `omac start --vm` and `omac vm exec` first look up the project in
`~/.config/omac/vm-projects.yaml`.

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
configuration. Then give the human one `omac vm image add <image>` line per
image, with the full name and tag. Testcontainers also starts its own helper
image `testcontainers/ryuk:<version>`; the version depends on the
Testcontainers library, and a failed test names the exact tag. Images that
need a registry login work if `docker login` was done on the host. The host
pulls with that login, and once an image is in the VM the agent can read all
of its files. So the human imports a private image only if the project's
tests use it.

## 6. Prime the build caches

The VM has no network access, so everything a build downloads must be
downloaded once while the VM is online. No session may be attached, because
omac refuses `omac vm online` while one is.

The priming build runs the VM's copy of the project online and with the
token. That copy can hold files an agent wrote only in the VM, or deleted on
the host after they were copied, so `git status` on the host does not show
everything. Prime from a fresh clone of a commit the human trusts, in a
new folder. That commit must contain the build change that needs the new
dependency, so the human reviews the change, commits it, for example on a
branch, and clones that commit.

The fresh clone makes sure the host side holds only reviewed files. It does
not clean the VM side: a session can create the VM's copy for any folder in
advance, and can leave files in the VM user's home, such as a Gradle init
script or a service that starts with the VM. All of these run while the VM
is online, and only `omac vm reset` removes them, copies included. If the
human does not trust every earlier session, they run `omac vm reset` first
and import the images again.

```sh
omac vm online
omac vm exec --env MY_REPO_TOKEN --cwd backend -- ./gradlew check --no-daemon
omac vm offline
```

For Maven, the middle line is
`omac vm exec --env MY_REPO_TOKEN --cwd backend -- ./mvnw verify`.

- If the build fails, still run `omac vm offline`: omac refuses `--vm`
  sessions while the VM is online.
- Run it from the fresh clone. `--cwd` is the root of the build (the
  directory with `gradlew`, `settings.gradle` or `settings.gradle.kts`, or
  with the top-level `pom.xml`), relative to the current directory.
- Run all checks, not only the tests (`check` for Gradle, `verify` for
  Maven), because some tools, such as Checkstyle or Spotless, are downloaded
  only when the build step that uses them runs.
- `--env NAME` passes a token from the host's shell. The human exports `NAME`
  in that shell first; otherwise omac stops with `environment variable NAME
  is not set`. Never look for the token or its value yourself. Leave `--env`
  out if the downloads need no token. Use a short-lived, read-only token,
  because anything a session left behind in the VM runs online with it and
  can store the token in the VM, where later sessions can read it; revoke it
  after priming. With Gradle, `--no-daemon` makes sure no daemon keeps the
  token afterwards.
- With `--env`, omac names the variables the build can read and waits: the
  human presses Enter on an empty line to continue, or Ctrl-C to stop.
  In some shells, pasting the three lines at once gives `omac vm offline`
  to that prompt as its answer: the build stops and the VM stays online.
  So the human runs the lines one at a time, and runs `omac vm offline` if
  the build stopped.
- Gradle: if the build reads its token from a Gradle property, for example
  `gpr.key` in `~/.gradle/gradle.properties`, do not reuse that stored token:
  it is usually long-lived. The human creates a short-lived read-only token,
  exports it as `TOKEN` and hands it to Gradle under the property's name.
  Gradle reads a property `foo` also from the environment variable
  `ORG_GRADLE_PROJECT_foo`. The wrapper runs under `bash` because the VM's
  `sh` drops variables with a dot in their name:

  ```sh
  omac vm exec --env TOKEN --cwd backend -- sh -c 'env "ORG_GRADLE_PROJECT_gpr.key=$TOKEN" bash ./gradlew check --no-daemon'
  ```

- Maven reads repository credentials only from a `<server>` entry in a
  settings file, not from the environment, and sends them only to a mirror
  or repository with the same `<id>`. Priming runs from a fresh clone, so
  the human commits two files to the project. `.mvn/settings.xml`, with the
  URL of a registry that also proxies Maven Central, such as a Nexus or
  Artifactory group, and the user name it expects:

  ```xml
  <settings>
    <mirrors>
      <mirror>
        <id>my-repo</id>
        <url>https://repo.example.com/maven</url>
        <mirrorOf>*</mirrorOf>
      </mirror>
    </mirrors>
    <servers>
      <server>
        <id>my-repo</id>
        <username>my-user</username>
        <password>${env.MY_REPO_TOKEN}</password>
      </server>
    </servers>
  </settings>
  ```

  `.mvn/maven.config`, with exactly this line (Maven 3.9 or later reads
  each line as one argument):

  ```text
  --settings=${maven.multiModuleProjectDirectory}/.mvn/settings.xml
  ```

  With `maven.config`, priming and every later offline build read the same
  settings: Maven records which repository each cached artifact came from,
  and a build without the settings' mirror fails offline although the cache
  is complete. This applies to every Maven run of the project, also outside
  the VM: the project's settings file replaces `~/.m2/settings.xml`.
  Every online Maven run of the project then needs `MY_REPO_TOKEN` in its
  environment, on developer machines and in CI too. Offline, the unset
  `${env.MY_REPO_TOKEN}` does no harm. Never write the token itself into a
  file: the VM keeps it.
- Priming has to end with exit code 0 and `BUILD SUCCESSFUL` (Gradle) or
  `BUILD SUCCESS` (Maven). Gradle, for example, stops at the first failed
  task and skips the downloads of the tasks after it, so offline builds in a
  session can then miss dependencies.

## 7. Sandbox grants

The session must be able to read `~/.gitconfig` and every file it includes
(`[include]`, `[includeIf]`), otherwise `omac vm exec` fails at
`git rev-parse` with an error that names the file. The default grants cover
`~/.gitconfig` only. If it includes other files, the human adds them to
`filesystem.read` in the sandbox grants file
`~/.config/omac/sandbox-profiles/default.json`. The
agent can read every granted file, so tokens must be moved out of them
first, for example a token in a `url.<base>.insteadOf` URL, an
`http.<url>.extraHeader` or an inline credential helper.

## 8. Start a session with the VM

```sh
omac start --vm
```

`omac continue`, `omac resume` and `omac serve` accept `--vm` as well. A bare
`--vm` uses the VM that `~/.config/omac/vm-projects.yaml` assigns to the
project (a line such as `/home/alex/work/acme: acme`), else `omac`;
`--vm=<name>` picks one. omac refuses `--vm` while the VM is online, with
`--no-sandbox`, with `omac serve --learn` or a launcher profile that runs
the sandbox with `--learn`, with `omac serve --no-inner` or
`omac serve --for-opencode-desktop`, and with launcher profiles that use a
sandbox other than omac's own. Once the human has started the session,
offer to check the connection with `omac vm exec -- true`.

## 9. Day-to-day

- New dependency in the build: end the session and prime again, see
  "Prime the build caches" above.
- New image: `omac vm image add <image>`; this works while sessions run.
- New JDK or package: add it to `vm.yaml`, end the session, run
  `omac vm up --reprovision`.
- Free the resources: `omac vm down` stops the VM and keeps everything;
  `omac vm up` starts it again.
- Start over: `omac vm reset` deletes the VM with its images, keys, copied
  projects and caches, then creates it anew. Import images and prime again.
- `omac vm online`, `omac vm up --reprovision` and `omac vm reset` are
  refused while a session is attached.
