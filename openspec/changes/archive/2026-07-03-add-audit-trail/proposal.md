# Add Audit Trail

## Why

omac is a security-boundary tool: it confines an agentic coder by spawning sidecar processes, injecting secrets, proxying HTTP, and gating outbound network access with allow/deny prompts. Yet there is no unified, tamper-evident record of what the confined agent actually did — commands executed, network destinations reached (and how they were decided), secrets injected, and control-plane mutations. Without an audit trail, incident response after a sandbox escape or a rogue skill is effectively impossible, and there is no way to prove containment held. The existing facade access log and per-skill stdout logs are partial, unstructured, and scattered.

## What Changes

- Add a central **audit log**: a single append-only, structured (JSON Lines) event stream written by omac for every security-relevant action across `start` and `serve` modes.
- **Log every command/process execution**: the sandboxed inner command and every spawned sidecar — argv (secret values redacted), cwd, resolved sandbox profile, injected secret *names* (never values), exit code, and duration.
- **Log every outbound network decision**: host, port, decision (allow/deny), scope (once/host/suffix), decision source (learned-policy / interactive-prompt / allowlist / blocklist / timeout-default), and whether it was persisted.
- **Log every facade/proxy request**: method, mount, namespace (redacted to a stable hash so secret dir-tokens don't leak), upstream status, bytes, duration — superseding the ad-hoc facade access log.
- **Log every control-plane mutation** (`/__omac__/activate|deactivate|reload|reload-global`): action, target directory, result, and caller info where available.
- **Log secret/config lifecycle events**: which skill received which secret *names* at sidecar bringup (values redacted), and pending-credentials / broken-route transitions.
- **Log session lifecycle**: omac start/stop, sandbox backend selected, profile in effect, and version.
- Add a **redaction guarantee**: secret values, keychain material, and dir-token namespaces are never written verbatim; a shared redaction layer enforces this at the emit boundary.
- Persist the log in a **central, well-known host directory that survives restarts** (Linux `$XDG_STATE_HOME/omac/audit/` → `~/.local/state/omac/audit/`; macOS `~/Library/Logs/omac/audit/`) — **not** the ephemeral per-run runtime dir, which is recreated and torn down every invocation. The default location sits outside the sandbox's writable grants so the confined child cannot tamper with it.
- Add a **`--audit-strict` command-line switch** (and `audit.strict` config) that makes an unwritable audit log **fatal** (fail-closed): omac refuses to start if the log can't be opened, and aborts the run if a write fails mid-session. The default stays fail-open (a write error becomes a single stderr warning).
- Add configuration for the audit log: destination path (default the persistent central path above), enable/disable, strict mode, and optional syslog forwarding — via the launcher config and `--audit-log` / `--no-audit` / `--audit-strict` flags.
- Add integrity affordances: `0600` file mode, append-only opens, and a monotonic per-event sequence number so gaps/truncation are detectable.

## Capabilities

### New Capabilities

- `audit-trail`: The audit event model (event types, common envelope, JSON Lines schema), the redaction rules, the writer/sink abstraction (file + optional syslog), the persistent central log location, the fail-open/fail-closed (`--audit-strict`) failure semantics, configuration surface (launcher fields + `--audit-log` / `--no-audit` / `--audit-strict` flags), and integrity properties (append-only, mode `0600`, sequence numbers).

### Modified Capabilities

<!-- openspec/specs/ has no merged baseline specs yet, so command execution, network
     decisions, facade proxying, and control-plane behavior are captured only in
     unmerged change deltas. No existing merged capability's requirements change;
     audit emission is additive and specified wholly within the new audit-trail
     capability. -->

## Impact

- **New code**: `internal/audit/` (event types, redaction, writer, file + syslog sinks, sequence counter, persistent-path resolver, fail-closed fatal handler). A small `Auditor` handle is threaded through the call sites below.
- **Changed code (emit points)**:
  - `internal/supervisor/supervisor.go` — command spawn/exit, secret-name injection.
  - `internal/sandbox/launcher.go` — inner-command exec, resolved profile.
  - `internal/netproxy/` + `internal/netprompt/prompt.go` — network allow/deny decisions and their source.
  - `internal/facade/facade.go` — replace `logAccess` with an audit-backed access event (namespace hashed).
  - `internal/cli/serve.go` — control-plane mutation handlers.
  - `internal/cli/start.go` / `serve.go` — session start/stop, `--audit-log` / `--no-audit` / `--audit-strict` flags, wiring the `Auditor` and its fatal-teardown callback before the inner command launches.
  - `internal/config/launcher.go` — new `audit` config block (`enabled`, `path`, `syslog`, `strict`).
- **Config/behavior**: a persistent audit log appears at a central host path (Linux `~/.local/state/omac/audit/audit.jsonl`, macOS `~/Library/Logs/omac/audit/audit.jsonl`) that survives restarts; opt-out via config/flag. No breaking changes to existing flags. Under `--audit-strict`, an unwritable log becomes a fatal startup/runtime error.
- **Dependencies**: none new; JSON Lines via stdlib `encoding/json`, syslog via stdlib `log/syslog` (Unix only, build-tag guarded).
- **Performance**: audit writes are buffered; default (fail-open) writes never block or crash a run (a writer error logs to stderr once); `--audit-strict` deliberately trades availability for completeness by aborting on write failure.
