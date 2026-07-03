## Context

omac is the security boundary between an agentic coder (OpenCode / Claude Code) and the host. It re-execs the harness inside a kernel sandbox, spawns per-skill sidecar processes with injected secrets, reverse-proxies HTTP to those sidecars over a Unix socket + loopback TCP facade, and gates outbound network with an interactive allow/deny prompt plus a learned-policy store.

Today, security-relevant actions leave only partial traces:

- `internal/facade/facade.go:598` `logAccess` writes a minimal per-request JSON line to a facade-specific log file — only when `AccessLogPath` is set, and it includes the raw `mount` but not the namespace, status source, or a stable request id.
- Sidecars get per-skill stdout/stderr log files (`internal/supervisor/supervisor.go:165`), but omac itself records nothing about the spawn (argv, secret names, exit code).
- `internal/netprompt/prompt.go` returns an allow/deny `PromptResult` and discards it; `internal/netproxy` enforces but does not record decisions in a durable, correlated form.
- Control-plane mutations (`internal/cli/serve.go` `handleActivate` etc.) are unlogged.

Constraints:
- Must not leak secrets. Secret values (`internal/secrets`), keychain material, and per-directory namespace tokens (random bearer tokens, see the SECURITY note at `facade.go:460`) must never appear verbatim in the audit log.
- Failure handling is operator-selectable. By default a failing audit sink is a degraded state, not a fatal one (fail-open); but a `--audit-strict` mode makes an unwritable audit log fatal (fail-closed), because in a compliance/forensics posture "we ran but didn't record it" is worse than "we refused to run".
- Logs must be **persistent and central**: the audit trail has to survive restarts and be findable at a single, well-known, host-level path — not the per-run runtime dir, which is created fresh and torn down every invocation.
- Cross-platform: macOS (Seatbelt) and Linux (bubblewrap). Syslog forwarding is Unix-only and must be build-tag guarded.
- Emit points live in packages that today have no shared logging handle; a thin `Auditor` must be injectable without a large refactor.

## Goals / Non-Goals

