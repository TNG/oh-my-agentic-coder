package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
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

// buildStopToken is the literal subcommand dispatched to runBuildStop.
const buildStopSub = "stop"

// runBuild implements `omac build [--root <rel>] -- gradle <args...>` and
// `omac build stop`.
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
	// `omac build stop` tears down the warm daemon for this worktree.
	if len(args) > 0 && args[0] == buildStopSub {
		return runBuildStop(args[1:], env)
	}

	deny := func(err error) int {
		fmt.Fprintf(env.Stderr, "omac build: %v\n", err)
		return ExitBuildPolicyDenied
	}
	failService := func(format string, args ...any) int {
		fmt.Fprintf(env.Stderr, "omac build: "+format+"\n", args...)
		return buildrun.ExitServiceFailure
	}

	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" {
			printBuildUsage(env)
			return ExitOK
		}
	}

	req, err := buildrun.ParseArgs(args)
	if err != nil {
		var reqErr *buildrun.RequestError
		if errors.As(err, &reqErr) {
			return deny(reqErr)
		}
		return deny(err)
	}
	resolved, err := buildrun.Resolve(env.Workdir, req)
	if err != nil {
		var reqErr *buildrun.RequestError
		if errors.As(err, &reqErr) {
			return deny(reqErr)
		}
		return failService("resolve: %v", err)
	}

	// GRADLE_USER_HOME derives from the resolved OMAC cache scope
	// (global/config/workdir per the launcher config), prepared through
	// toolcache — permissions + shared-lock handled there. Never
	// hardcoded, never host ~/.gradle.
	cacheDir, closeScope, err := prepareBuildCache(env.Workdir, "")
	if err != nil {
		return failService("resolve cache scope: %v", err)
	}
	defer closeScope()

	// Proxy: start the omac filtered proxy so public dependency resolution
	// works without printing a proxy password (GRADLE_OPTS, NEVER
	// JAVA_TOOL_OPTIONS). Best-effort configurable but ON by default for
	// the build path on macOS (Shape A). On Linux the kernel-blocked
	// posture makes the proxy unreachable, so it is not started.
	proxyURL, proxyPort, stopProxy, proxyErr := startBuildProxy(env)
	if proxyErr != nil {
		return failService("build proxy: %v", proxyErr)
	}
	if stopProxy != nil {
		defer stopProxy()
	}

	grants, err := buildrun.GrantsFor(resolved.Worktree, cacheDir, buildrun.BuildConfig{
		ProxyURL:  proxyURL,
		ProxyPort: proxyPort,
	})
	if err != nil {
		return failService("derive executor grants: %v", err)
	}
	defer grants.CleanupTmp()

	// Per-worktree queue: serialize `omac build` invocations in the same
	// worktree (they share a warm Gradle daemon and would corrupt each
	// other's cache). Independent worktrees resolve to independent leaves
	// (independent lockfiles) → concurrent. The flock is auto-released on
	// crash (kernel releases flock when the process dies); no stale-lock
	// cleanup is needed.
	//
	// The acquire is CANCELLABLE (S2: spec.md:136 — queued requests are
	// individually cancellable): the build's cancel channel is wired in
	// so a second `omac build` Ctrl-C unwinds a waiter without killing
	// the running build. SignalContext is therefore created BEFORE the
	// acquire so the cancel channel exists while we wait for the lock.
	cancel, force, _, release := buildrun.SignalContext()
	defer release()

	lock, err := buildrun.AcquireCtx(grants.GradleUserHome(), buildrun.DefaultQueueTimeout, cancel)
	if err != nil {
		if errors.Is(err, buildrun.ErrLockCancelled) {
			// Cancelled while queued: ExitCancelled (4) + marker, not the
			// ExitServiceFailure (10) a busy-denial produces.
			fmt.Fprintln(env.Stderr, buildrun.CancelledMarker)
			return ExitBuildCancelled
		}
		return failService("%v", err)
	}
	defer lock.Release()

	// Audit: open the persistent trail best-effort (a build must never
	// fail because the audit log is unavailable; config strictness is the
	// start/serve path's concern).
	auditor := buildAuditor(env)
	defer auditor.Close()
	auditor.Emit(audit.ControlMutation("build.request", resolved.Worktree,
		fmt.Sprintf("adapter=gradle root=%s args=%d", resolved.ProjectDir, len(resolved.Args))))

	maxDur := req.MaxDuration
	// S3: a forced cancel (second signal / MaxDuration expiry) SIGKILLs
	// the gradlew group, but the Gradle daemon (a separate process
	// outside the group) survives with potentially-corrupt state. Recycle
	// it by running `gradlew --stop` against the leaf (best-effort — a
	// wedged daemon may need manual `omac build stop`). Graceful cancel
	// (first signal) does NOT recycle the daemon, preserving the warm
	// executor per spec §144.
	daemonRecycle := func(stderr io.Writer) error {
		return buildrun.StopGradleDaemon(buildrun.StopDaemonOptions{
			Wrapper:    resolved.Wrapper,
			ProjectDir: resolved.ProjectDir,
			Leaf:       grants.GradleUserHome(),
			Grants:     grants,
			Stderr:     stderr,
		})
	}
	code, err := buildrun.RunBuild(buildrun.RunOptions{
		Resolved:       resolved,
		Grants:         grants,
		Stdout:         env.Stdout,
		Stderr:         env.Stderr,
		Cancel:         cancel,
		ForceCancel:    force,
		MaxDuration:    maxDur,
		OnForcedCancel: daemonRecycle,
		Auditor:        auditor,
	})
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build: %v\n", err)
		return buildrun.ExitServiceFailure
	}
	return code
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
  omac build stop                    stop the warm Gradle daemon for this worktree

The gradle adapter token is required (literal; Maven: "unsupported adapter").
OMAC resolves <root>/gradlew under the canonical worktree and runs it with
the build's real arguments passed through unchanged. Output streams through;
SIGINT/SIGTERM cancels with a graceful-then-kill staged shutdown (a second
signal forces the kill immediately AND recycles the Gradle daemon).

Warm executor (Gradle daemon reuse):
  Each "omac build" spawns a fresh gradlew process, but GRADLE_USER_HOME is
  a stable session-scoped leaf (<cache scope>/gradle), so Gradle keeps its
  daemon alive in that leaf and reuses it across invocations — no fresh
  startup per red-green cycle. No long-lived omac supervisor process; the
  daemon lingers by Gradle's idle-stop policy until "omac build stop" or
  idle-stop.

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
              works. Linux — kernel-blocked (warm-daemon cohabitation is a
              later Linux-validation item).
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
                         window; PRESERVE the warm Gradle daemon (spec §144).
  Second signal /       — forced: collapse the window, SIGKILL the group,
  --max-duration expiry   AND RECYCLE the (possibly corrupt) Gradle daemon
                          (best-effort gradlew --stop against the leaf).

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
the cache leaf — warm from a previous build or pre-seeded by a host run.`)
}
