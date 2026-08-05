package cli

import (
	"io"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildbroker"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
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

// _ keeps the buildrun import referenced for future wiring (the engine
// invoker passes through to buildengine.Run which uses buildrun; the cli
// wiring uses buildrun.ExitServiceFailure for diagnostics).
var _ = buildrun.ExitServiceFailure
