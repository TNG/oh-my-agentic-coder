package cli

import (
	"io"
	"path/filepath"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildbroker"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildengine"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
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
func brokerEngineInvoker(env *Env, cacheDir string, closeScope func(), auditor audit.Auditor, snapshot buildengine.SnapshotProvider) buildbroker.EngineInvoker {
	return func(worktree string, args []string, stdout, stderr io.Writer, graceful, force <-chan struct{}) buildengine.Result {
		return buildengine.Run(buildengine.Options{
			Workdir:     worktree,
			RawArgs:     args,
			Stdout:      stdout,
			Stderr:      stderr,
			CacheDir:    cacheDir,
			CacheRoot:   buildControlCacheRoot(cacheDir),
			CloseScope:  closeScope,
			Auditor:     auditor,
			Proxies:     cliProxyStarter,
			Cancel:      graceful,
			ForceCancel: force,
			// Snapshot: the parent-owned snapshot provider is wired by
			// the parent (start/serve) and passed in here. When nil,
			// the engine falls back to DirectSnapshotProvider (the
			// gate-3 behavior the broker invoker originally had). The
			// parent-owned snapshot (ticket 06) freezes the active
			// capability set in parent memory; the engine cannot
			// advance or replace it.
			Snapshot: snapshot,
		})
	}
}

// newBuildBroker constructs the host build broker with the production
// engine invoker. Both `start` and `serve` share this factory; only the
// Authorizer differs (StartAuthorizer for a single session worktree,
// ServeAuthorizer for multiple active directories). cacheDir is the
// resolved cache scope dir (empty when no scope is prepared). auditor
// is the parent's auditor. snapshot is the parent-owned snapshot
// provider (nil selects DirectSnapshotProvider — the gate-3 fallback).
// Returns (broker, nil) on success or (nil, err) on construction
// failure.
func newBuildBroker(token string, authorizer buildbroker.Authorizer, env *Env, cacheDir string, auditor audit.Auditor, snapshot buildengine.SnapshotProvider) (*buildbroker.Broker, error) {
	return buildbroker.New(buildbroker.Options{
		Token:         token,
		Authorizer:    authorizer,
		EngineInvoker: brokerEngineInvoker(env, cacheDir, nil, auditor, snapshot),
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

// buildControlCacheRoot returns the shared cache root (parent of
// cache-scope dirs) under which the host-only build-control root lives,
// or empty when cacheDir is empty (no scope prepared). The engine uses
// this to acquire the leaf-keyed persistent lock at
// <cacheRoot>/build-control/locks/<sha256(leaf)>.lock before any
// mutable control state, proxy startup, or execution (ticket 06).
// Empty makes the engine fall back to the legacy in-leaf lock.
func buildControlCacheRoot(cacheDir string) string {
	return buildcontrol.CacheRootFromCacheDir(cacheDir)
}

// buildControlApprovalLocation returns a buildmanifest.Location that
// stores durable approval records under the host-only build-control
// root, namespaced by canonical worktree. cacheDir is the resolved
// cache scope dir; worktree is the canonical (EvalSymlinks-resolved)
// worktree root. Returns the legacy OnLeaf location when cacheDir is
// empty (behavior-preserving fallback).
func buildControlApprovalLocation(cacheDir, canonicalWorktree string) buildmanifest.Location {
	root := buildControlCacheRoot(cacheDir)
	if root == "" {
		return buildmanifest.NewOnLeafLocation()
	}
	return buildmanifest.NewBuildControlLocation(root, canonicalWorktree)
}

// startSnapshotProvider returns a SnapshotProvider for the `start`
// parent. The parent freezes the in-memory capability snapshot for
// its single session worktree before launching the inner process,
// reading the durable approval record from the host-only
// build-control root (ticket 06). When no durable approval exists OR
// the worktree manifest's current digest does not match the durable
// approval, the snapshot is left unset and the engine surfaces a host
// diagnostic requiring `omac build approve` + parent restart (build
// unavailable for this directory).
//
// The provider reads the durable approval record at activation time
// (here, at parent construction) and freezes the snapshot in a
// ParentSnapshotStore; the engine's per-invocation lookup is a pure
// in-memory read. A changed manifest cannot update the snapshot or
// activate before explicit host approval + parent restart.
func startSnapshotProvider(canonicalWorktree, cacheDir string) buildengine.SnapshotProvider {
	store := buildengine.NewParentSnapshotStore()
	if canonicalWorktree != "" && cacheDir != "" {
		freezeSnapshotFromDurableApproval(store, canonicalWorktree, cacheDir)
	}
	return store.ParentSnapshotProvider()
}

// freezeSnapshotFromDurableApproval reads the durable approval record
// for canonicalWorktree from the host-only build-control root and, if
// it exists and its digest matches the worktree manifest's current
// digest, freezes a ParentSnapshot. When the manifest is absent (the
// normal standard-Gradle-project case), a zero snapshot is frozen so
// builds proceed with default capabilities. When a manifest is present
// but no durable approval exists OR the digests mismatch, no snapshot
// is frozen — the engine surfaces a host diagnostic requiring `omac
// build approve` + parent restart.
func freezeSnapshotFromDurableApproval(store *buildengine.ParentSnapshotStore, canonicalWorktree, cacheDir string) {
	loc := buildControlApprovalLocation(cacheDir, canonicalWorktree)
	leaf := filepath.Join(cacheDir, "gradle")
	// Load the worktree manifest to compute the current digest. A
	// missing manifest is the normal standard-Gradle case: freeze a
	// zero snapshot so builds proceed with defaults.
	manifest, err := buildmanifest.Load(canonicalWorktree)
	if err != nil {
		// A malformed manifest is a policy denial at build time; do
		// not freeze a snapshot (build unavailable with the manifest
		// error surfaced by the engine).
		return
	}
	host := buildrun.HostPolicy(0) // start freezes the default ceiling; --max-duration is per-request
	if !manifest.HasManifest() {
		store.FreezeFromApproval(canonicalWorktree, "", buildmanifest.CapabilitySet{HostPolicy: host}, host)
		return
	}
	digest := buildmanifest.Digest(manifest)
	// Read the durable approval record. A missing record means no
	// prior approval — build unavailable until `omac build approve` +
	// restart. A present record whose digest matches the current
	// manifest is the frozen snapshot. A digest mismatch is NOT a
	// frozen snapshot (the manifest changed; the host must re-approve
	// + restart).
	rec, err := buildmanifest.LoadApprovalAt(leaf, loc)
	if err != nil || rec.Digest == "" || rec.Digest != digest {
		return
	}
	store.FreezeFromApproval(canonicalWorktree, digest, rec.Capabilities, host)
}
