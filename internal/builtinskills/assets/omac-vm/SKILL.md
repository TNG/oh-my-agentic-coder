---
name: omac-vm
description: >-
  Run builds, tests and Docker in the omac VM from an omac session started
  with `--vm` (OMAC_VM_SSH_PORT is set): the exact `omac vm exec` commands,
  how to read results that stay in the VM, and what to ask the human for when
  an image or a dependency is missing. Use this when the project needs Docker,
  Testcontainers, a JDK or a build (Gradle, Maven, ...) that the sandbox
  cannot run, also in a session without `--vm`; when the user asks how to set
  up or start using the omac VM; or when the user mentions the omac VM.
license: Same as the omac repository
compatibility: >-
  Pure guidance skill: no sidecar and no omac.yaml. Needs an omac session
  started with `--vm`. Works under any omac inner harness. Shipped with the
  omac binary and provisioned by `omac setup`.
metadata:
  author: tngtech
  version: "0.1.0"
  omac-builtin: "true"
---

# omac-vm

The omac VM is a long-lived Linux VM on the human's computer. In a session
started with `--vm`, `omac vm exec` runs a command in the VM on a copy of the
worktree. Docker exists only there: run `omac vm exec -- docker ps`, not
`docker ps`. The VM has no network access. Only the human can import Docker
images into it and fill its build caches ("priming"), from the host; you
cannot do either.

## Which VM, and which ports

The human may run several VMs. Your session is attached to one, named in
`OMAC_VM_NAME`; `omac vm exec` uses it without `--name`. In the `omac vm`
commands you hand to the human for the host (`image add`, `online`, `offline`,
`up --reprovision`), add `--name <the value of OMAC_VM_NAME>` unless it is
`omac`.

Containers in the VM are reachable from the host only on published ports in
the range `OMAC_VM_PORT_FIRST` to `OMAC_VM_PORT_LAST`. Read both variables and
publish inside that range, for example
`omac vm exec -- docker run -d -p <port>:5432 postgres:16` with a `<port>` from
it. Other ports work only inside the VM.

If the project belongs in a different VM, ask the human to start a new session
with `omac start --vm=<name>`, or to assign the project in
`~/.config/omac/vm-projects.yaml`. You cannot switch VMs inside a session.

## Setting up the VM for the first time

If the human has no VM yet, or this session was started without `--vm`
(`OMAC_VM_SSH_PORT` is not set), read [setup.md](setup.md) and walk the
human through it. They run every step on the host, not you.

## Check that the VM is reachable

```sh
omac vm exec -- true
```

The first run copies the whole worktree and can take a while. Do not use
`omac vm status`: it needs files the sandbox blocks, so it always fails
inside a session. If `omac vm exec` says to start the session with `--vm`,
that the VM is not created, or that the connection failed, stop and ask the
human to start the session with `--vm` or the VM with `omac vm up`. Never
run `omac vm up` yourself.

## Run a build

```sh
omac vm exec --cwd <module> -- <build command> > "${TMPDIR:-/tmp}/vm-test.log" 2>&1; echo "exit=$?"
```

`<module>` is the directory with the build, relative to your current
directory, for example `backend`. Typical build commands, with the tool's
offline switch:

- Gradle: `./gradlew test --offline`. Without `--offline`, Gradle checks
  version ranges such as `[1.4,2.0)` online again after 24 hours and fails
  in the VM even though the cache is complete.
- Maven: `./mvnw -o test` (or `mvn -o test`).

The VM has no network access: everything a build downloads must already be
in its cache. Keep the tool's offline switch if it has one.

- Each `omac vm exec` first copies the worktree into the VM, so your edits are
  there. `--cwd` is relative to your current directory.
- The copy has no `.git`, no `.env` files and nothing git ignores, so build
  steps that call git fail in the VM. Files you delete are not deleted in the
  VM; remove a stale one with `omac vm exec -- rm -rf <path>`.
