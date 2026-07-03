# audit-trail Specification

## Purpose
TBD - created by archiving change add-audit-trail. Update Purpose after archive.
## Requirements
### Requirement: Central append-only audit log

omac SHALL maintain a single, append-only audit stream, written as JSON Lines (one JSON object terminated by `\n` per event), capturing every security-relevant action it performs across `start` and `serve` modes.

#### Scenario: Log file is created on session start

- **WHEN** omac starts a session with auditing enabled
- **THEN** an audit log file is opened at the configured path (default: the persistent central path)
- **AND** the file is opened append-only with mode `0600`
- **AND** a `session.start` event is written and flushed before the inner sandboxed command is launched

#### Scenario: Each event is a single valid JSON line

- **WHEN** any audit event is emitted
- **THEN** exactly one line is appended to the log
- **AND** that line parses as a standalone JSON object
- **AND** no prior line is modified

### Requirement: Common event envelope

Every audit event SHALL include a common envelope: `ts` (RFC3339Nano UTC), `run_id` (a per-invocation random hex identifier), `seq` (a monotonically increasing per-run unsigned integer), `type` (a dotted event type), `mode` (`start` or `serve`), and `pid`.

#### Scenario: Envelope fields present on every event

- **WHEN** any event of any type is emitted
- **THEN** the serialized object contains non-empty `ts`, `run_id`, `type`, `mode`, and `pid`
- **AND** contains a `seq` integer

#### Scenario: Sequence numbers are monotonic and gap-detectable within a run

- **WHEN** N events are emitted during a single omac run
- **THEN** their `seq` values are strictly increasing by 1 starting from the first emitted event
- **AND** all events in the run share the same `run_id`

### Requirement: Log command and process executions

omac SHALL record a `process.exec` event when it spawns the sandboxed inner command or any sidecar process, and a `process.exit` event when that process terminates.

#### Scenario: Sidecar spawn is logged

- **WHEN** the supervisor spawns a sidecar process
- **THEN** a `process.exec` event records the argv (redacted), the working directory, the skill name, the namespace (hashed), and the names (not values) of injected secrets

#### Scenario: Inner command spawn is logged

- **WHEN** the sandbox launcher execs the inner harness command
- **THEN** a `process.exec` event records the argv (redacted), the resolved sandbox profile name, and whether the run is sandboxed or `--no-sandbox`

#### Scenario: Process exit is logged with code and duration

- **WHEN** a spawned process terminates
- **THEN** a `process.exit` event records the same process identity, its exit code, and its wall-clock duration

### Requirement: Log outbound network decisions

omac SHALL record a `net.decision` event for every outbound network access decision it makes (allow or deny), including the decision source.

#### Scenario: Interactive prompt decision is logged

- **WHEN** the network prompt returns a decision for a host:port
- **THEN** a `net.decision` event records the host, port, `allow` boolean, `scope` (`once`/`host`/`suffix`), `source` (`prompt`), and whether the decision was persisted

#### Scenario: Learned-policy or list decision is logged

- **WHEN** a network access is decided by the learned-policy store, an allowlist, or a blocklist without prompting
- **THEN** a `net.decision` event records the host, port, `allow` boolean, and `source` (`learned`, `allowlist`, or `blocklist`)

#### Scenario: Timeout default is logged

- **WHEN** a network prompt times out or no prompt backend is available and the `on_unavailable` policy applies
- **THEN** a `net.decision` event records the resulting `allow` boolean and `source` (`timeout` or `unavailable`)

### Requirement: Log facade proxy requests

omac SHALL record a `facade.request` event for every request the facade proxies or answers, superseding the ad-hoc facade access log.

#### Scenario: Proxied request is logged with hashed namespace

- **WHEN** the facade proxies or serves a request for a route
- **THEN** a `facade.request` event records the method, mount, hashed namespace, request path remainder, upstream status, bytes written, and duration

#### Scenario: Secret dir-token namespace is never written verbatim

- **WHEN** a request targets a per-directory namespaced route whose namespace is a secret dir-token
- **THEN** the emitted `facade.request` event contains only a hash of that namespace, never the token itself

### Requirement: Log control-plane mutations

omac SHALL record a `control.mutation` event for every control-plane state change (`activate`, `deactivate`, `reload`, `reload-global`).

#### Scenario: Directory activation is logged

- **WHEN** the control plane handles an `activate`/`deactivate`/`reload` request
- **THEN** a `control.mutation` event records the action, the absolute target directory, and the result (success or error summary)

#### Scenario: Global reload is logged

- **WHEN** the control plane handles a `reload-global` request
- **THEN** a `control.mutation` event records the action `reload-global` and the result

### Requirement: Log secret and config injection

omac SHALL record a `secret.inject` event describing which secret and config *names* were supplied to a sidecar, and a `route.state` event when a route enters a non-ready state (pending-credentials or broken).

#### Scenario: Secret names are logged without values

- **WHEN** a sidecar is brought up with injected secrets
- **THEN** a `secret.inject` event records the skill name and the list of secret and config env-var names
- **AND** the event contains no secret values

#### Scenario: Pending-credentials or broken route is logged

