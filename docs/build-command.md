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

## Executor process model (warm-daemon reuse + per-worktree queue)

Ticket 04 superseded the v0 "no warm executor, no queue" model. The warm
executor is **Gradle's own daemon** persisting under the session-scoped
`GRADLE_USER_HOME` leaf — there is NO long-lived omac supervisor process
and NO IPC/socket service:

- **Warm daemon reuse.** Each `omac build` spawns a fresh `gradlew`
  process (as in v0), but because `GRADLE_USER_HOME` is a stable
  session-scoped leaf (`<cache scope>/gradle`, already from ticket 03),
  Gradle keeps a daemon alive in that leaf and reuses it across
  invocations. No new long-lived omac process to manage; the daemon
  lingers by Gradle's idle-stop policy — that IS the warm state.

- **Per-worktree queue serialization.** Each `omac build` acquires an
  exclusive `flock` on `<leaf>/.omac-build.lock`, released on exit
  (`defer`). Auto-released on crash (the kernel releases flock when the
  process dies) — NO stale-lock cleanup is needed. Independent worktrees
  resolve to independent leaves (independent lockfiles) → concurrent.
  Same-worktree invocations serialize (they share a warm daemon and would
  corrupt each other's cache). The acquire is **cancellable** while
  waiting (spec §136: queued requests are individually cancellable): the
  build's cancel channel is wired in, so a second `omac build` Ctrl-C
  unwinds a waiter without killing the running build. Two outcomes on
  contention:
  - cancelled-while-waiting → `ExitCancelled` (4) + the
    `omac build: cancelled` marker (the waiter was individually
    cancelled, not busy-denied);
  - timed-out-waiting (30s `DefaultQueueTimeout`) → `ExitServiceFailure`
    (10) + "another build is running in this worktree" (the busy path).

- **Resource ceilings.** `--max-duration <duration>` (before `--`)
  bounds the total build wall-clock; an over-budget run is cancelled as
  if the caller signalled (graceful first, then the staged kill). A
  non-positive or unparseable value is rejected at parse time
  (spec §150: an excessive request fails before executor startup).

- **Cancellation (two stages).** The first SIGINT/SIGTERM is a GRACEFUL
  cancel: SIGTERM to the gradlew process group, then SIGKILL after the
  bounded graceful window — and the warm Gradle daemon is PRESERVED
  (spec §144: graceful cancellation keeps a trustworthy warm executor).
  A second signal (or `--max-duration` expiry) is a FORCED cancel: the
  graceful window collapses to ~0 and the gradlew group is SIGKILLed
  immediately, AND the (potentially corrupt) Gradle daemon is RECYCLED —
  `omac build` runs `gradlew --stop` against the leaf best-effort after
  the forced kill, so a build that corrupted daemon state does not leave
  a poisoned warm daemon for the next request. A wedged daemon that
  ignores `--stop` may require manual `omac build stop`.

- **Teardown.** `omac build stop [--root <rel>]` runs `gradlew --stop`
  under the leaf's `GRADLE_USER_HOME` (the SAME isolated env as the
  build: no host HOME, no host `~/.gradle`, no host creds — spec §125-132
  boundary) to stop lingering daemons for this worktree, then
  **force-kills** any wedged daemon for the leaf that ignored the
  cooperative stop (spec §146: session teardown kills the process
  tree). `--root <rel>` resolves the wrapper at
  `<worktree>/<rel>/gradlew` (default `.`) — the same root the build
  path uses, so `omac build stop --root backend` tears down the daemon
  for the `backend/` build, not the worktree root. The two-stage
  teardown (cooperative `--stop` then force-kill from the leaf's daemon
  registry) is best-effort. Finally it removes the lockfile. A crashed
  `omac build` releases the flock automatically; the daemon may linger
  until `stop` or idle-stop.