- Redirect output to a file and read the file, as above. Do not pipe into
  `head` or `tail`: the pipe reports the exit code of its last command, so a
  failed build looks successful.
- A non-zero exit code can come from the command or from omac (2, 4 or 5),
  so read the log. omac's own errors are lines starting with `omac vm`.
  `connection lost or remote command killed` can also mean the build was
  killed in the VM, for example when it ran out of memory.
- omac quotes every argument after `--`, so `*` and `|` are not expanded in
  the VM. Wrap such a command in `sh -c '...'`, as in the examples below.
- Run Docker commands and tests that use Testcontainers through
  `omac vm exec`; the sandbox has no Docker. Paths in `docker run -v` and
  `docker build` refer to the VM, so use paths inside the worktree copy, for
  example `omac vm exec -- docker build -t app .`.
- Keep runs short: run focused tests, do not run `clean`, and keep build
  daemons running between runs. With Gradle: `--tests 'com.example.FooTest'`;
  with Maven: `-Dtest=FooTest`.

## Read the results

Build output stays in the VM and is not copied back. Read it with another
`omac vm exec`. For example, after a Gradle test run in `backend/`:

```sh
# number of test cases that ran
omac vm exec --cwd backend -- sh -c 'cat build/test-results/test/*.xml | grep -c "<testcase "'
# result files that contain a failure or an error
omac vm exec --cwd backend -- sh -c 'grep -l -e "<failure" -e "<error" build/test-results/test/*.xml || echo "no failures"'
```

## When something is missing, ask the human

- **An image is missing.** Docker or Testcontainers reports `Unable to find
  image` or a failed pull. Name the full image reference, for example
  `postgres:16-alpine`, and ask the human to run
  `omac vm image add <image>` on the host.
- **Dependencies do not resolve offline.** The build reports a missing
  artifact or an unknown host, for example Gradle's `Could not resolve` or
  `No cached version ... available for offline mode`, or Maven's `Cannot
  access ... in offline mode`. A build wrapper that fails to download its own
  distribution (`UnknownHostException: services.gradle.org`) means the VM was
  never primed, for example after `omac vm reset`. Do not drop the offline
  switch. Ask the human to prime the VM from the host: run the build once
  while the VM is online. omac refuses `omac vm online` while a session is
  attached, so they end this session first:

  ```sh
  omac vm online
  omac vm exec --env <TOKEN_VARIABLE> --cwd <module> -- <full build command>
  omac vm offline
  ```

  `<full build command>` runs all checks, not only the tests, because some
  tools are downloaded only when their task runs. Examples: Gradle
  `./gradlew check --no-daemon`, Maven `./mvnw verify`. Leave out `--env` if
  the downloads need no token; otherwise the human exports it in that shell
  first. Never look for the token yourself. Gradle's `--no-daemon` is only
  for priming: a daemon would keep the token. Priming has to end with a
  successful build.
- **A JDK or system package is missing.** The build reports a missing Java
  installation (Gradle: `Cannot find a Java installation matching ...`), or
  a command is not found. Name it, for example
  `temurin-21`, and ask the human to add it to `jdks` or `packages` in
  `~/.config/omac/vm.yaml` (`~/.config/omac/vms/<name>.yaml` for a named VM),
  end this session and run `omac vm up --reprovision` on the host. For a tool
  that is no package, such as a global Node package, they can add an install
  script as `setup` in the same file. It runs as root, with network, at
  provisioning, so it has to be safe to run again. Never write it yourself into
  that file; you cannot, and it is the human's decision.
- **`omac vm exec` fails at `git rev-parse` with an error about
  `~/.gitconfig`.** The session cannot read `~/.gitconfig`, so every git
  command in it fails. Ask the human to add `~/.gitconfig`, its symlink target
  if it is a symlink, and every file it `[include]`s to `filesystem.read` in
  the sandbox grants file `~/.config/omac/sandbox-profiles/default.json`, then
  restart the session.
