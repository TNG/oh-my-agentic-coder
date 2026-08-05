package cli

import (
	"fmt"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// runBuildStop implements `omac build stop`: stop any Gradle daemon
// lingering for this worktree and release the per-worktree queue lockfile.
//
// The CLI owns the `--help` short-circuit and the local help rendering;
// the orchestration (parse --root, resolve the wrapper, run `gradlew
// --stop` under the same isolated env as the build, force-kill lingering
// wedged daemons, remove the lockfile) lives in internal/buildengine.Stop,
// called by both this direct-host path and the future brokered path.
//
// Exit codes mirror `omac build`: 0 on success, 10 on service failure, 3
// on policy denial (e.g. missing wrapper). The Gradle --stop exit code
// passes through as a build_failure.
func runBuildStop(args []string, env *Env) int {
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" {
			fmt.Fprintln(env.Stderr, `omac build stop — stop any lingering Gradle daemon for this worktree

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

	cacheDir, closeScope, err := prepareBuildCache(env.Workdir, "")
	if err != nil {
		fmt.Fprintf(env.Stderr, "omac build stop: resolve cache scope: %v\n", err)
		return buildrun.ExitServiceFailure
	}

	auditor := buildAuditor(env)
	defer auditor.Close()

	result := buildengine.Stop(buildengine.StopOptions{
		Workdir:    env.Workdir,
		RawArgs:    args,
		Stdout:     env.Stdout,
		Stderr:     env.Stderr,
		CacheDir:   cacheDir,
		CloseScope: closeScope,
		Auditor:    auditor,
	})

	// Exit-code translation: the engine assigns the explicit class at
	// the outcome site; the CLI translates it to the documented exit
	// code. Policy-denial and service-failure diagnostics are rendered
	// omac-prefixed here (the engine does not print them — it stays
	// transport-neutral).
	switch result.Class {
	case buildengine.ClassPolicyDenial:
		if result.Err != nil {
			fmt.Fprintf(env.Stderr, "omac build stop: %v\n", result.Err)
		}
	case buildengine.ClassServiceFailure:
		if result.Err != nil {
			fmt.Fprintf(env.Stderr, "omac build stop: %v\n", result.Err)
		}
	}
	return result.ExitCode()
}