**Linux daemon-cohabitation (known item).** Linux per-request
private-loopback namespace (kernel-blocked posture) may prevent a new
client reaching a prior request's daemon — warm-daemon reuse may not hold
on Linux the way it does on macOS Shape A (env-only filtered, so the
Gradle daemon's loopback worker protocol works). Linux validation of the
warm-daemon path is deferred to later tickets; macOS Shape A makes it
work by construction.

## Cold-cache wrapper bootstrap

On macOS (Shape A) the executor is env-only filtered via the omac proxy,
so the Gradle *distribution* can download through the proxy on first
use. On Linux the executor is kernel-blocked, so the distribution must
already be resolvable under the cache leaf
(`GRADLE_USER_HOME = <cache scope>/gradle/wrapper/dists/…`) — warm from a
previous build in the same scope, or pre-seeded by a host-side
`./gradlew` run.

## JDK resolution (Shape A)

jenv/asdf/SDKMAN shims break under deny-default Seatbelt (`/dev/fd`
process substitution denied; see the loopback spike REPORT.md:105-115).
The executor resolves the REAL JDK: it follows symlink chains from
`JAVA_HOME` and each `PATH` entry, rejects shim **shell scripts** (a
jenv shim at `~/.jenv/shims/java` is a regular executable `#!/bin/sh`
script, NOT a symlink and NOT a native binary — `realJava` reads the
first two bytes and rejects any `#!` header), and sets `JAVA_HOME` +
`PATH` to the real JDK bin (shims stripped), granting the JDK's
`bin`+`lib` read access. This is why a `JAVA_HOME` pointing at the jenv
ROOT (`~/.jenv`, which has no real `bin/java`) is never trusted.
`/usr/libexec/java_home` is a FALLBACK only — it pointed at a
nonexistent JDK on a test host, so it is not used as a primary
discovery path.

## Platform read baseline

The build executor merges `sandboxprofile.PlatformBaseline().Read` into
its grant set — the SAME baseline `omac sandbox run` merges via
`ResolveGrants`. On macOS this grants read-only access to `/bin`,
`/usr/bin`, `/usr/lib`, `/private/var/select` (the `sh` symlink),
`/etc`, `/System`, `/Library`, and the Homebrew roots, so the executor
under deny-default Seatbelt can exec `/bin/sh`, read `/usr/bin/uname`,
and resolve the dynamic linker. Without this baseline the `gradlew`
script fails with `uname: command not found` and
`Error opening /private/var/select/sh: Operation not permitted`. The
WRITE set stays minimal (worktree + cache leaf + private temp only);
the baseline's broad `/tmp` / `/var/folders` write grants are
deliberately NOT added. The baseline `ProtectedPaths` (`~/.ssh`,
`~/.gradle`, cloud creds, keychains) are merged into
`Grants.ProtectedPaths` so host secrets stay denied even though system
dirs are now read-granted.

## Network posture (Shape A)

macOS: env-only filtered. The Gradle daemon talks to its workers over a
random loopback port, which a kernel network boundary blocks; env-only
lets that loopback work while the omac proxy still filters external
egress. Proxy config is injected via `GRADLE_OPTS` (proxy system
properties, plus the proxy credentials in `https.proxyUser` /
`https.proxyPassword`), **NEVER `JAVA_TOOL_OPTIONS`** — the JVM prints
`JAVA_TOOL_OPTIONS` on every launch, leaking any proxy token
(spec.md:180). The proxy token itself rides ONLY in `GRADLE_OPTS`:
the omac proxy (`netproxy.Server`) authenticates every connection via
`Proxy-Authorization: Basic user:token`, and Gradle's HTTP client sends
`https.proxyUser` / `https.proxyPassword` as that header. The JVM does
**not** print `GRADLE_OPTS`, so the token is safe there; it is never
written to the OMAC-generated `gradle.properties` (that file is readable
by build code and persists on disk in the cache leaf). `NO_PROXY` /
`http.nonProxyHosts` excludes loopback so the daemon's worker protocol
is not proxied. Linux: kernel-blocked.

## Control-state protection

OMAC-generated control state under the leaf (`gradle.properties`,
`.omac-control/`, AND the `init.d/` directory — Gradle loads
`init.d/*.gradle` as init scripts) is READ-ONLY to the executor: it
appears in `ReadPaths` and a new `WriteDenyPaths` grant (an SBPL
write-deny emitted AFTER the write-allows) overrides any broader leaf
write-grant covering it. The `init.d/` directory is created OMAC-owned
(mode 0o500) so the executor cannot create it if absent and cannot plant
`<leaf>/init.d/evil.gradle` (spec §164: Gradle init scripts and init.d
entries are executable control state that must be write-protected).
Gradle reads the OMAC-imposed proxy/JVM-arg/resource-ceiling settings;
build or test code cannot rewrite them.

A write to any control-state path surfaces to the build as an EPERM
(kernel-denied by the sandbox), not an OMAC-specific message — runtime
EPERM interception is a later diagnostics ticket. The OMAC explanation
lives where the agent will look: `.omac-control/README` names the
resource (init scripts, gradle.properties, OMAC control config) and the
supported alternatives (project-level `build.gradle` / `gradle.properties`
in the worktree, or the OMAC manifest at `.omac/build.yaml`).

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
