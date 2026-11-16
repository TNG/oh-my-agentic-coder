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
  Pure guidance skill: no sidecar and no omac.yaml. The build commands need
  an omac session started with `--vm`; setup.md covers sessions without it.
  Works under any omac inner harness. Shipped with the omac binary and
  installed by `omac start` and `omac setup`.
metadata:
  author: tngtech
  version: "0.1.0"
  omac-builtin: "true"
---

# omac-vm

The omac VM is one long-lived Linux VM on the human's computer. In a session
started with `--vm`, `omac vm exec` runs a command in the VM on a copy of the
worktree. Docker exists only there: run `omac vm exec -- docker ps`, not
`docker ps`. The VM has no network access. Only the human can import Docker
images into it and fill its build caches ("priming"), from the host; you
cannot do either.

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
  there. `--cwd` is relative to your current directory. In a directory in
  which git tracks no file yet, such as a new module, omac refuses to copy
  with `git in <top> tracks nothing in <dir>`. If `<top>` is your own
  worktree, rerun from it with `--cwd <dir>`. Otherwise your worktree lost
  its `.git` file: stop and ask the human to restore it.
- The copy has no `.git`, no `.env` files and nothing git ignores, so build
  steps that call git fail in the VM. Files you delete are not deleted in the
  VM; remove a stale one with `omac vm exec -- rm -rf <path>`.
- Redirect output to a file and read the file, as above. Do not pipe into
  `head` or `tail`: the pipe reports the exit code of its last command, so a
  failed build looks successful.
- A non-zero exit code can come from the command or from omac (2, 4 or 5),
  so read the log. omac's own errors name `omac vm` or `--vm`.
  `connection lost or remote command killed` can also mean the build was
  killed in the VM, for example when it ran out of memory. Its hint to run
  `omac vm status` is for the human on the host.
- omac quotes every argument after `--`, so `*` and `|` are not expanded in
  the VM. Wrap such a command in `sh -c '...'`, as in the examples below.
- Run Docker commands and tests that use Testcontainers through
  `omac vm exec`; the sandbox has no Docker. Paths in `docker run -v` and
  `docker build` refer to the VM, so use paths inside the worktree copy:
  `omac vm exec -- docker build -t app .`, or
  `omac vm exec -- sh -c 'docker run --rm -v "$PWD":/src -w /src <image> ls'`,
  where `$PWD` is the directory the command runs in: the place in the copy
  that matches your current directory or `--cwd`.
- Keep runs short: run focused tests, do not run `clean`, and keep build
  daemons running between runs. With Gradle: `--tests 'com.example.FooTest'`;
  with Maven: `-Dtest=FooTest`.

## Read the results

Build output stays in the VM and is not copied back. Read it with another
`omac vm exec`. Result files from an earlier run stay in the VM too, so
read them only after a build whose log shows that the test task ran. For
example, after a Gradle test run in `backend/`:

```sh
# number of test cases that ran
omac vm exec --cwd backend -- sh -c 'cat build/test-results/test/*.xml | grep -c "<testcase "'
# result files that contain a failure or an error
omac vm exec --cwd backend -- sh -c 'grep -l -e "<failure" -e "<error" build/test-results/test/*.xml; [ $? -eq 1 ] && echo "no failures"'
```

If no result file exists, the tests did not run: read the build log
instead.

## When something is missing, ask the human

- **An image is missing.** Docker or Testcontainers reports `Unable to find
  image` or a failed pull. Name the full image reference, for example
  `postgres:16-alpine`, and ask the human to run
  `omac vm image add <image>` on the host.
- **Dependencies do not resolve offline.** The build reports a missing
  artifact or an unknown host, for example Gradle's `Could not resolve` or
  `No cached version ... available for offline mode`, or Maven's `Cannot
  access ... in offline mode`. Do not drop the offline switch. Name the
  dependency and the build file you changed, and ask the human to prime the
  VM from the host, as described in "Prime the build caches" in
  [setup.md](setup.md). They prime from a fresh clone, in a new folder, of a
  commit with your change, and omac refuses `omac vm online` while a session
  is attached, so they end this session first. Never look for the registry
  token the build needs yourself.
- **The build wrapper cannot download its build tool.** The wrapper fails to
  download its own distribution before the build starts, for example with
  `UnknownHostException` for the host in Gradle's `distributionUrl` (usually
  `services.gradle.org`): the VM was not primed for this project's build
  tool version, for example after `omac vm reset` or a wrapper upgrade. Name
  the version from the wrapper's properties file, such as `distributionUrl`
  in `gradle/wrapper/gradle-wrapper.properties`, and ask the human to prime
  the VM as described in [setup.md](setup.md); they end this session first.
- **A JDK or system package is missing.** The build reports a missing Java
  installation (Gradle: `No matching toolchains found` or `Cannot find a Java
  installation`), or a command is not found. Name it, for example
  `temurin-21`, and ask the human to add it to `jdks` or `packages` in
  `~/.config/omac/vm.yaml`, end this session and run
  `omac vm up --reprovision` on the host.
- **`omac vm exec` fails at `git rev-parse` with an error about
  `~/.gitconfig` or a file it includes.** The session cannot read that
  file, so every git command in it fails. Ask the human to grant it as
  described in "Sandbox grants" in [setup.md](setup.md), then restart the
  session.