- **WHEN** a route is installed in `pending-credentials` or `broken` state
- **THEN** a `route.state` event records the skill, the state, and the diagnostic detail (with any secret material redacted)

### Requirement: Log session lifecycle

omac SHALL record `session.start` and `session.stop` events bracketing each run.

#### Scenario: Session start records environment

- **WHEN** omac begins a run
- **THEN** a `session.start` event records the omac version, mode, selected sandbox backend/profile, and the active harness

#### Scenario: Session stop is recorded on teardown

- **WHEN** omac tears down at the end of a run
- **THEN** a `session.stop` event is the last record written for that `run_id`

### Requirement: Redaction of secrets and namespace tokens

The audit writer SHALL redact secret material and secret namespace tokens at the single emit boundary, so that no call site can cause them to be written verbatim.

#### Scenario: Known secret value on argv is redacted

- **WHEN** an event's argv contains an element exactly equal to a known injected secret value
- **THEN** that element is replaced with `<redacted>` in the written record

#### Scenario: Namespace is hashed except for non-secret sentinels

- **WHEN** an event carries a namespace field
- **THEN** a random dir-token namespace is written as a truncated hash
- **AND** the non-secret sentinels `__global__` and the empty (flat) namespace are written unchanged

#### Scenario: Secret type is never serialized

- **WHEN** any secret value is included in an event payload by mistake
- **THEN** the redactor prevents the raw value from appearing in the written line

### Requirement: Fail-open writing by default

Unless strict mode is enabled, audit writing SHALL never block or crash a sandboxed run. A sink failure SHALL degrade to a single stderr warning, after which further audit writes for that sink are skipped.

#### Scenario: Sink write failure does not abort the run

- **WHEN** strict mode is off and the audit sink returns a write error
- **THEN** omac emits one warning to stderr
- **AND** the current operation (command spawn, proxy request, etc.) proceeds normally
- **AND** subsequent audit writes to that sink are silently skipped

#### Scenario: Auditing disabled uses a no-op sink

- **WHEN** auditing is disabled via config or `--no-audit`
- **THEN** every emit call is a no-op with no file created and no error

### Requirement: Strict fail-closed mode

omac SHALL provide a strict mode, selectable via the `--audit-strict` flag or `audit.strict` config, under which an unwritable audit log is fatal (fail-closed). Strict mode applies to the file sink (the source of truth).

#### Scenario: Unwritable log aborts startup before the inner command runs

- **WHEN** strict mode is enabled and the audit log cannot be opened or its first event cannot be written
- **THEN** omac aborts with a non-zero I/O error exit code and a clear message
- **AND** the inner sandboxed command is never started

#### Scenario: Mid-run write failure aborts the run

- **WHEN** strict mode is enabled and an audit write fails after the run has started
- **THEN** omac runs its normal teardown (stops sidecars, closes the facade/control plane) and exits non-zero

#### Scenario: Strict combined with disable is a misuse error

- **WHEN** both `--audit-strict` and `--no-audit` are supplied
- **THEN** omac exits with a misuse error and does not start

#### Scenario: Syslog-only failure is not fatal under strict

- **WHEN** strict mode is enabled, the file write succeeds, but the syslog mirror fails
- **THEN** omac emits a warning and continues, because the file already recorded the event

### Requirement: Persistent central log location

The audit log SHALL default to a persistent, central, host-level directory that survives restarts, and SHALL NOT default to the ephemeral per-run runtime directory. Successive runs SHALL append to the same default file, distinguished by `run_id`.

#### Scenario: Default location is a persistent state directory

- **WHEN** no audit path is configured on Linux
- **THEN** the log is written under `$XDG_STATE_HOME/omac/audit/` when set, otherwise `$HOME/.local/state/omac/audit/`
- **AND** on macOS it is written under `$HOME/Library/Logs/omac/audit/`

#### Scenario: Log survives across restarts

- **WHEN** omac runs, exits, and runs again with the default path
- **THEN** the second run appends to the existing log file rather than replacing it
- **AND** events from the two runs carry different `run_id` values

#### Scenario: Default location is outside sandbox writable grants

- **WHEN** the default persistent path is used
- **THEN** the audit directory is not among the sandbox's writable grants, so the confined child cannot modify the host audit trail

#### Scenario: Directory and file permissions

- **WHEN** the persistent audit directory and file are created
- **THEN** the directory mode is `0700` and the file mode is `0600`

### Requirement: Configuration surface

The audit log SHALL be configurable via launcher config fields (`enabled`, `path`, `syslog`, `strict`) and CLI flags (`--audit-log`, `--no-audit`, `--audit-strict`), with precedence flag over config over default.

#### Scenario: Default path is the persistent central path

- **WHEN** no audit path is configured
- **THEN** the log is written to `<persistent-central-dir>/audit.jsonl` as defined by the persistent central log location requirement

#### Scenario: Flag overrides config path

- **WHEN** `--audit-log <path>` is passed and `audit.path` is also set in config
- **THEN** the flag value is used

#### Scenario: Optional syslog mirroring

- **WHEN** `audit.syslog` is true on a Unix platform
- **THEN** each event is additionally written to the system log under the `omac` tag
- **AND** a syslog failure does not prevent file-sink writes

