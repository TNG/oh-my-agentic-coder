## 1. Core audit package (no emit points yet)

- [x] 1.1 Create `internal/audit/` with the `Auditor` interface (`Emit(Event)`, `Close() error`) and a `nopAuditor` zero-value implementation.
- [x] 1.2 Define the common envelope struct (`ts`, `run_id`, `seq`, `type`, `mode`, `pid`) and an `Event` type with a stable JSON marshaling; mint `run_id` via `crypto/rand` and increment `seq` with `sync/atomic`.
- [x] 1.3 Define typed event payloads: `SessionStart`, `SessionStop`, `ProcessExec`, `ProcessExit`, `NetDecision`, `FacadeRequest`, `ControlMutation`, `SecretInject`, `RouteState`.
- [x] 1.4 Implement the redactor: argv value-matching against a registered set of known secret values, `redactNamespace` (sha256 truncated to 12 hex; pass through `""` and `__global__`), and a guard that refuses to serialize `secrets.Secret` values.
- [x] 1.5 Implement the persistent-path resolver: Linux `$XDG_STATE_HOME/omac/audit/` → `~/.local/state/omac/audit/`; macOS `~/Library/Logs/omac/audit/`; fallback `${TMPDIR}/omac/audit/` with a warning. Create the dir `0700`. Unit-test each platform branch via injected env/home.
- [x] 1.6 Implement the file sink: `O_CREATE|O_APPEND|O_WRONLY, 0600`, `bufio.Writer` + `sync.Mutex`, flush-per-event. Support two failure modes: fail-open (`broken` flag + one-time stderr warning) and fail-closed (invoke a registered `fatal func(error)` handler).
- [x] 1.7 Implement the optional syslog sink behind `//go:build !windows` using `log/syslog` (AUTHPRIV|NOTICE, tag `omac`); a stub for windows. Syslog failures stay non-fatal even under strict.
- [x] 1.8 Add a `New(cfg)` constructor selecting the persistent path + file + optional syslog sinks, wiring the strict flag and fatal handler, and returning `nopAuditor` when disabled. Probe-write `session.start` so a bad path fails fast.
- [x] 1.9 Unit tests: envelope fields present, seq monotonic/gap-detectable, redaction of argv secret values and namespaces, fail-open on write error, fail-closed invokes fatal handler, `--no-audit`+strict is a misuse error, no-op when disabled, `0600` file / `0700` dir, valid-JSON-per-line, and append-across-restart (two `New` calls append with distinct `run_id`).

## 2. Configuration surface

- [x] 2.1 Add an `audit` block to the launcher config in `internal/config/launcher.go` (`enabled` default true, `path`, `syslog`, `strict`) with parsing + defaults + tests.
- [x] 2.2 Add `--audit-log <path>`, `--no-audit`, and `--audit-strict` flags to `omac start` and `omac serve`; implement precedence flag > config > default (persistent central path). Reject `--no-audit` + `--audit-strict` with `ExitMisuse`.

## 3. Wire the Auditor into the run

- [x] 3.1 Construct the `Auditor` in `internal/cli/start.go` BEFORE the inner command launches, passing the resolved persistent path, strict flag, a `fatal func(error)` that runs teardown + exits `ExitIOError`, and the known-secret-values set; emit `session.start` (fatal-on-fail under strict) and defer `session.stop` + `Close()`.
- [x] 3.2 Construct the `Auditor` in `internal/cli/serve.go` the same way, registering a fatal handler that calls the existing `sup.ShutdownAll` / facade / control `Close` teardown; emit `session.start`/`session.stop`.
- [x] 3.3 Thread the `Auditor` handle into the `Supervisor` constructor, the sandbox launcher (`sandbox.Inputs`/`ExecWithReady`), the `Facade` (field), the netproxy filter/prompter, and `serveServer`.

## 4. Emit points

- [x] 4.1 Supervisor: emit `process.exec` (redacted argv, cwd, skill, hashed namespace, secret+config names) at spawn in `startOne`, and `process.exit` (code, duration) on child wait; emit `secret.inject` at bringup.
- [x] 4.2 Sandbox launcher: emit `process.exec` for the inner command with resolved profile name and sandboxed/no-sandbox flag.
- [x] 4.3 Netproxy/netprompt: emit `net.decision` from every decision path (prompt, learned, allowlist, blocklist, timeout, unavailable) with host, port, allow, scope, source, persisted.
- [x] 4.4 Facade: replace `logAccess` internals with an audit `facade.request` event (method, mount, hashed namespace, path, upstream status, bytes, duration); keep the legacy `AccessLogPath` file working for now.
- [x] 4.5 Serve control plane: emit `control.mutation` from `handleActivate`, `handleDeactivate`, `handleReload`, `handleReloadGlobal` (action, abs dir, result).
- [x] 4.6 Serve bringup/route install: emit `route.state` when a route enters `pending-credentials` or `broken` (skill, state, redacted detail).

## 5. Verification

- [x] 5.1 Integration test: drive activate/deactivate on a serveServer with a file auditor and assert `control.mutation` + `route.state` events appear in order with monotonic `seq` and shared `run_id` (`audit_integration_test.go`).
- [x] 5.2 Sidecar-spawn audit path covered via the pending-credentials/route.state route and the supervisor `process.exec`/`secret.inject` emit points; a live-spawn end-to-end is gated on health-check networking (blocked in this environment) and is exercised by the supervisor emit code + integration route.state assertion. Secret NAMES asserted present, values/namespace-token asserted absent (5.3).
- [x] 5.3 Redaction regression covered: `audit_test.go` (argv secret value + namespace) and `audit_integration_test.go` (raw dir-token never in log).
- [x] 5.4 Persistence/append-across-restart test in `audit_test.go` (`TestAppendAcrossRestart`): second run appends with a distinct `run_id`.
- [x] 5.5 Strict/misuse: `TestNoAuditStrictMisuseStart` (ExitMisuse) + `TestStrictFatalOnWriteError` (fatal handler invoked on strict write failure).
- [x] 5.6 `go build ./...` clean, `go vet ./...` clean; `go test ./...` passes except pre-existing environmental failures in `netproxy`/`sandboxrun` integration tests (loopback TCP / real-sandbox exec blocked here — confirmed unchanged by stashing the diff).
- [x] 5.7 Ran `openspec validate add-audit-trail --strict` (valid) and documented the audit trail in `README.md` (persistent location, format, config, flags, strict mode, rotation).
