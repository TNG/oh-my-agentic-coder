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

## Build manifest (`.omac/build.yaml`)

Standard Gradle projects require **no** manifest — `omac build` auto-detects
the wrapper and proceeds with defaults. An optional committed manifest
declares non-standard build capabilities that are not discovered
automatically:

```yaml
version: 1
builds:
  - root: backend
    tool: gradle
    containers:
      images:
        - pgvector/pgvector:pg16
        - minio/minio:latest
registries:
  - alias: internal
    upstream: ghcr.io/tng     # non-secret upstream identity only
resources:
  maxHeap: 3g                 # narrows the host default (within the ceiling)
  maxDuration: 45m
  maxCPU: 4
  maxProcesses: 512
```

**The manifest REQUESTS capabilities; it does NOT grant them.** Host policy
is the ceiling. A resource request above the host ceiling is rejected before
executor startup with exit 3 (`ExitPolicyDenied`).

**No secrets.** The manifest must never contain credentials. Any field whose
name matches `password|secret|token|credential|apikey|auth` with a non-empty
value is rejected at parse time — credentials stay in each developer's OMAC
keychain (ticket 06 wires the credential lift). A registry `upstream:` with
embedded userinfo (`user:pass@host`) is likewise rejected.

**No absolute paths.** `root:` is relative to the worktree (e.g. `backend`),
so a colleague's linked worktree resolves identically without per-worktree
setup. Absolute roots and `..` traversal are rejected.

**Forbidden capabilities.** Host bind mounts, privileged mode, raw sockets,
host namespaces, and devices are NOT in the manifest schema — project
configuration cannot enable them. A manifest that attempts one (e.g.
`containers.bindMounts:`) is rejected with a `HostForbiddenError`:

```text
OMAC rejected host bind mount builds[0].containers.bindMounts[0].
Host bind mounts are forbidden by host policy and cannot be enabled through
.omac/build.yaml.
```

### Approval and frozen-for-session policy

OMAC stores an **approval** against the manifest content digest (SHA-256 over
a canonical re-encoding) and the effective (post-ceiling) capability set.
The approval record lives **under the cache leaf** at
`<cache scope>/gradle/.omac-control/manifest-approval.json` — it is
per-developer-per-machine, NEVER committed to the worktree (the worktree is
shared/committed; approval is personal). An **active-manifest** record at
`<cache scope>/gradle/.omac-control/active-manifest.json` freezes the
in-effect digest + capability set for the session.

Both files are OMAC-owned control state, read-only to the executor (covered
by the same `WriteDenyPaths` protection as `gradle.properties` and `init.d/`).

The gate runs on every `omac build` after Resolve, before GrantsFor:

- **First use of changed manifest content** (no active record, or a digest
  differing from the active record): OMAC RECORDS the approval (digest +
  effective set) AND **fails the build with exit 3** plus one consolidated
  capability diff (added/removed build roots, images, registries, resource
  changes) and a restart instruction. The build does NOT start this time —
  the human reviews the diff first. Example:

  ```text
  omac build: manifest approval required
  manifest gate: manifest changed since last approval — review the consolidated diff, then restart OMAC to activate
  OMAC build manifest changed. Consolidated capability diff:
    - added images: [postgres:17]
    - removed images: [postgres:16]
  Restart OMAC to review and activate the changed capability set.
  ```

