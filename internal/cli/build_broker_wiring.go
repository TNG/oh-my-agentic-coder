package cli

import (
	"io"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildbroker"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// brokerEngineInvoker returns a buildbroker.EngineInvoker that adapts
// accepted broker requests to buildengine.Run. The broker has already
// canonicalized and authorized the worktree; the adapter constructs the
// engine Options from the parent's resolved cache scope + auditor +
// proxy starter, wires the broker's graceful/force cancellation
// signals to the engine, and returns the engine's Result.
//
// The adapter does NOT own the cache scope or auditor — the parent
// resolves them once and passes them in, so a brokered build reuses the
// same cache scope and audit trail the parent already prepared. The
// engine's snapshot provider is the parent-owned snapshot for the
// authorized worktree (frozen at activation); the adapter does not
// write approvals or replace snapshots.
//
// The adapter is the production EngineInvoker the parent wires into the
// broker. Tests inject their own stub; this function is not exercised
// by the protocol tests (they use a fake invoker).
func brokerEngineInvoker(env *Env, cacheDir string, closeScope func(), auditor audit.Auditor) buildbroker.EngineInvoker {
	return func(worktree string, args []string, stdout, stderr io.Writer, graceful, force <-chan struct{}) buildengine.Result {
		return buildengine.Run(buildengine.Options{
			Workdir:     worktree,
			RawArgs:     args,
			Stdout:      stdout,
			Stderr:      stderr,
			CacheDir:    cacheDir,
			CloseScope:  closeScope,
			Auditor:     auditor,
			Proxies:     cliProxyStarter,
			Cancel:      graceful,
			ForceCancel: force,
			// Snapshot: nil selects DirectSnapshotProvider for now.
			// The parent-owned snapshot adapter is wired in a later
			// gate (ticket 06 freezes the active capability set in
			// parent memory). This gate uses the direct adapter so
			// the broker path behaves like the direct path: the gate
			// records approval on first use and returns a *GateError
			// when the manifest changed.
			Snapshot: buildengine.DirectSnapshotProvider,
		})
	}
}

// newBuildBroker constructs the host build broker with the production
// engine invoker. Both `start` and `serve` share this factory; only the
// Authorizer differs (StartAuthorizer for a single session worktree,
// ServeAuthorizer for multiple active directories). cacheDir is the
// resolved cache scope dir (empty when no scope is prepared). auditor
// is the parent's auditor. Returns (broker, nil) on success or
// (nil, err) on construction failure.
func newBuildBroker(token string, authorizer buildbroker.Authorizer, env *Env, cacheDir string, auditor audit.Auditor) (*buildbroker.Broker, error) {
	return buildbroker.New(buildbroker.Options{
		Token:         token,
		Authorizer:    authorizer,
		EngineInvoker: brokerEngineInvoker(env, cacheDir, nil, auditor),
		Auditor:       auditor,
	})
}

// canonicalWorktree resolves the canonical path of a worktree
// (filepath.EvalSymlinks after filepath.Abs). Used by the parent to
// build the start authorizer's session worktree. The parent
// canonicalizes its own workdir at launch so the authorizer compares
// canonical forms.
func canonicalWorktree(workdir string) (string, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// cacheScopeDirOrEmpty returns the cache scope dir, or empty when the
// scope is nil (no-sandbox / no-inner path). The build broker's engine
// invoker reuses this so brokered builds share the same cache scope as
// direct host invocation; empty is a valid "no cache scope prepared"
// sentinel the engine treats as "use the default shared scope".
func cacheScopeDirOrEmpty(scope *toolcache.Scope) string {
	if scope == nil {
		return ""
	}
	return scope.Dir
}

// injectBuildBrokerEnv injects the managed-build environment into the
// inner command's env map. The required marker is injected
// unconditionally (even when the broker is not mounted) so a
// misconfigured parent fails closed instead of falling back to nested
// local execution. The token is injected only when the broker is
// actually mounted on a loopback listener; a missing token with the
// marker present makes the CLI exit 10 with a restart/upgrade
// diagnostic (the fail-closed path).
func injectBuildBrokerEnv(extra map[string]string, mounted bool, token string) {
	extra["OMAC_BUILD_BROKER_REQUIRED"] = "1"
	if mounted && token != "" {
		extra["OMAC_BUILD_TOKEN"] = token
	}
}
