package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// runBuildStop implements `omac build stop`: tear down the warm Gradle
// daemon for this worktree and release the per-worktree queue lockfile.
//
// The "warm executor" is Gradle's own daemon persisting under the
// session-scoped GRADLE_USER_HOME leaf (no long-lived omac supervisor).
// `stop` runs the repo wrapper with `--stop` under the SAME restricted
// env as the build (S6: isolated ChildEnv — no host HOME, no host
// ~/.gradle, no host creds; GRADLE_USER_HOME=<leaf>; JDK-resolved
// PATH/JAVA_HOME) so Gradle stops its daemons for this worktree, then
// force-kills any wedged daemon that ignored the cooperative stop (S7).
// Finally it removes the lockfile a crashed `omac build` may have left.
//
// Exit codes mirror `omac build`: 0 on success, 10 on service failure, 3
// on policy denial (e.g. missing wrapper). The Gradle --stop exit code
// passes through.
func runBuildStop(args []string, env *Env) int {
	failService := func(format string, args ...any) int {
		fmt.Fprintf(env.Stderr, "omac build stop: "+format+"\n", args...)
		return buildrun.ExitServiceFailure
	}
	deny := func(err error) int {
		fmt.Fprintf(env.Stderr, "omac build stop: %v\n", err)
		return ExitBuildPolicyDenied
	}

	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" {
			fmt.Fprintln(env.Stderr, `omac build stop — stop the warm Gradle daemon for this worktree

Usage:
  omac build stop [--root <rel>]

Runs the repo wrapper with 'gradle --stop' under the session-scoped
GRADLE_USER_HOME leaf (same isolated env as the build: no host HOME, no
host ~/.gradle, no host creds) so Gradle stops its daemons for this
worktree. --root <rel> resolves the wrapper at <worktree>/<rel>/gradlew
(default ".", the worktree root) — the same root the build path uses.
Then force-kills any wedged daemon that ignored the cooperative stop.
Finally removes the per-worktree queue lockfile. A clean 'omac build'
already releases its flock; 'stop' is for teardown after the session
ends or after a crashed build that left the lockfile on disk (the
kernel released the flock on crash, so removal is safe).`)
			return ExitOK
		}
	}

	// Parse --root from the user's args (before any `--`), mirroring
	// buildrun.ParseArgs. The hardcoded "." was the ticket-04 host bug:
	// `omac build stop --root backend` resolved the wrapper at the
	// worktree root instead of backend/, failing with "no repository-owned
	// gradlew at <worktree>/gradlew". We accept `--root <rel>` and
	// `--root=<rel>`; any other flag is a policy denial (same as
	// `omac build`). There is no adapter token here — we synthesize
	// `-- gradle --stop` after extracting the root.
	root := "."
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--root":
			if i+1 >= len(args) {
				return deny(errors.New("--root requires a value"))
			}
			root = args[i+1]
			i++
		case strings.HasPrefix(a, "--root="):
			root = strings.TrimPrefix(a, "--root=")
		case a == "--":
			// Anything after `--` is the adapter token + pass-through;
			// `stop` owns those, so ignore further flags here.
			i = len(args)
		default:
			return deny(fmt.Errorf("unknown flag %q (usage: omac build stop [--root <rel>])", a))
		}
	}
	if root == "" {
		return deny(errors.New("--root must not be empty"))
	}

	stopArgs := []string{"--root", root, "--", "gradle", "--stop"}
	req, err := buildrun.ParseArgs(stopArgs)
	if err != nil {
		return deny(err)
	}
	resolved, err := buildrun.Resolve(env.Workdir, req)
	if err != nil {
		return deny(err)
	}

	cacheDir, closeScope, err := prepareBuildCache(env.Workdir, "")
	if err != nil {
		return failService("resolve cache scope: %v", err)
	}
	defer closeScope()

	// P7: the leaf name belongs to buildrun, not cli. Reuse the same
	// helper GrantsFor uses so stop and build resolve the same leaf.
	leaf := buildrun.GradleLeaf(cacheDir)

	auditor := buildAuditor(env)
	defer auditor.Close()
	auditor.Emit(audit.ControlMutation("build.stop", resolved.Worktree, "gradle --stop"))

	// S6 + S7: run --stop under the SAME isolated env as the build (no
	// host HOME, no host ~/.gradle, no host creds — the spec executor
	// boundary), then force-kill lingering wedged daemons for the leaf.
	// We build a Grants here so the isolated ChildEnv (JDK-resolved
	// PATH/JAVA_HOME, proxy GRADLE_OPTS if configured) is reused; the
	// kernel sandbox is NOT applied to --stop (it signals a daemon
	// across the process boundary) — documented in docs/build-command.md.
	grants, err := buildrun.GrantsFor(resolved.Worktree, cacheDir, buildrun.BuildConfig{})
	if err != nil {
		// A Grants derivation failure is non-fatal for stop: fall back to
		// the minimal leaf-only env (still no HOME — the spec-critical
		// part) and run the cooperative stop. The force-kill fallback
		// still runs.
		grants = nil
	}
	if grants != nil {
		defer grants.CleanupTmp()
	}
	if err := buildrun.StopGradleDaemon(buildrun.StopDaemonOptions{
		Wrapper:    resolved.Wrapper,
		ProjectDir: resolved.ProjectDir,
		Leaf:       leaf,
		Grants:     grants,
		Stdout:     env.Stdout,
		Stderr:     env.Stderr,
	}); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		return failService("gradle --stop: %v", err)
	}

	// Release the queue lockfile: a clean build released its flock on
	// exit, so the file only lingers after a crash. The kernel already
	// released the flock (flock is per-process; the crashed process is
	// gone), so removing the file is safe — the next Acquire recreates it.
	lockPath := filepath.Join(leaf, buildrun.BuildLockName)
	if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
		// Non-fatal: the daemon stop succeeded; a lingering lockfile the
		// next build can still acquire (kernel flock is released) is not
		// worth failing the teardown over.
		fmt.Fprintf(env.Stderr, "omac build stop: warning: could not remove lockfile %s: %v\n", lockPath, err)
	}
	fmt.Fprintf(env.Stdout, "omac build stop: stopped Gradle daemons for %s and released the queue lock\n", resolved.Worktree)
	return ExitOK
}
