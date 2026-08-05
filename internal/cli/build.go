package cli

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// `omac build` reservation of omac-owned exit codes (build success 0 and
// arbitrary build-failure codes pass through from the wrapper):
const (
	// ExitBuildPolicyDenied marks requests OMAC rejected before any build
	// code ran: grammar/adapter errors, worktree escapes, wrapper
	// validation failures. Distinct from a Gradle failure.
	ExitBuildPolicyDenied = 3
	// ExitBuildCancelled marks a caller-cancelled build.
	ExitBuildCancelled = 4
)

// buildStopSub is the literal subcommand dispatched to runBuildStop.
const buildStopSub = "stop"

// runBuild implements `omac build [--root <rel>] -- gradle <args...>` and
// `omac build stop`.
//
// The CLI owns public command dispatch (the `stop` subcommand route, the
// `--help` short-circuit), local help rendering (printBuildUsage),
// managed-vs-direct mode selection (decideManagedMode), signal handling
// (SignalContext for direct, signal→cancel POST for managed), and
// exit-code translation. The build orchestration — manifest gating,
// cache-leaf preparation, proxy startup, grants derivation, per-leaf
// locking, restricted-executor launch, staged cancellation, post-build
// daemon recycle, and cleanup — lives in internal/buildengine, called
// by both this direct-host path and the brokered path (which submits to
// the parent's buildbroker over the loopback control plane).
//
// Exit-code contract (also printed in the help text):
//
//	0                build success
//	gradle's code    build failure (wrapper exit code, incl. 128+n on signals)
//	3                policy denial (rejected before any build code ran)
//	4                cancellation (SIGINT/SIGTERM; staged shutdown, with the
//	                 "omac build: cancelled" marker on stderr)
//	10               service failure (sandbox unavailable, exec error, I/O,
//	                 queue busy; 10 not 1: Gradle's own build-failure code IS 1)
func runBuild(args []string, env *Env) int {
	// `omac build stop --help` renders locally without a broker (the
	// stop subcommand owns its help). The `stop` subcommand dispatch
	// happens AFTER the managed-mode check so a managed `omac build
	// stop` goes through the broker (which refuses stop in this gate).
	// The direct path dispatches `stop` to runBuildStop below.
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" {
			printBuildUsage(env)
			return ExitOK
		}
	}

	// Managed-vs-direct mode selection. In a managed OMAC session
	// (OMAC_BUILD_BROKER_REQUIRED=1 + OMAC_CONTROL_BASE +
	// OMAC_BUILD_TOKEN) the CLI submits to the parent's broker; on
	// the host it runs the build engine in-process. A partial broker
	// tuple or any partial OMAC session env fails closed with exit 10
	// so a truncated/partial broker environment is never mistaken for
	// build success. Managed invocation never falls back to nested
	// local execution.
	mode, ep := decideManagedMode()
	switch mode {
	case managedModeFailClosed:
		fmt.Fprintln(env.Stderr, "omac build: managed build required but the broker environment is incomplete (OMAC_BUILD_BROKER_REQUIRED set without OMAC_CONTROL_BASE/OMAC_BUILD_TOKEN, or partial OMAC session env). Restart or upgrade the omac parent.")
		return buildrun.ExitServiceFailure
	case managedModeManaged:
		return runBuildManaged(args, env, ep)
	}

	// Direct host execution: dispatch `omac build stop` here (after the
	// managed check, so a managed `omac build stop` is brokered).
	if len(args) > 0 && args[0] == buildStopSub {
		return runBuildStop(args[1:], env)
	}

	// Cache scope + auditor: the CLI owns the launcher-config resolution
	// (prepareBuildCache reuses the start path's scope machinery) and the
	// audit-trail construction (buildAuditor). The engine consumes the
	// resolved cache dir + auditor as inputs — it does not touch the
	// launcher config or the audit-sink config directly.
	cacheDir, closeScope, err := prepareBuildCache(env.Workdir, "")
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build: resolve cache scope: %v\n", err)
		return buildrun.ExitServiceFailure
	}

	// Signal handling: the CLI owns the staged graceful-then-forced
	// cancellation wiring (SignalContext). The engine consumes the
	// cancel + force channels as inputs; it does not install signal
	// handlers (a transport-neutral engine cannot assume it owns the
	// process's signal disposition — the broker path delivers
	// cancellation via HTTP, not signals).
	cancel, force, _, release := buildrun.SignalContext()
	defer release()

	auditor := buildAuditor(env)
	defer auditor.Close()

	result := buildengine.Run(buildengine.Options{
		Workdir:     env.Workdir,
		RawArgs:     args,
		Stdout:      env.Stdout,
		Stderr:      env.Stderr,
		CacheDir:    cacheDir,
		CloseScope:  closeScope,
		Auditor:     auditor,
		Proxies:     cliProxyStarter,
		Cancel:      cancel,
		ForceCancel: force,
	})

	// Exit-code translation: the engine assigns the explicit class at
	// the outcome site; the CLI translates it to the documented exit
	// code. Policy-denial and service-failure diagnostics are rendered
	// omac-prefixed here (the engine does not print them — it stays
	// transport-neutral; the broker frames them as a sanitized
	// service-failure result instead).
	switch result.Class {
	case buildengine.ClassPolicyDenial:
		if result.Err != nil {
			fmt.Fprintf(env.Stderr, "omac build: %v\n", result.Err)
		}
	case buildengine.ClassServiceFailure:
		if result.Err != nil {
			fmt.Fprintf(env.Stderr, "omac build: %v\n", result.Err)
		}
	}
	return result.ExitCode()
}

