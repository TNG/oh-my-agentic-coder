# `omac build` — established OMAC contract mapping

Ticket: `03-run-safe-gradle-build-request` (JVM build executor v0).
Spec requirement: the implementation must reuse existing OMAC types and
lifecycle conventions where they fit and document any deviation before
introducing it. One row per contract dimension; status is current for v0.

| Dimension | Reused component | Deviation / reason |
|---|---|---|
| **CLI** | `internal/cli` subcommand registry (`cli.go`), `runSandbox`/`runCache` verb style, `Env` workdir resolution, `omac build:` stderr prefix, `--help` pattern | none — `build.go` follows `sandbox_cmd.go`/`diagnose.go` structure verbatim |
| **Transport** | none — OS process invocation (`exec` of the built `omac` binary), the same harness-independence model as `omac sandbox run`; proven identical across opencode-flavored and claude-flavored stripped envs in `build_integration_test.go` | deliberate: the spec's build *service* (facade/REST route) is a later ticket; v0 keeps transport harness-free by being a plain command. Documented in the spec's User Contract as CLI-first |
| **Streaming** | direct stdio pass-through: the child writes the caller's `Stdout`/`Stderr` directly (no buffering to completion), same model as `sandbox.ExecWithEnv` | none |
| **Sandbox / launcher** | `internal/sandboxrun`: hand-built `Grants` rendered by the unmodified `GenerateSBPL`, launched via the unmodified `BuildChildArgv` (Seatbelt backend on darwin, bwrap on Linux) | none — no parallel sandbox invented; `sbpl.go`, `facade.go`, and network semantics untouched |
| **Grant derivation** | `sandboxrun.Grants` shape + platform baseline protected paths (`~/.gradle`, `~/.ssh`, cloud dirs stay denied even under broad grants) | the grant *profile* is constructed programmatically in `buildrun.GrantsFor` rather than loaded from a named YAML profile, because the executor grant set is fixed by architecture (worktree + resolved cache leaf + private temp), not user-configured |
| **GRADLE_USER_HOME / cache scope** | `internal/toolcache`: `config.LoadLauncher` → `Cache.Resolve`, then the SAME `start.go:prepareLaunchCache` the launch path uses (no duplicate switch in build.go); `$cache/gradle` leaf per spec §Gradle State. Only the gradle leaf itself (plus private temp + worktree) is granted rw — never the cache scope dir, so sibling tool caches (go/npm/pip) stay unwritable by the executor | none — no hardcoded paths; the shared LOCK_SH lock re-acquired by `omac build` inside a parent session is compatible (flock shared locks compose) |
| **Cancellation** | process-group staged shutdown (`Setpgid` + `kill(-pgid, …)`), the same staged graceful-then-kill model as `internal/sandbox/launcher.go` (graceful deadline → SIGKILL). The hard stage fires only while the child is unreaped — a reaped child's pgid could already be recycled by an unrelated process group, so the SIGKILL is skipped once `Wait` has returned | SIGINT/SIGTERM are consumed by omac and mapped to a **distinct exit code 4** preceded by the `omac build: cancelled` stderr marker, instead of being forwarded as the child's 128+n; the ticket's exit-code contract requires cancellation to be distinguishable from a build killed by a stray signal, which pure forwarding cannot express, and exit code 4 alone would collide with a raw `gradle exit 4` |
| **Health / authentication** | — | **deferred**: both belong to the supervisor/sidecar layer (facade), which v0 deliberately does not introduce. There is no long-lived build service to health-check and no ambient caller to authenticate: the invoking process *is* the authority boundary (anyone who can run `omac` can run `omac build`, same as `omac sandbox run`). Lands with the executor-service ticket |
| **Audit** | `internal/audit`: JSONL trail via `audit.New` (best-effort, non-strict — a build never fails because the log is unavailable), `InnerExec` for the build request, `ProcessExit` for the result, `ControlMutation` for request receipt and cancellation. Sanitized metadata only — argv is task names, never credential values (credentials cannot enter the executor by construction: env pass-through is a fixed allowlist) | event types reused rather than new `build.*` types, per "reuse established patterns"; the `build.request`/`build.cancel` ControlMutation actions carry adapter/root/arg-count only |
| **Errors / diagnostics** | `omac build: <msg>` stderr style (per `omac sandbox:`), structured policy-denial phrases per spec §Diagnostics: denials name the rejected root/wrapper, the containment rule violated (outside-worktree / symlink escape), and that no build code ran; a removed-capability denial would name the manifest path + restart requirement (no runtime capability denials exist in v0 — network is fully blocked and nothing is requestable yet) | exit codes 3 (policy), 4 (cancellation), and 10 (service failure) are command-local reservations chosen to avoid *every* collision, not just with the global table: Gradle's own build-failure code is 1, its CLI misuse is 2, and 126/127/128+n are shell signal conventions. `cli.go`'s global `ExitConfigInvalid=3` / `ExitPrerequisiteMissing=4` are different domains (the global codes were assigned for `start`/`serve`); `build.go` documents its contract in help text |

## Executor process model (v0)

One restricted process per request — no warm executor session, no queue
(ADR 0001's session-scoped executor is a later ticket). Daemon-lock
staleness is a non-issue in v0 by construction: each request runs a
short-lived executor under its scoped `GRADLE_USER_HOME`, and there is
no warm-daemon reuse to wedge. v0 therefore never deletes files inside
the cache (the earlier `PruneStaleDaemonLocks` prototype was removed);
lock hygiene lands together with warm-daemon reuse in a later ticket.

## Cold-cache wrapper bootstrap (v0 limitation)

Network is fully blocked inside the executor, so the Gradle
*distribution* must already be resolvable under the cache leaf
(`GRADLE_USER_HOME = <cache scope>/gradle/wrapper/dists/…`) before
`omac build` runs — warm from a previous build in the same scope, or
pre-seeded by a host-side `./gradlew` run. A cold cache cannot
bootstrap the wrapper distribution (the download is blocked egress).

TODO(doc): host-side validation of a real `./gradlew :help` against a
pre-seeded cache is pending. The dev environment runs inside an omac
sandbox, and macOS denies nested `sandbox_apply`, so the kernel-gated
integration test (`build_integration_test.go`) cannot run here — it
skips via the sandbox-exec self-test and runs on host/CI.

## Kernel-enforcement proof status

The grants construction (worktree + cache leaf + private temp only,
blocked network, host `~/.gradle` and protected paths denied) is unit
proven by `internal/buildrun/grants_test.go` without applying a kernel
profile. Kernel enforcement is asserted by the gated integration tests
in `internal/cli/build_integration_test.go`, which skip when the
`sandbox-exec -p '(allow default)'` self-test fails — macOS refuses
nested `sandbox_apply`, so those tests run on host/CI but not inside an
omac sandbox. Reading a host secret fixture from within the kernel
sandbox is therefore a **host-side follow-up**; the fixture path is
asserted absent from the generated SBPL at unit level.