- **Unchanged approved manifest** (active record's digest matches): the build
  starts UNATTENDED with the frozen capability set. Editing the worktree file
  mid-session changes the digest and triggers the re-approval gate on the
  next build — the edit does NOT silently take effect.

- **Host ceiling dropped** below what was previously approved: the gate
  re-records approval against the new (lower) capability set and fails with
  the diff + restart instruction (the previously-approved set is no longer
  valid).

### v1 approval limitation

v1 has **no auto-approve and no `omac build approve` subcommand**. The
approval flow is: 1st build after a change → fails with the diff (approval
recorded); 2nd build (same digest) → starts unattended. There is no way to
skip the review on the first run, and no CLI to approve without running the
build. The gate failure IS the approval prompt. (A future `omac build
approve` or auto-approval policy would call the same `buildmanifest.Approve`
seam.)

### Runtime missing-capability diagnostic

When a build requests a capability (image, registry, build root) NOT in the
active approved set, OMAC emits a structured diagnostic and exits 3:

```text
OMAC build denied container image postgres:17.
Add the container image to .omac/build.yaml, then restart OMAC to review and
activate the changed capability set. The current session policy is frozen; do
not retry.
```

(The mediated-container enforcement that emits this at runtime is tickets
08/09; ticket 05 only declares and approves the image list.)

## Private Maven registry access (credential lift)

Ticket 06 lets an unchanged Gradle build resolve a real private Maven
dependency while the long-lived registry credential remains entirely
outside the JVM build executor. This is the Gradle tracer bullet for
GitHub issue #92's scoped credential-proxy design (spec §"Dependency
And Credential Networking").

### Two proxies, one CLI startup

`omac build` starts TWO host-side proxies side by side on macOS (Shape A):

1. **The existing filtered proxy** (`internal/netproxy`) handles PUBLIC
   dependency resolution — Maven Central, Gradle plugin/distribution
   hosts, JitPack, common mirrors — over direct CONNECT tunnels. **No
   TLS interception** (spec non-goal §57). Ticket 06 TIGHTENS this filter
   from allow-all (ticket 04) to an allowlist of public Gradle/Maven
   endpoints + the approved private-registry upstream hosts, with
   build-scan upload hosts (`scans.gradle.com`, `ge.gradle.org`,
   `scan.gradle.com`) DENIED (spec non-goal §56). Anything outside the
   allowlist is denied fail-closed (prompting is disabled — the manifest
   approval IS the prompt replacement).

2. **The credential-lift proxy** (`internal/credproxy`) handles ONLY the
   declared private registries. It is a forward HTTP server (not a CONNECT
   tunnel): it receives Gradle's plain-HTTP request for a private-repo
   path, injects an `Authorization: Basic <user:pass>` header using the
   developer's OMAC keychain credential, and forwards to the upstream
   Maven repo over a fresh TLS connection. Gradle sees only a non-secret
   local loopback URL per alias — `http://127.0.0.1:<port>/<alias>/` —
   NEVER the credential.

### How Gradle is pointed at the credential proxy

Gradle never sees the upstream private registry directly. OMAC authors a
read-only init script at `<cache scope>/gradle/init.d/registry-
credentials.gradle` (control state, read-only to the executor) that
injects one `maven { url = 'http://127.0.0.1:<port>/<alias>/' }`
repository per approved alias into every project's `repositories { }`
block via `allprojects`. No credentials are configured on the injected
repository — the credential-lift proxy authenticates upstream. The
developer's `build.gradle` still declares the upstream registry; the
injected local mirror is additive.

### Where the credential lives

The credential stays in each developer's OMAC keychain, looked up ONCE at
proxy startup (host-side, unsandboxed) by the registry ALIAS (the
non-secret manifest entry). Convention:

```
service = omac/build/registry/<alias>
account = credential
value  = <user>:<password>     (HTTP Basic auth credentials)
```

The manifest carries ONLY the alias + upstream; the credential is the
developer's keychain entry. Set it with `omac secrets set` (or the OS
keychain directly).

### What NEVER sees the credential

The credential NEVER appears in: executor env (`ChildEnv`), `GRADLE_OPTS`,
`gradle.properties`, process args, the cache leaf, stdout/stderr, audit
events, captured logs, or the init script. It rides ONLY in the
`Authorization` header sent upstream over TLS from the credential-lift
proxy. `JAVA_TOOL_OPTIONS` is NEVER used (the JVM prints it — spec
§180); the proxy URL is a non-secret loopback URL with no userinfo. The
red-team test `TestCredentialLift_GrantsEnvAndControlStateDoNotLeak`
asserts this end-to-end.

### Read-only / publish rejection

The credential-lift proxy is READ-ONLY for the dependency workflow:
only `GET` and `HEAD` (artifact/metadata download + presence check) are
forwarded. `PUT`/`POST`/`DELETE` (publish, deploy) and any request to an
unregistered upstream are denied with a structured denial naming the
registry alias — never the credential.

### Missing-credential denial

A build with an APPROVED private registry alias but NO keychain credential
for it fails closed with exit 3 (`ExitPolicyDenied`) and a structured
diagnostic naming the alias and the keychain service/account convention
— never a crash, never the credential:

```text
OMAC build denied private registry "internal".
Add the registry credential to the OMAC keychain:
  service = omac/build/registry/internal
  account = credential
  value  = <user>:<password>
Run `omac secrets set <alias>` (or set the keychain entry directly), then
restart OMAC to activate the credential lift.
The current session policy is frozen; do not retry.
```

On headless Linux without a Secret Service daemon (keychain backend
unavailable), the same structured denial points at the OS fix instead.
The credential cannot be recovered from inside the executor.

### Platform posture (v1)

Both proxies are **macOS-only in v1** (Shape A, env-only network). On
Linux the build executor is kernel-blocked, so neither the filtered proxy
nor the credential-lift proxy is started (the loopback HTTP server would
be unreachable from the executor). Linux private-registry resolution is
deferred to the kernel-sandbox validation tickets. The credential-lift
design is platform-agnostic; only the startup gate is macOS-only.

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