// prepareBuildCache resolves the launcher config's cache scope for workdir
// and prepares (locks + creates) the corresponding persistent cache dir.
// Returns the scope dir and a release func. This REUSES the start path's
// scope machinery (resolveCacheScope + prepareLaunchCache) so Gradle state
// follows the single configured cache scope exactly and cannot drift from
// what `omac start`/`serve` lay out.
func prepareBuildCache(workdir, scopeOverride string) (string, func(), error) {
	lc, cfgPath, err := config.LoadLauncher(workdir)
	if err != nil {
		return "", nil, err
	}
	scope, err := resolveCacheScope(lc.Cache, scopeOverride)
	if err != nil {
		return "", nil, err
	}
	// Same preparation as `omac start`'s persistent path: sandboxed launch
	// (noSandbox=false), never ephemeral (build has no ephemeral variant
	// in v0; sandboxTmp is only read by the ephemeral branch and stays
	// empty here).
	ts, err := prepareLaunchCache(false, false, scope, workdir, cfgPath, "")
	if err != nil {
		return "", nil, err
	}
	return ts.Dir, func() { _ = ts.Close() }, nil
}

// buildAuditor constructs the audit trail writer for a build invocation.
// Best-effort: disabled or unavailable sinks degrade to Nop.
func buildAuditor(env *Env) audit.Auditor {
	lc, _, err := config.LoadLauncher(env.Workdir)
	if err == nil && !lc.Audit.AuditEnabled() {
		return audit.Nop()
	}
	cfg := audit.Config{
		Enabled: true,
		Mode:    audit.ModeBuild,
		Version: env.Version,
	}
	if err == nil {
		cfg.Path = lc.Audit.Path
		cfg.Syslog = lc.Audit.Syslog
	}
	a, err := audit.New(cfg)
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build: warning: audit log unavailable (%v)\n", err)
		return audit.Nop()
	}
	return a
}

