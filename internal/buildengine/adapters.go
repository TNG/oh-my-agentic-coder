package buildengine

import (
	"fmt"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// DirectSnapshotProvider is the invocation-scoped snapshot adapter for
// direct host-terminal invocation. It resolves the snapshot from the
// durable approval record by calling the existing buildmanifest.Gate —
// the same path the current internal/cli/build.go uses. This preserves
// the prefactor's behavior-preserving constraint: the direct-host path
// keeps its current gate semantics (the gate RECORDS approval on first
// use and returns a *GateError when the manifest changed or there is
// no prior approval — the engine surfaces that as policy_denial).
//
// The host ceiling is derived from the parsed --max-duration (req.MaxDuration),
// matching the original cli/build.go's `buildrun.HostPolicy(req.MaxDuration)`
// call. A zero req.MaxDuration means no per-invocation duration ceiling.
//
// This provider is the default when Options.Snapshot is nil. The broker
// path wires a different adapter (parent-owned snapshot, never writes).
//
// The provider is a function, not a struct, so the engine calls it as
// Snapshot(worktree, leaf, req) — the broker adapter has the same
// signature and replaces it without a wrapper type.
//
// Ticket 06: the durable approval record is read via the legacy
// OnLeaf location (buildmanifest.NewOnLeafLocation) so the direct-host
// path stays behavior-preserving with the existing on-leaf gate. The
// parent-owned build-control approval layout is used by the broker
// path (via the parent's snapshot store). A future gate may migrate
// the direct-host path to the build-control layout too.
func DirectSnapshotProvider(worktree, leaf string, req buildrun.Request) (PolicySnapshot, error) {
	// Replicate the exact sequence the current cli/build.go uses:
	//   hostPolicy := buildrun.HostPolicy(req.MaxDuration)
	//   manifest := Load(worktree); Validate(hostPolicy)
	//   caps := CapabilitySet(hostPolicy); digest := Digest(manifest)
	//   Gate(leaf, digest, caps)
	//
	// The engine reloads the manifest AFTER the snapshot for digest
	// verification; the direct provider ALSO loads it here to compute
	// the digest + capability set the gate needs. This is the same
	// double-load the current code does (Load → Validate → CapabilitySet
	// → Digest → Gate, all in runBuild); the prefactor preserves it.
	hostPolicy := buildrun.HostPolicy(req.MaxDuration)
	manifest, err := buildmanifest.Load(worktree)
	if err != nil {
		return PolicySnapshot{}, err
	}
	if err := manifest.Validate(hostPolicy); err != nil {
		return PolicySnapshot{}, err
	}
	if !manifest.HasManifest() {
		// No manifest: zero snapshot, the engine skips the gate.
		return PolicySnapshot{HostPolicy: hostPolicy}, nil
	}
	caps := manifest.CapabilitySet(hostPolicy)
	digest := buildmanifest.Digest(manifest)
	gateRes, gerr := buildmanifest.Gate(leaf, digest, caps)
	if gerr != nil {
		return PolicySnapshot{}, gerr
	}
	return PolicySnapshot{
		Digest:       gateRes.Digest,
		Capabilities: gateRes.Capabilities,
		HostPolicy:   hostPolicy,
	}, nil
}

// nopProxyStarter is the default ProxyStarter when Options.Proxies is
// nil (tests that don't exercise the proxy path). It returns three
// disabled handles — no proxies started, nothing to stop. The engine
// proceeds with an empty proxy posture (kernel-blocked / no approved
// images / no private registries), matching the Linux v1 build path and
// the no-manifest / no-approved-images common case.
func nopProxyStarter(env *ProxyEnv) (filtered ProxyHandle, credential CredentialProxyHandle, container ContainerProxyHandle, err error) {
	return ProxyHandle{}, CredentialProxyHandle{}, ContainerProxyHandle{}, nil
}

// parseStopArgs parses the args for `omac build stop [--root <rel>]`
// (and `--root=<rel>`), mirroring the current cli/build_stop.go's
// inline parser. Any other flag is a policy denial (same as `omac
// build`). There is no adapter token here — the engine synthesizes
// `--root <rel> -- gradle --stop` after extracting the root, exactly
// as the current cli/build_stop.go does.
//
// Returns the resolved root ("." when no --root is supplied) or an
// error describing the rejection. The engine maps the error to a
// policy_denial result.
func parseStopArgs(args []string) (string, error) {
	root := "."
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--root":
			if i+1 >= len(args) {
				return "", fmt.Errorf("--root requires a value")
			}
			root = args[i+1]
			i++
		case strings.HasPrefix(a, "--root="):
			root = strings.TrimPrefix(a, "--root=")
		case a == "--":
			// Anything after `--` is the adapter token + pass-through;
			// stop owns those, so ignore further flags here.
			i = len(args)
		default:
			return "", fmt.Errorf("unknown flag %q (usage: omac build stop [--root <rel>])", a)
		}
	}
	if root == "" {
		return "", fmt.Errorf("--root must not be empty")
	}
	return root, nil
}

// exitError is the engine's view of a *exec.ExitError from
// buildrun.StopGradleDaemon. The engine pattern-matches via errors.As
// against this interface, which *exec.ExitError satisfies (ExitCode()
// is the method *exec.ExitError exposes). This keeps the engine free
// of an os/exec dependency while still passing the wrapper's exit code
// through as a build_failure.
type exitError interface {
	ExitCode() int
}

// leafLock is the unified lock handle the engine uses regardless of
// whether the lock lives under the host-only build-control root (ticket
// 06) or the legacy in-leaf location. Both underlying types expose a
// Release method; this wrapper dispatches.
type leafLock struct {
	bc *buildcontrol.Lock
	br *buildrun.BuildLock
}

func (l *leafLock) Release() {
	if l == nil {
		return
	}
	if l.bc != nil {
		l.bc.Release()
		return
	}
	if l.br != nil {
		l.br.Release()
	}
}

// acquireLeafLock acquires the leaf-keyed queue lock. When cacheRoot is
// non-empty, the lock lives at <cacheRoot>/build-control/locks/<sha256(leaf)>.lock
// (host-only, persistent, never unlinked, never in executor grants).
// When cacheRoot is empty, the engine falls back to the legacy in-leaf
// lock at <leaf>/.omac-build.lock (behavior-preserving for tests and
// the unmigrated no-parent direct path).
//
// The lock is acquired BEFORE any mutable control state, generated
// control-state writes, proxy startup, grants derivation, container
// scavenging, or execution (spec §Serialization and control state,
// ticket 06). A cancelled-while-waiting returns buildcontrol.ErrLockCancelled
// (build-control path) or buildrun.ErrLockCancelled (legacy path); the
// engine maps both to ClassCancelled + the marker. A busy-denial
// returns a service failure.
func acquireLeafLock(cacheRoot, leaf string, cancel <-chan struct{}) (*leafLock, error) {
	if cacheRoot == "" {
		l, err := buildrun.AcquireCtx(leaf, buildrun.DefaultQueueTimeout, cancel)
		if err != nil {
			return nil, err
		}
		return &leafLock{br: l}, nil
	}
	l, err := buildcontrol.Acquire(cacheRoot, leaf, buildcontrol.DefaultQueueTimeout, cancel)
	if err != nil {
		return nil, err
	}
	return &leafLock{bc: l}, nil
}