**Goals:**
- One append-only, structured (JSON Lines) audit stream capturing every command execution, network decision, facade request, control-plane mutation, secret-injection event, and session lifecycle transition.
- Strong, centralized redaction: secrets and dir-token namespaces are redacted at the single emit boundary, so no call site can accidentally leak them.
- Tamper-evidence affordances that are cheap and dependency-free: `0600` append-only file, monotonic per-run sequence numbers, and a run id that correlates every event.
- A **persistent, central** default log location that survives restarts, so the trail accumulates across runs rather than vanishing with each per-run runtime dir.
- Selectable failure semantics: fail-open by default (a sink error degrades to a stderr warning), or fail-closed under `--audit-strict` (a sink error aborts the run before the inner command is exec'd, and terminates it if the log becomes unwritable mid-run).
- Opt-out and destination configurable via launcher config and a `--audit-log` flag.

**Non-Goals:**
- Cryptographic tamper-proofing (hash chaining / signing / remote attestation). Sequence numbers detect gaps; true tamper-proofing is a follow-up.
- Log shipping to cloud SIEMs beyond optional local syslog.
- Auditing *inside* the sandbox (the harness's own tool calls) — omac only sees what crosses its boundary (commands it spawns, network it proxies/gates, control-plane calls). Harness-internal tool calls are out of scope and noted as an open question.
- Rotating/retention policy beyond the existing per-run runtime-dir lifecycle.

## Decisions

### D1: JSON Lines to a file, one event per line
One `{...}\n` JSON object per event. Rationale: greppable, streamable, appendable without parsing prior content, and trivially ingestible by `jq`/syslog/SIEM. Alternative considered: a binary/protobuf log (rejected — opaque, needs tooling, overkill for the volume). Alternative: SQLite (rejected — concurrent-writer complexity, and we already saw fragility shelling to `sqlite3` in `session.go`).

### D2: Common event envelope
Every event shares a fixed envelope:
```
{ "ts": RFC3339Nano, "run_id": "<hex>", "seq": <uint64>, "type": "<event.type>",
  "mode": "start|serve", "pid": <int>, ... type-specific fields ... }
```
`run_id` is minted once per omac invocation (reuse `mintToken`-style `crypto/rand`); `seq` is a process-global atomically-incremented counter so missing lines are detectable. `type` is a dotted namespace: `session.start`, `process.exec`, `process.exit`, `net.decision`, `facade.request`, `control.mutation`, `secret.inject`, `route.state`.

### D3: Redaction at the emit boundary, not the call site
The `Auditor.Emit` path runs every event through a redactor before serialization. Rationale: defense in depth — even if a future call site passes a secret, the boundary strips it. Concretely:
- Argv is passed as `[]string`; the redactor replaces any element that exactly equals a known secret value with `"<redacted>"`. Call sites SHOULD also pass secret *names* separately (the preferred, always-safe form) rather than relying on value matching.
- A `Namespace` field is always hashed (`sha256` truncated to 12 hex chars) via a dedicated `redactNamespace` helper, so dir-tokens never appear. `__global__` and `""` (flat) are passed through unhashed since they are not secret.
- The event structs implement a typed API (`ProcessExec{Argv, SecretNames, ...}`) rather than free-form maps, so the redactor knows which fields are sensitive.

### D4: `Auditor` is an interface with a no-op default
```
type Auditor interface {
    Emit(ev Event)
    Close() error
}
```
A `nopAuditor` is the zero value used when auditing is disabled or a sink fails to open, so every call site can hold a non-nil `Auditor` and call `Emit` unconditionally. Rationale: keeps emit points a single unguarded line; avoids nil checks scattered across packages.

### D5: Buffered, mutex-guarded file sink with selectable failure mode
The file sink wraps a `*os.File` opened `O_CREATE|O_APPEND|O_WRONLY, 0600` behind a `bufio.Writer` and a `sync.Mutex`. Each `Emit` marshals, writes, and flushes (flush-per-event to bound loss on crash; volume is low — human-scale actions, not per-byte). The response to a write error depends on the failure mode (see D9):

- **fail-open (default):** the sink sets a `broken` flag, prints one stderr warning, and becomes a no-op for the rest of the run. Correctness of the sandboxed run wins over audit completeness, but silent total failure is worse than one warning.
- **fail-closed (`--audit-strict`):** the sink reports the error up through `Emit`, which invokes a registered fatal handler that tears down the run (stops sidecars, kills the inner command) and exits non-zero. "We ran but didn't record it" is unacceptable in this posture.

To make fail-closed meaningful at startup, the auditor is **opened and probed before the inner command is exec'd**: a `session.start` event is written and flushed first, so an unwritable/permission-denied log aborts the launch cleanly (exit `ExitIOError`) rather than after the sandbox is already running.

### D6: Optional syslog sink (Unix, build-tagged)
When `audit.syslog: true`, a second sink via `log/syslog` (LOG_AUTHPRIV|LOG_NOTICE, tag `omac`) mirrors events. Guarded by `//go:build !windows`. Rationale: syslog gives the host operator a tamper-resistant, centrally-collected copy outside the sandbox's writable tree — valuable precisely because the sandboxed process could otherwise tamper with a file it can reach. Alternative (journald native protocol) rejected as adding a dependency; classic syslog is stdlib.

### D7: Persistent, central default log location (not the runtime dir)
The audit log must **survive restarts** and live in one **well-known host-level directory**, not the ephemeral per-run runtime dir (`createRuntimeDir*` builds `${TMPDIR}/omac-serve-<hash>/…` fresh and `RemoveAll`s stale copies each start — anything there is lost on restart). The default audit directory is resolved once, host-wide:

- **Linux:** `$XDG_STATE_HOME/omac/audit/` if `XDG_STATE_HOME` is set, else `$HOME/.local/state/omac/audit/`. (`state` per the XDG Base Directory spec is the correct home for logs/history that should persist but aren't config.)
- **macOS:** `$HOME/Library/Logs/omac/audit/`.
- If no home dir resolves (rare, e.g. some CI): fall back to `${TMPDIR}/omac/audit/` and log a warning that persistence is not guaranteed.

The directory is created `0700` and the file `0600`. The default filename is `audit.jsonl` (a single growing file so the trail is continuous across restarts; rotation/retention is a documented Non-Goal for now — operators can use `logrotate`/`newsyslog`, and the append-only format is rotation-safe). A per-run `run_id` in every event keeps runs distinguishable within the shared file.

This directory is deliberately **outside** every sandbox writable grant by default, so the confined child cannot tamper with the host's audit trail; the sandbox continues to write only its own runtime-dir logs.

### D8: Configuration surface
Launcher config gains:
```
audit:
  enabled: true            # default true
  path: ""                 # default: <persistent-central-dir>/audit.jsonl (see D7)
  syslog: false
  strict: false            # fail-closed on audit write failure (see D9)
```
CLI overrides: `--audit-log <path>` overrides `audit.path`; `--no-audit` disables (ignored under strict — see D9); `--audit-strict` forces `strict: true`. Precedence: flag > config > default.

### D9: `--audit-strict` fail-closed switch
A new boolean, settable via `audit.strict` config or the `--audit-strict` flag, changes the failure mode from fail-open (D5) to fail-closed. When strict:

- The auditor is constructed and its log opened+probed **before** the inner command launches; an open/permission failure aborts with `ExitIOError` and a clear message, and the inner command never starts.
- A mid-run write failure invokes a fatal handler (a callback the CLI registers) that runs the normal teardown (`sup.ShutdownAll`, facade/control `Close`, remove socket/control-info) and exits non-zero (`ExitIOError`).
- `--no-audit` combined with `--audit-strict` is a misuse error (`ExitMisuse`): you cannot both require a complete trail and disable it.
- Strict applies to the **file** sink (the source of truth). A syslog-only failure remains a warning even under strict, since the file already recorded the event; this is documented so operators aren't surprised.

Rationale: two legitimate postures exist — developer convenience (never let logging break my run) and compliance/forensics (never run unrecorded). One flag cleanly selects between them; the default stays convenient.

### D10: Threading the Auditor and the fatal handler
`start.go` / `serve.go` construct the `Auditor` **before** launching the inner command, passing in (a) the resolved persistent path (D7), (b) the strict flag + a `fatal func(error)` teardown callback (D9), and (c) the set of known secret values for redaction (D3). The handle is then passed into: the `Supervisor` (constructor gains an `Auditor`), the sandbox launcher (`sandbox.Inputs` / `ExecWithReady` gain it), the `Facade` (field), the netproxy filter/prompter (field), and the `serveServer` (field). A single construction point keeps path resolution, failure mode, and redaction config in one place.

## Risks / Trade-offs

- **[Secret leak via argv value-matching is heuristic]** → The always-safe path is `SecretNames` (names only). Value-matching is a secondary net for cases where a secret is interpolated into an argv the caller didn't tag. Document that call sites must never place secrets on argv (they already use env), so in practice argv redaction is belt-and-suspenders.
- **[Flush-per-event cost]** → Volume is human-scale (spawns, prompts, control calls), so the cost is negligible; if a future high-rate event type appears (e.g. per-facade-request under load), that specific type can batch-flush. Facade requests are the one higher-rate source — mitigate by sampling/coalescing only if profiling shows it matters (Open Question).
- **[Sandboxed process tampering with the file log]** → The default persistent dir (D7) is outside every sandbox writable grant, so the child cannot reach it. Syslog (D6) provides an additional out-of-band copy. Document that if an operator points `--audit-log` *inside* a granted tree they reintroduce this risk.
- **[Persistent log grows unbounded]** → Single-file append is rotation-safe; document `logrotate` (Linux) / `newsyslog` (macOS) recipes. Automatic rotation/retention is a Non-Goal for this change.
- **[Fail-open hides audit gaps]** → Mitigated by `seq` gap-detection, the one-time stderr warning, and — for operators who cannot tolerate gaps — `--audit-strict` (D9), which makes the gap fatal instead.
- **[`--audit-strict` turns a disk/permission problem into an outage]** → Intentional for the compliance posture, but a footgun for the convenience posture, so it is strictly opt-in and off by default. The pre-launch probe (D5) makes the common failure (bad path/permissions) a fast, clear startup error rather than a surprise mid-session kill.
- **[Namespace hashing breaks correlation across runs]** → Acceptable: the goal is not to leak the token, and within one run the hash is stable, so per-run correlation holds. Cross-run correlation of a directory can use its (non-secret) absolute path, which control-mutation events log.

## Migration Plan

1. Land `internal/audit/` with the interface, file sink (with fail-open/fail-closed modes), the persistent-path resolver (D7), redactor, event types, and tests — no emit points yet (pure addition, no behavior change).
2. Wire construction in `start.go`/`serve.go` behind the default-on config, opening the log at the persistent central path before the inner command launches; emit `session.start`/`session.stop` only. Verify the file appears at the persistent path, is `0600`, and survives a restart (second run appends with a new `run_id`).
3. Add emit points incrementally: process exec/exit, then net decisions, then facade requests (replacing `logAccess`), then control mutations, then secret injection. Each step is independently reviewable and testable.
4. Keep the old `AccessLogPath` facade log working during transition; once `facade.request` events cover it, deprecate `AccessLogPath` in a follow-up (out of scope here).

Rollback: set `audit.enabled: false` (or `--no-audit`, when not strict); the code paths become the no-op auditor with zero behavioral impact.

## Open Questions

- Should facade-request events be sampled/coalesced under high throughput, or is flush-per-event fine? (Resolve via profiling; default to per-event.)
- Do we want to (optionally) capture harness-internal tool calls by having the bridge plugin post them to a control-plane `/__omac__/audit` endpoint, so the audit trail includes what the agent *asked* to do, not just what crossed omac's boundary? (Deferred; would need an authenticated control endpoint, which ties into the separate control-plane-auth gap.)
- Hash-chaining events for tamper-evidence: worth a lightweight follow-up capability, or leave to syslog/SIEM?