func printBuildUsage(env *Env) {
	fmt.Fprintln(env.Stderr, `omac build — run a repository-owned Gradle build inside the restricted JVM build executor

Usage:
  omac build [--root <rel>] [--max-duration <duration>] -- gradle <args...>
  omac build stop                    stop any lingering Gradle daemon for this worktree

The gradle adapter token is required (literal; Maven: "unsupported adapter").
OMAC resolves <root>/gradlew under the canonical worktree and runs it with
the build's real arguments passed through unchanged. Output streams through;
SIGINT/SIGTERM cancels with a graceful-then-kill staged shutdown (a second
signal forces the kill immediately AND recycles the Gradle daemon).

Daemon lifecycle (cold start per build):
  Each "omac build" spawns a fresh gradlew client against the session-scoped
  leaf (<cache scope>/gradle). When the build finishes (or is forced-cancelled),
  OMAC recycles the daemon via "gradlew --stop" — not as a separate step, and
  never via --no-daemon (forbidden) — so every build starts COLD with fresh
  env, fresh init scripts, and fresh JUnit Platform listener discovery. The
  ~10s cold start per build is the price of correctness with Testcontainers +
  embedded Kafka. "omac build stop" is still available for a wedged daemon
  that ignored --stop.

Queue (per-worktree serialization, individually cancellable):
  Each invocation takes an exclusive flock on <leaf>/.omac-build.lock,
  released on exit (auto-released on crash). Same worktree serializes;
  independent worktrees resolve to independent leaves (independent locks)
  and run concurrently. A queued request is individually cancellable: a
  second "omac build" Ctrl-C unwinds a waiter without killing the running
  build (cancelled-while-waiting -> exit 4 + marker); a 30s timeout
  waiting for a busy lock -> exit 10 ("another build is running").

Executor authority (one restricted process per request):
  read+write: current worktree, resolved OMAC cache leaf
               (GRADLE_USER_HOME = <cache scope>/gradle), private temp
  read-only:  the real JDK bin+lib (jenv/asdf shims bypassed), OMAC
               control state (gradle.properties, .omac-control/, init.d/) —
               readable by Gradle but NOT writable by build/test code, so
               the OMAC-imposed proxy/JVM guardrails cannot be relaxed
               (writes surface as EPERM; see .omac-control/README for the
               supported alternatives)
  network:    macOS — env-only filtered via the omac proxy (GRADLE_OPTS,
               NEVER JAVA_TOOL_OPTIONS which the JVM prints, leaking tokens);
               loopback is excluded so the Gradle daemon's worker protocol
               works — macOS is filesystem-confinement only, NO kernel
               network mediation (Shape A; raw-socket-capable build code can
               reach host loopback and external egress — no host-listener
               monitoring/guarding is claimed, ADR 0003 Revision). Linux —
               kernel-blocked (private sandbox loopback).
  worker checks: canonical checkstyleMain/checkstyleTest run unchanged via
               the Gradle Worker API on both platforms; yarp3's
               checkstyle*Sandbox twin tasks are retired by the OMAC-authored
               read-only init.d/retire-checkstyle-twins.gradle (defensive
               no-op when no twins exist). No host init script required.
  containers: the executor gets a FILTERED Docker endpoint (DOCKER_HOST
               points at a loopback HTTP proxy, NEVER the raw daemon socket)
               only when the approved manifest declares container images
               (macOS v1; Linux kernel-blocked → no proxy). Approved images
               only; host bind mounts, socket nesting, privileged mode, host
               namespaces, devices, extra capabilities, and unsafe security
               options are denied with structured OMAC errors. Published ports
               bind to 127.0.0.1; containers attach to an executor-owned
               internal network with no outbound route. Testcontainers Ryuk
               is disabled (TESTCONTAINERS_RYUK_DISABLED=true). Executor-
               owned containers + network are removed on normal completion
               and cancellation.
  denied:     host ~/.gradle, host secrets, SSH/AWS state, OMAC config

JDK resolution:
  jenv/asdf/SDKMAN shims break under deny-default Seatbelt (/dev/fd process
  substitution denied), so OMAC resolves the REAL JDK (follows symlink chains,
  rejects shim shell scripts; /usr/libexec/java_home is a FALLBACK only — it
  pointed at a nonexistent JDK on a test host) and sets JAVA_HOME + PATH to
  the real JDK bin, granting its bin+lib read access.

Resource ceilings:
  org.gradle.jvmargs=-Xmx in the OMAC-generated gradle.properties bounds the
  Gradle daemon JVM heap (default 2g; overridable). --max-duration <duration>
  (before --) bounds the total build wall-clock; an over-budget run is
  cancelled as if the caller signalled. A non-positive/unparseable value is
  rejected before executor startup (excessive request -> exit 3).

Cancellation (two stages):
  First SIGINT/SIGTERM  — graceful: SIGTERM the group, SIGKILL after the
                         window.
  Second signal /       — forced: collapse the window, SIGKILL the group,
  --max-duration expiry   AND RECYCLE the (possibly corrupt) Gradle daemon
                           (best-effort gradlew --stop against the leaf).
  In both cases the daemon serving the build is recycled post-build via
  "gradlew --stop"; the next build starts cold.

Exit codes:
  0            build success
  <gradle>     build failure — the wrapper's own exit code (128+n on signal)
  3            policy denial — rejected before any build code ran
                (grammar/adapter error, root outside the worktree, symlink
                escape, missing or non-executable gradlew, bad --max-duration)
  4            cancellation — SIGINT/SIGTERM honored during the build, OR a
                queued request cancelled while waiting for the lock; distinct
                from a raw "gradle exit 4" by the "omac build: cancelled"
                marker on stderr
  10           service failure — OMAC-side error (sandbox unavailable, exec
                failure, queue busy after 30s); 10 rather than 1 because
                Gradle's own build-failure code IS 1; diagnostic is
                omac-prefixed on stderr

omac build stop:
  Runs the repo wrapper with "gradle --stop" under the SAME isolated env as
  the build (no host HOME, no host ~/.gradle, no host creds) so Gradle stops
  its daemons for this worktree, then force-kills any wedged daemon that
  ignored the cooperative stop, then removes the per-worktree queue lockfile.
  Use after the session ends or to clean up a lockfile left by a crashed
  build (the kernel released the flock on crash, so removal is safe).

Cold-cache note: the Gradle distribution must already be resolvable under
the cache leaf — cached from a previous build in the same scope or
pre-seeded by a host run.`)
}

// newBuildRequestID delegates to buildrun.NewBuildRequestID — the single
// source of truth. Retained as a thin wrapper so existing cli tests
// (build_test.go's TestBuildExecutorSecurityBoundary) that reference the
// cli-qualified name keep compiling; the wrapper has no behavior of its
// own.
func newBuildRequestID() string { return buildrun.NewBuildRequestID() }
