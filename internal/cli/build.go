package cli

import (
	"errors"
	"fmt"

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

// runBuild implements `omac build [--root <rel>] -- gradle <args...>`.
//
// Exit-code contract (also printed in the help text):
//
//	0                build success
//	gradle's code    build failure (wrapper exit code, incl. 128+n on signals)
//	3                policy denial (rejected before any build code ran)
//	4                cancellation (SIGINT/SIGTERM; staged shutdown, with the
//	                 "omac build: cancelled" marker on stderr)
//	10               service failure (sandbox unavailable, exec error, I/O;
//	                 10 not 1: Gradle's own build-failure code IS 1)
func runBuild(args []string, env *Env) int {
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

	grants, err := buildrun.GrantsFor(resolved.Worktree, cacheDir)
	if err != nil {
		return failService("derive executor grants: %v", err)
	}
	defer grants.CleanupTmp()

	// Audit: open the persistent trail best-effort (a build must never
	// fail because the audit log is unavailable; config strictness is the
	// start/serve path's concern).
	auditor := buildAuditor(env)
	defer auditor.Close()
	auditor.Emit(audit.ControlMutation("build.request", resolved.Worktree,
		fmt.Sprintf("adapter=gradle root=%s args=%d", resolved.ProjectDir, len(resolved.Args))))

	cancel, requestForce, release := buildrun.SignalContext()
	defer release()

	code, err := buildrun.RunBuild(buildrun.RunOptions{
		Resolved: resolved,
		Grants:   grants,
		Stdout:   env.Stdout,
		Stderr:   env.Stderr,
		Cancel:   cancel,
		Auditor:  auditor,
	})
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build: %v\n", err)
		return buildrun.ExitServiceFailure
	}
	// Second signal (the "get out NOW" gesture): do not sleep again — a
	// second RunBuild is never started, so the next line is the whole
	// urgent-exit behavior. RunBuild already honors the (possibly
	// collapsed) staging and has run all deferred cleanups above, so a
	// raw os.Exit here would skip them.
	_ = requestForce
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
  omac build [--root <rel>] -- gradle <args...>

The gradle adapter token is required (literal; Maven: "unsupported adapter").
OMAC resolves <root>/gradlew under the canonical worktree and runs it with
the build's real arguments passed through unchanged. Output streams through;
SIGINT/SIGTERM cancels with graceful-then-kill staged shutdown.

Executor authority (one restricted process per request):
  read+write: current worktree, resolved OMAC cache leaf
              (GRADLE_USER_HOME = <cache scope>/gradle), private temp
  network:    fully blocked (no proxy endpoints in v0; configuration-only
              tasks such as :help work, dependency downloads do not)
  denied:     host ~/.gradle, host secrets, SSH/AWS state, OMAC config

Exit codes:
  0            build success
  <gradle>     build failure — the wrapper's own exit code (128+n on signal)
  3            policy denial — rejected before any build code ran
               (grammar/adapter error, root outside the worktree, symlink
               escape, missing or non-executable gradlew)
  4            cancellation — SIGINT/SIGTERM honored during the build;
               distinct from a raw "gradle exit 4" by the
               "omac build: cancelled" marker on stderr
  10           service failure — OMAC-side error (sandbox unavailable,
               exec failure); 10 rather than 1 because Gradle's own
               build-failure code IS 1; diagnostic is omac-prefixed on
               stderr

Cold-cache note (v0): network is fully blocked inside the executor, so
the Gradle distribution must already be RESOLVABLE under the cache leaf
(GRADLE_USER_HOME = <cache scope>/gradle) — warm from a previous build
or pre-seeded by a host run. A cold cache cannot bootstrap the wrapper
distribution (distribution download is egress). Pre-seed once on the
host, then "omac build" reuses it offline.`)
}
