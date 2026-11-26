# sandbox-launch

<!--
Baseline: openspec/specs/ has no sandbox-launch spec yet. The requirement
below is ADDED next to those in
openspec/changes/native-sandbox/specs/sandbox-launch/spec.md, so archive
native-sandbox before archiving this change.
-->

## ADDED Requirements

### Requirement: Per-session OpenCode PTY socket folder
When omac launches the OpenCode harness through the built-in sandbox (`omac sandbox run`), with `omac start` or `omac serve`, on macOS or Linux and for every OpenCode version, it SHALL create a new folder for this launch directly under `/tmp`, readable and writable by the current user only. It SHALL export `OPENCODE_PTY_SOCKET_DIR=<folder>` to the harness, replacing any value inherited from the host, and SHALL grant the sandbox read, write and AF_UNIX connect on that folder, equivalent to `--allow-unix-dir <folder>`. omac SHALL remove the folder when the launch ends.

The folder name SHALL be short enough that `<folder>/opencode-pty-<uid>/<32 hex characters>.sock` stays below the macOS Unix-socket path limit of 104 bytes for a 10-digit uid, measured on the resolved path (`/private/tmp/…`).

If the effective profile's `environment.deny_vars` matches `OPENCODE_PTY_SOCKET_DIR`, omac SHALL refuse the launch with an error that names the variable.

omac SHALL NOT create the folder or set the variable for other harnesses, with `--no-sandbox`, or with a launcher that does not use the built-in sandbox.

#### Scenario: Shell calls reach the session's own helper
- **WHEN** `omac start opencode` runs OpenCode 2.3.0 or later in the built-in sandbox on macOS and the agent runs a shell command
- **THEN** OpenCode's `opencode-pty` helper listens on a socket under the session's folder, OpenCode connects to it, and the command returns without the helper-start retries (no `process daemon unavailable` warning in OpenCode's log)

#### Scenario: Serve mode
- **WHEN** `omac serve opencode` launches OpenCode 2.3.0 or later in the built-in sandbox and a client sends a shell command through `POST /api/shell`
- **THEN** the command returns within a few seconds, and the helper's socket is under a folder of that launch

#### Scenario: Host value is replaced
- **WHEN** the host environment has `OPENCODE_PTY_SOCKET_DIR=/somewhere/else` and `omac start opencode` is launched in the built-in sandbox
- **THEN** the harness sees `OPENCODE_PTY_SOCKET_DIR` set to the session's folder

#### Scenario: Folder is removed
- **WHEN** the session ends normally
- **THEN** the session's folder under `/tmp` no longer exists

#### Scenario: Folder cannot be created
- **WHEN** the user who starts omac cannot create a folder under `/tmp`
- **THEN** omac prints a warning and launches the harness without the folder, the grant and the variable

#### Scenario: Profile blocks the variable
- **WHEN** the effective profile has `environment.deny_vars: ["OPENCODE_*"]` and `omac start opencode` is launched in the built-in sandbox
- **THEN** omac exits with a configuration error that names `OPENCODE_PTY_SOCKET_DIR`, before the harness starts

#### Scenario: Other harnesses are unchanged
- **WHEN** `omac start claude` is launched
- **THEN** no PTY socket folder is created and `OPENCODE_PTY_SOCKET_DIR` is not set in the sandbox

#### Scenario: No sandbox
- **WHEN** `omac start opencode --no-sandbox` is launched
- **THEN** no PTY socket folder is created and omac does not set or change `OPENCODE_PTY_SOCKET_DIR`

#### Scenario: nono backend
- **WHEN** `omac start opencode` is launched with a nono launcher profile
- **THEN** no PTY socket folder is created and omac does not set or change `OPENCODE_PTY_SOCKET_DIR`