macOS: env-only filtered, **filesystem confinement only — no kernel
network mediation**. The Gradle daemon talks to its workers over a random
loopback port, which a kernel network boundary would block; env-only lets
that loopback work because nothing filters it. The omac proxy filters
external egress for well-behaved clients, but raw-socket-capable build
code can reach host loopback services and external egress directly — this
is the **accepted macOS residual**, reported in provenance, never
described as loopback protection or guarded loopback (ADR 0003 Revision
retired guarded loopback; the 2026-07-29 Seatbelt spike proved it
unimplementable). Host-listener monitoring/guarding is **not** claimed and
**not** implied; such behavior returns only with a future micro-VM
executor ("Shape B"). Proxy config is injected via `GRADLE_OPTS` (proxy
system properties, plus the proxy credentials in `https.proxyUser` /
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
is not proxied. Linux: kernel-blocked (private sandbox loopback via the
isolated network transport — a kernel boundary; host-loopback services
are unreachable from the executor while Gradle workers reach
executor-created dynamic ports). See also [Canonical worker-based checks
(ticket 07)](#canonical-worker-based-checks-ticket-07) and the
`omac provenance` build-executor section.

## Canonical worker-based checks (ticket 07)

The canonical `checkstyleMain` / `checkstyleTest` tasks run unchanged on
**both** platforms — they run their Checkstyle analysis through the
Gradle Worker API process isolation, exactly as developers and CI run
them. No OMAC-specific replacement tasks and no host init script are
required.

### Why canonical tasks work on both postures

- **macOS Shape A (env-only, filesystem confinement only):** the Gradle
  Worker API's dynamic loopback works because nothing filters it — there
  is no kernel network boundary to trip. The canonical worker process
  spawns and the daemon reaches it over a random loopback port, exactly
  as on a host build.
- **Linux (private sandbox loopback, kernel boundary):** the executor
  gets a private loopback via its isolated network transport. Workers
  reach executor-created dynamic ports; host-loopback services stay
  unreachable from the executor.

### yarp3 checkstyle twin retirement

yarp3 historically needed machine-local `checkstyle*Sandbox` twin tasks
AND a host init script because guarded loopback was the goal. ADR 0003
Revision killed guarded loopback → the twins and host init script are no
longer needed. Per spec §Gradle State (168): "yarp3's existing Checkstyle
twin tasks are retired."

OMAC authors a read-only init script at
`<cache scope>/gradle/init.d/retire-checkstyle-twins.gradle` (control
state, read-only to the executor) that **neutralizes any stale
machine-local `checkstyle*Sandbox` twin** so the canonical
`checkstyleMain`/`checkstyleTest` are the only checkstyle tasks that
actually run. The script:

- runs BEFORE project task-graph evaluation (via Gradle's `beforeProject`
  hook), so the twins are neutralized in time;
- uses task configuration avoidance (`tasks.matching { it.name ==~
  /checkstyle.*Sandbox/ }.configureEach { … }`) so projects without the
  twins are not configured — it is a **defensive no-op** when no twins
  exist;
- overrides each twin's actions to a no-op (`task.actions = []`) and logs
  the retirement at **configuration time** via the init-script `logger`
  (NOT via `task.doFirst` — the subsequent `actions = []` would clear a
  doFirst closure, so the log line must not ride on the task's action
  list), so the twin cannot run the machine-local Checkstyle it was wired
  for;
- is wrapped in `try/catch` so a project that fails to configure for
  unrelated reasons is unaffected.

The retirement script is written **unconditionally** by
`PrepareControlState` (it applies to every build) and granted read-only
(appears in `controlFiles` + `WriteDenyPaths`, same protection as the
ticket-06 credential-lift init script and the ticket-05 manifest records).

> The retirement script neutralizes only the `checkstyle*Sandbox` twins.
> The canonical `checkstyleMain` / `checkstyleTest` tasks are left
> untouched — they are what actually runs. The required Mockito-agent
> behavior (spec §168) is a separate, later ticket.

### Accepted macOS residual (stated plainly)

On macOS, raw-socket-capable build code can reach host loopback services
and external egress directly. **No host-listener monitoring/guarding is
claimed or implied.** ADR 0003 Revision retired guarded loopback; the
2026-07-29 Seatbelt spike proved guarded executor loopback unimplementable
on macOS (IP-literal endpoints inexpressible, deny-beneath-allow inert,
IPv4/IPv6 asymmetry in `localhost:` rules). Such guarding returns only
with a future micro-VM executor ("Shape B" — Virtualization.framework
per-worktree micro-VM). The threat model is explicitly limited to
accidental harm, and this posture is reported, never described as
loopback protection.

### Provenance

`omac provenance` reports the build-executor network posture in a
`build executor` section (and a `build_executor` JSON object) that
clearly distinguishes:

- **Linux private loopback** (kernel boundary): network posture
  `kernel-blocked (private sandbox loopback)`, loopback boundary
  `kernel (network namespace)`, worker loopback `private sandbox
  loopback`, accepted residual `host-loopback services unreachable from
  the executor`.
- **macOS env-only filtering** (filesystem-only boundary): network
  posture `env-only filtered (filesystem confinement only)`, loopback
  boundary `filesystem-only`, worker loopback `works (no kernel network
  filter)`, accepted residual `raw-socket-capable build code can reach
  host loopback and external egress; no host-listener monitoring/guarding
  (ADR 0003 Revision)`.
- **Canonical checks** (same on both platforms): `yarp3 checkstyle twin
  tasks retired (OMAC init.d); canonical checkstyleMain/checkstyleTest
  run unchanged via Gradle Worker API`.

On no platform may a build executor be described as having a loopback
guarantee it does not have (spec §Network, 297).

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
