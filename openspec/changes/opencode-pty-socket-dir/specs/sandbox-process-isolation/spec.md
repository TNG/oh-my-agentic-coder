# sandbox-process-isolation

<!--
Baseline: openspec/specs/ has no sandbox-process-isolation spec yet. The
requirement below is ADDED next to "Protected-path denials override grants"
in openspec/changes/native-sandbox/specs/sandbox-process-isolation/spec.md,
so archive native-sandbox before archiving this change.

On macOS the socket-connect scenario builds on pull request #513 (merged),
which makes protected paths deny Unix-socket connects.
-->

## ADDED Requirements

### Requirement: Shared OpenCode PTY socket folder is protected
The built-in protected paths on macOS and Linux SHALL include `/tmp/opencode-pty-<uid>`, where `<uid>` is the uid of the user who launches omac. The entry SHALL apply to every harness. Like every protected path it SHALL stay closed when a profile grant covers it, including a `filesystem.allow_unix_dir` grant, and SHALL be removable only through `filesystem.override_deny`. A profile grant that covers it SHALL be reported by the profile audit with high severity.

On Linux the folder is hidden only if it exists when the session starts, as for every protected path.

#### Scenario: Workaround grant no longer opens the helpers
- **WHEN** a profile has `filesystem.allow_unix_dir: ["/private/tmp/opencode-pty-<uid>"]` and a process inside the session connects to a socket in that folder that an OpenCode outside the sandbox listens on
- **THEN** the connection is denied with EPERM on macOS

#### Scenario: Broad grant on /tmp
- **WHEN** a profile grants `/tmp` through `filesystem.allow_unix_dir` and a process inside the session connects to a socket under `/tmp/opencode-pty-<uid>`
- **THEN** the connection is denied on macOS, and on Linux the folder appears empty when it existed at launch

#### Scenario: Audit reports the grant
- **WHEN** `omac doctor` checks a profile whose `filesystem.allow_unix_dir` covers `/tmp/opencode-pty-<uid>`
- **THEN** it reports a high-severity finding that names the grant

#### Scenario: Session's own folder is unaffected
- **WHEN** `omac start opencode` runs in the built-in sandbox
- **THEN** the helper's socket under the per-session folder `/tmp/omac-pty-<random>/opencode-pty-<uid>` is reachable
