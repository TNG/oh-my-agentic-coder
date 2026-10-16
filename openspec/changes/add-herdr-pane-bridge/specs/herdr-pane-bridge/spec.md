# herdr-pane-bridge

Host-side integration between omac and the herdr terminal session manager: pane lifecycle reports, working/idle state, and sandboxed session restore.

## ADDED Requirements

### Requirement: Bridge activates only inside herdr panes
omac SHALL detect the herdr pane environment (`HERDR_ENV=1` plus a valid `HERDR_PANE_ID` and an absolute `HERDR_BIN_PATH`) before any bridge activity. When detection fails — including values that could be confused with CLI flags or point at a non-herdr binary — the bridge SHALL stay off and the launch SHALL proceed identically to a non-herdr environment.

#### Scenario: Launch outside herdr
- **WHEN** `omac start` runs without herdr pane variables
- **THEN** no herdr CLI call is made and the launch is unaffected

#### Scenario: Malformed pane variables rejected
- **WHEN** `HERDR_PANE_ID` is flag-like, overlong, or contains control characters, or `HERDR_BIN_PATH` is relative
- **THEN** the bridge stays off

### Requirement: Bridge runs host-side, never in the sandbox
The bridge SHALL run in the omac process, outside the sandbox. No `HERDR_*` variable and no herdr socket SHALL be passed into the sandboxed process: the herdr pane API can drive other panes, so it must stay outside the sandboxed agent's reach. The bridge SHALL be the sole reporter of omac's state to herdr.

#### Scenario: Sandboxed process cannot reach herdr
- **WHEN** the inner harness enumerates its environment or probes for the herdr socket
- **THEN** no herdr variables or socket are present, exactly as in a launch without the bridge

### Requirement: Pane lifecycle reports
While a launch runs in a herdr pane, the bridge SHALL claim the pane via `pane report-agent` with source `omac`, agent label `omac`, a monotonically increasing `--seq` (timestamp-derived, with a rewound-clock guard), and a state of `working` or `idle`. It SHALL never report `blocked`: omac has no precise host-side signal for a harness permission prompt, and herdr's screen-based detection covers that case better than a file heuristic. When the inner command exits, the bridge SHALL release the pane via `pane release-agent`. All herdr calls SHALL be fail-soft: backgrounded, bounded by a short timeout, output discarded, errors ignored, and never able to affect the launch or its exit code. Identical reports (same state and session id) SHALL be coalesced rather than resent.

#### Scenario: Claim on launch
- **WHEN** `omac start` runs inside a herdr pane
- **THEN** the pane is reported as agent `omac` with state `working`

#### Scenario: Release on exit
- **WHEN** the inner harness exits and omac shuts down
- **THEN** the pane is released exactly once, and a second close call is a no-op

#### Scenario: Hung herdr binary cannot stall the launch
- **WHEN** the herdr CLI hangs or is missing
- **THEN** each bridge call still returns within its timeout and the launch proceeds and exits with the inner command's code

### Requirement: Working/idle state from store activity
The bridge SHALL derive the state from host-side session-store activity probed by stat calls only (no parsing): for opencode, the session db and its `-wal`/`-shm` sidecars; for claude-code, the per-project session directory with per-entry stamps. A launch or any store delta SHALL yield `working`; a `working` session whose store has been unchanged for a generous quiet period (default 60 s, so long builds do not flap the state) SHALL yield `idle`.

#### Scenario: Streaming turn stays working
- **WHEN** the harness streams a turn and keeps writing session state
- **THEN** the pane is reported `working` for the duration

#### Scenario: Quiet session goes idle
- **WHEN** the harness sits at its prompt and its store is unchanged past the quiet period
- **THEN** the pane is reported `idle`

### Requirement: Sandboxed session restore
Once the launch's session id is known — from an explicit resume (`omac continue -s <id>`), from opencode's control-plane session report, or from a newly discovered claude session file — the bridge SHALL attach it and a resume command to its report. The resume command SHALL be `omac continue [harness] -s <id>` so a herdr restore re-enters through the sandboxed launch path with launch-time checks (including the skill-bundle drift check) re-applied. Session ids SHALL be validated before embedding: omac's session id charset, no leading punctuation (would read as a flag), and claude `sidechain-` ids (sub-agent sessions, not user-resumable) rejected. The report attaching a resume command SHALL follow the pane claim (herdr only accepts resume argv from a source that holds the pane).

#### Scenario: Restore after herdr server restart
- **WHEN** a herdr server restart recovers the pane and runs the reported resume command
- **THEN** the session re-opens via `omac continue`, inside the sandbox, in the pane's working directory

#### Scenario: Drift check fails at restore
- **WHEN** the skill bundle hash changed since the original session and `omac continue` re-validates at restore time
- **THEN** the restore fails with omac's normal error rather than silently degrading

#### Scenario: Invalid id never embedded
- **WHEN** a discovered candidate session id fails validation (flag-like, non-charset, or `sidechain-`)
- **THEN** no report attaches it as the agent session id or embeds it in a resume command

### Requirement: Global-only enable switch
The bridge SHALL be enabled by default and deactivate when the global launcher config sets `herdr.enabled: false`. The setting SHALL be stripped from project-local launcher configs (the bridge runs on the host with the user's environment, so a project must not control it), the same rule as `sandbox.*`. `omac doctor` SHALL report whether the bridge is enabled and whether omac is currently inside a herdr pane.

#### Scenario: Opt-out honored
- **WHEN** the global config sets `herdr.enabled: false` and `omac start` runs inside a herdr pane
- **THEN** no herdr call is made

#### Scenario: Project cannot re-enable
- **WHEN** a project-local config sets `herdr.enabled: true` while the global config disables it
- **THEN** the bridge stays off

#### Scenario: Doctor reports bridge status
- **WHEN** `omac doctor` runs
- **THEN** the output contains a herdr bridge line stating enabled/disabled and, when enabled, whether omac is inside a herdr pane
