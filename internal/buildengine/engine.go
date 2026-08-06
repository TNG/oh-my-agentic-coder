package buildengine

import (
	"errors"
	"fmt"
	"io"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildcontrol"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildrun"
)

// ResultClass is the explicit, transport-independent outcome class the
// engine assigns where the outcome occurs. Callers (the CLI client, the
// broker, tests) never infer it from a numeric exit code: reserved codes
// (3/4/10) are ambiguous with raw wrapper exits, so the class carries the
// disambiguation the wire protocol and the CLI exit-code translator both
// need.
//
// Exit-code mapping (spec §Result class mapping):
//
//	success          -> 0
//	build_failure    -> raw wrapper exit code, including 3, 4, or 10
//	policy_denial    -> 3
//	cancelled        -> 4, with the "omac build: cancelled" marker on stderr
//	service_failure  -> 10
type ResultClass string

const (
	// ClassSuccess is a successful build (wrapper exit 0).
	ClassSuccess ResultClass = "success"
	// ClassBuildFailure is a build-tool failure: the wrapper exited
	// non-zero for a reason that is the build's own (compile error,
	// test failure, wrapper signal death, wrapper exit 3/4/10). The
	// engine assigns this for EVERY non-zero wrapper exit that is not
	// known to be an OMAC outcome (cancellation, service failure,
	// policy denial). Raw wrapper exits 3, 4, and 10 are build
	// failures, NOT OMAC outcomes — the engine distinguishes them via
	// the class, not the numeric code.
	ClassBuildFailure ResultClass = "build_failure"
	// ClassPolicyDenial is an OMAC policy denial: the request was
	// rejected before any build code ran (grammar/adapter error,
	// worktree escape, wrapper validation failure, manifest load /
	// validate / gate failure). The build never started.
	ClassPolicyDenial ResultClass = "policy_denial"
	// ClassCancelled is a caller-cancelled build: the graceful
	// cancellation signal fired (first SIGINT/SIGTERM or
	// --max-duration expiry). The "omac build: cancelled" marker is
	// printed to stderr before the result is returned.
	ClassCancelled ResultClass = "cancelled"
	// ClassServiceFailure is an OMAC infrastructure failure: sandbox
	// unavailable, exec error, queue busy, grants derivation failure,
	// proxy startup failure, mandatory cleanup failure. Distinct from
	// build_failure (Gradle's own rc 1) by the class, not just by exit
	// code 10.
	ClassServiceFailure ResultClass = "service_failure"
)

// ExitCode returns the CLI exit code the engine's result maps to. The
// CLI exit-code translator calls this; it never infers the class from
// the code.
func (r Result) ExitCode() int {
	switch r.Class {
	case ClassSuccess:
		return 0
	case ClassBuildFailure:
		// Raw wrapper exit code, including 3, 4, or 10. The engine
		// recorded the wrapper's exit code in r.Exit; it is the
		// pass-through build-failure code.
		return r.Exit
	case ClassPolicyDenial:
		return 3
	case ClassCancelled:
		return 4
	case ClassServiceFailure:
		return 10
	default:
		// Unknown class: treat as a service failure (defensive — the
		// engine never produces an unknown class; this is the
		// fail-closed fallback for a caller that constructed a Result
		// directly).
		return 10
	}
}

// Result is the engine's transport-independent outcome. The class is
// assigned where the outcome occurs; the exit code is derived from it.
// Stdout/stderr are streamed through the writers the caller supplied —
// they are NOT captured in the Result.
type Result struct {
	// Class is the explicit outcome class (never inferred from Exit).
	Class ResultClass
	// Exit is the raw wrapper exit code for ClassBuildFailure (the
	// pass-through build-failure code, incl. 128+n on signals); 0 for
	// ClassSuccess; the reserved code (3/4/10) for the other classes.
	Exit int
	// Err is the diagnostic error for non-success classes (nil for
	// ClassSuccess and ClassBuildFailure unless the caller wants a
	// structured diagnostic). The CLI prints it omac-prefixed on
	// stderr; the broker frames it as a sanitized service-failure
	// diagnostic. The engine does NOT print Err itself for the
	// policy_denial / service_failure paths it owns — the caller
	// renders it — to keep the engine transport-neutral.
	Err error
}

// PolicySnapshot is the immutable approved-policy snapshot the engine
// consumes for one invocation. It contains the approved manifest digest,
// the frozen effective capability set, and the host ceilings. The engine
// reloads the manifest only to verify its digest still matches Digest;
// it cannot write approvals or replace the snapshot.
//
// A zero Digest (empty string) means "no manifest is present" — the
// normal case for a standard Gradle project. The engine skips the
// digest-verification gate and proceeds with an empty capability set
// (no private registries, no container images, default resources).
type PolicySnapshot struct {
	// Digest is the approved manifest content digest
	// (buildmanifest.Digest). Empty when no manifest is present.
	Digest string
	// Capabilities is the frozen effective capability set
	// (buildmanifest.CapabilitySet). Zero when no manifest is present.
	Capabilities buildmanifest.CapabilitySet
	// HostPolicy is the host authority ceiling in effect for this
	// invocation (buildrun.HostPolicy → buildmanifest.HostPolicy).
	// The engine passes this to buildmanifest.Validate when it
	// reloads the manifest.
	HostPolicy buildmanifest.HostPolicy
}

// SnapshotProvider resolves an invocation-scoped PolicySnapshot for a
// canonical worktree. The engine calls it once per invocation, before
// reloading the manifest.
//
// Two real adapters:
//
//   - Direct host invocation: resolves the snapshot from the durable
//     approval record under the cache leaf by calling the existing
//     buildmanifest.Gate, which (per its current contract) RECORDS
//     approval on first use and returns a *GateError when the manifest
//     changed or there is no prior approval — the engine surfaces that
//     as policy_denial. This preserves the prefactor's
//     behavior-preserving constraint: the direct-host path keeps its
//     current gate semantics.
//   - Brokered (start/serve parent): reads the parent-owned in-memory
//     snapshot frozen at activation. Never writes approvals; a digest
//     mismatch is a policy_denial (the broker routes the human to
//     `omac build approve` + parent restart).
//
// The engine cannot write approvals or replace snapshots: the provider
// is the only seam that touches approval state, and the engine treats
// its result as immutable.
//
// worktree is the canonical worktree (resolved.Worktree). leaf is the
// resolved Gradle cache leaf (buildrun.GradleLeaf(cacheDir)) where the
// durable approval record lives; the direct-host adapter uses it, the
// broker adapter ignores it (the parent snapshot is in memory). req is
// the parsed build request (buildrun.Request) so the provider can
// derive the host ceiling from --max-duration (the direct adapter
// passes req.MaxDuration to buildrun.HostPolicy, matching the original
// cli/build.go behavior; the broker adapter ignores it — the parent
// snapshot already froze the ceiling).
type SnapshotProvider func(worktree, leaf string, req buildrun.Request) (PolicySnapshot, error)

// ErrPolicyDenial is the sentinel the engine uses to mark a snapshot /
// manifest error as a policy denial (the SnapshotProvider may return a
// *buildmanifest.GateError directly; the engine wraps it for the
// result). Callers do NOT need to errors.As against this — the engine
// assigns ClassPolicyDenial at the outcome site.
var ErrPolicyDenial = errors.New("buildengine: policy denial")

// ProxyStarter is the seam for the three host proxies (filtered,
// credential, container) the engine starts for an ordinary build. The
// engine owns the STARTUP ORDERING and the defer chain for cleanup; the
// adapter starts the proxies and returns stop funcs. Production wires the
// existing cli startBuildProxy / startCredentialProxy /
// startContainerProxy orchestration; tests inject a fake to assert
// ordering and avoid touching real network/Docker/keychain.
//
// The seam is one function returning three handles so the engine can
// sequence startup exactly as the current internal/cli/build.go does:
// filtered first, then credential (which needs the manifest's approved
// registries), then container (which needs the approved images + the
// auditor + the build request id). The signature carries everything the
// engine already passes today; nothing new is exposed (no proxy
// constructors, no daemon endpoints, no credential values — those stay
// inside the adapter).
type ProxyStarter func(env *ProxyEnv) (filtered ProxyHandle, credential CredentialProxyHandle, container ContainerProxyHandle, err error)

// ProxyEnv bundles the inputs the ProxyStarter needs. The engine
// constructs it from the resolved build state; the adapter consumes it.
// Nothing in here is a credential value or a raw daemon endpoint — the
// adapter is the only thing that touches those.
type ProxyEnv struct {
	// Workdir is the canonical worktree root.
	Workdir string
	// Worktree is the canonical worktree (resolved.Worktree). Equal to
	// Workdir for the direct-host path; the broker passes the
	// authorized canonical worktree.
	Worktree string
	// CacheDir is the resolved OMAC cache scope dir.
	CacheDir string
	// Leaf is the Gradle cache leaf (buildrun.GradleLeaf(CacheDir)).
	Leaf string
	// ManifestRegistries is the manifest's declared registry entries
	// (upstream identities, non-secret). The credential proxy uses
	// these to know which aliases to lift credentials for.
	ManifestRegistries []buildmanifest.RegistryEntry
	// ApprovedRegistries is the frozen-for-session approved alias list
	// (from the snapshot's capability set).
	ApprovedRegistries []string
	// ApprovedImages is the frozen-for-session approved image list.
	ApprovedImages []string
	// BuildRequestID is the short non-secret request id threaded into
	// the container proxy so denials are correlated with the active
	// request.
	BuildRequestID string
	// Auditor receives container-proxy events.
	Auditor audit.Auditor
	// Stderr receives proxy log lines (omac build: proxy: ...).
	Stderr io.Writer
}

// ProxyHandle is the result of starting the filtered proxy: the URL
// Gradle is pointed at via GRADLE_OPTS (empty when the proxy is not
// started — Linux or no manifest), the port (0 when no proxy), the
// enabled flag (true when the proxy is actually serving), and a stop
// func that tears down the listener. Nil stop means nothing to tear
// down.
type ProxyHandle struct {
	URL     string
	Port    int
	Enabled bool
	Stop    func()
}

// CredentialProxyHandle is the credential-lift proxy result: the
// alias→URL map Gradle is pointed at via the OMAC-authored init.d
// script (empty when no private registries are approved or on Linux),
// and a stop func that tears down the listener. Nil stop means nothing
// to tear down. The map carries NO credential — the URL is
// http://127.0.0.1:<port>/<alias>/.
type CredentialProxyHandle struct {
	URLs map[string]string
	Stop func()
}

// ContainerProxyHandle is the container-proxy result: the DOCKER_HOST
// URL (loopback, no userinfo), the enabled flag, and a stop func that
// tears down the listener AND runs Cleanup (best-effort removal of
// executor-owned containers + the executor-owned internal network). Nil
// stop means nothing to tear down.
type ContainerProxyHandle struct {
	URL     string
	Enabled bool
	Stop    func()
}

// Options bundles the engine inputs for one Run invocation.
type Options struct {
	// Workdir is the canonical worktree root (env.Workdir, already
	// absolutized by the CLI). The engine canonicalizes it again via
	// buildrun.Resolve to defend against a non-canonical caller.
	Workdir string
	// RawArgs are the arguments AFTER `omac build` (the engine does
	// NOT see "build" itself). For `omac build stop` the caller
	// dispatches to Stop instead. The engine reparses RawArgs with
	// buildrun.ParseArgs.
	RawArgs []string
	// Stdout/Stderr receive the build's output incrementally (direct
	// pipe through, never buffered to completion). The engine also
	// writes its own omac-prefixed diagnostics to Stderr.
	Stdout io.Writer
	// Stderr must be non-nil; the engine does not substitute io.Discard
	// (a nil caller is a bug).
	Stderr io.Writer
	// CacheDir is the resolved OMAC cache scope dir (from
	// internal/toolcache via the cli wiring). The engine never invents
	// paths.
	CacheDir string
	// CloseScope releases the cache-scope lock; the engine defers it.
	// nil means the caller owns the scope (e.g. a test reusing a
	// prepareBuildCache scope).
	CloseScope func()
	// Auditor receives the build lifecycle events; the engine opens it
	// BEFORE the build and emits build.request here. nil → audit.Nop().
	Auditor audit.Auditor
	// Snapshot resolves the immutable approved-policy snapshot for the
	// worktree. The engine calls it once per invocation. nil selects
	// DirectSnapshotProvider (the direct-host adapter that calls the
	// existing buildmanifest.Gate).
	Snapshot SnapshotProvider
	// Proxies starts the three host proxies. nil selects a no-op
	// starter (no proxies — used by tests that inject a fake). The
	// engine uses the returned URLs/enabled flags to populate
	// buildrun.BuildConfig exactly as the current cli/build.go does.
	Proxies ProxyStarter
	// Cancel, when closed, cancels the build: SIGTERM to the child's
	// process group, then SIGKILL after KillAfter. nil disables
	// cancellation (the engine builds non-cancellable — used by tests
	// that don't exercise the cancel path).
	Cancel <-chan struct{}
	// ForceCancel, when closed, collapses the graceful KillAfter
	// window to ~0 (a second signal forces immediate SIGKILL and
	// triggers the OnForcedCancel daemon recycle). nil disables
	// forced cancellation.
	ForceCancel <-chan struct{}
	// Launcher, when non-nil, overrides the platform sandbox launch
	// (sandboxrun.BuildChildArgv). Tests inject
	// buildrun.NoSandboxLauncher so the engine executes without
	// applying a Seatbelt/bwrap profile (which nested sandboxes
	// cannot apply). Production leaves it nil so the default
	// kernel-sandboxed launch runs — the engine does NOT expose this
	// as a public capability, only as the existing test seam
	// buildrun.RunOptions already documents.
	Launcher func(g *buildrun.BuildGrants, innerArgv []string) ([]string, error)
	// CacheRoot is the shared cache root (parent of cache-scope dirs,
	// typically ~/.cache/omac) under which the host-only build-control
	// root lives. When non-empty, the engine acquires the leaf-keyed
	// persistent lock at <cacheRoot>/build-control/locks/<sha256(leaf)>.lock
	// BEFORE any mutable control state, generated control-state writes,
	// proxy startup, grants derivation, container scavenging, or
	// execution (spec §Serialization and control state, ticket 06).
	// When empty, the engine falls back to the legacy in-leaf lock at
	// <leaf>/.omac-build.lock (behavior-preserving for tests that
	// don't set CacheRoot and for the no-parent direct-host path that
	// has not yet been migrated).
	CacheRoot string
	// DaemonOwnership wires the pending-to-active daemon ownership
	// handshake (ticket 07, spec.md §237). When
	// DaemonOwnership.Enabled() (CacheRoot + CanonicalLeaf + RequestID
	// set), the engine mints the marker, writes the pending
	// DaemonRecord, starts the DaemonHandshakeChannel, threads the
	// marker + socket path into BuildConfig so GrantsFor →
	// PrepareControlState renders them, runs the handshake concurrently
	// with RunBuild, cancels the wrapper on handshake failure (fail
	// closed), and after RunBuild runs the in-sandbox `gradlew --stop`
	// recycle (RunStopInSandbox) — preserving ADR 0001's
	// cold-start-per-build behavior without an unsandboxed host wrapper
	// invocation (the Phase-3 supervisor requirement). When disabled,
	// the engine runs the legacy Phase-2 path (the unsandboxed
	// daemonRecycle closure) — behavior-preserving for existing tests.
	// Phase 4 wires the brokered `omac build stop`; Phase 5 wires
	// parent-startup reconciliation.
	DaemonOwnership buildrun.DaemonOwnershipConfig
}

// Run executes one complete build invocation behind a
// transport-independent function. It is the prefactor extraction of the
// orchestration currently in internal/cli/build.go's runBuild: manifest
// gating (digest verification against the snapshot), cache-leaf
// preparation, proxy startup, grants derivation, per-leaf locking,
// restricted-executor launch, staged cancellation, post-build daemon
// recycle, and cleanup.
//
// The engine returns a Result with an explicit class assigned at the
// outcome site. The CLI client translates the class to an exit code
// (Result.ExitCode) and renders the diagnostic; the broker frames it as
// a terminal result. The engine does NOT call os.Exit.
//
// Behavior-preserving prefactor (ticket 04): no ordering, exit-code,
// lock-location, or direct-host-semantics change. Every existing
// internal/buildrun and internal/cli/build_integration_test.go test
// stays green.
func Run(opts Options) Result {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	deny := func(err error) Result {
		return Result{Class: ClassPolicyDenial, Exit: 3, Err: err}
	}
	failService := func(format string, args ...any) Result {
		return Result{Class: ClassServiceFailure, Exit: 10, Err: fmt.Errorf(format, args...)}
	}

	// Reparse the raw arguments with the existing command parser. The
	// engine does NOT own the parser; buildrun.ParseArgs is the
	// existing seam and stays the source of truth for the grammar.
	req, err := buildrun.ParseArgs(opts.RawArgs)
	if err != nil {
		var reqErr *buildrun.RequestError
		if errors.As(err, &reqErr) {
			return deny(reqErr)
		}
		return deny(err)
	}
	resolved, err := buildrun.Resolve(opts.Workdir, req)
	if err != nil {
		var reqErr *buildrun.RequestError
		if errors.As(err, &reqErr) {
			return deny(reqErr)
		}
		return failService("resolve: %v", err)
	}

	if opts.CloseScope != nil {
		defer opts.CloseScope()
	}

	// Snapshot: resolve the immutable approved-policy snapshot for this
	// worktree. The engine treats the snapshot as immutable; the
	// provider is the only seam that touches approval state.
	leaf := buildrun.GradleLeaf(opts.CacheDir)
	snapshotProvider := opts.Snapshot
	if snapshotProvider == nil {
		snapshotProvider = DirectSnapshotProvider
	}
	snap, err := snapshotProvider(resolved.Worktree, leaf, req)
	if err != nil {
		// A *buildmanifest.GateError is a policy denial (manifest
		// changed / no prior approval). The provider may also return a
		// *buildmanifest.ManifestError (Load/Validate failure: secret
		// field, absolute root, host-ceiling violation) or a
		// *buildmanifest.HostForbiddenError (forbidden-shape field:
		// bindMounts, privileged, ...) — those are policy denials too
		// (the build never starts). Anything else (a wrapped
		// approval-record I/O error) is a service failure. The
		// direct-host adapter preserves the current behavior: gate
		// errors and manifest errors are policy denials with the
		// structured diagnostic.
		var gateErr *buildmanifest.GateError
		if errors.As(err, &gateErr) {
			fmt.Fprintln(stderr, "omac build: manifest approval required")
			fmt.Fprintln(stderr, gateErr)
			return Result{Class: ClassPolicyDenial, Exit: 3, Err: gateErr}
		}
		var manifestErr *buildmanifest.ManifestError
		if errors.As(err, &manifestErr) {
			return deny(err)
		}
		var forbiddenErr *buildmanifest.HostForbiddenError
		if errors.As(err, &forbiddenErr) {
			return deny(err)
		}
		return failService("snapshot: %v", err)
	}

	// Reload the manifest ONLY to verify its digest still matches the
	// snapshot. The engine does NOT trust the worktree file for
	// capabilities — it uses the snapshot's frozen set. A missing
	// manifest is the normal case (zero snapshot); a present manifest
	// whose digest mismatches the snapshot is a policy denial (the
	// snapshot is stale relative to the worktree — the broker path
	// routes the human to approve + restart; the direct path already
	// caught this in the snapshot provider, but a race between the
	// provider and the reload is still a denial).
	manifest, err := buildmanifest.Load(resolved.Worktree)
	if err != nil {
		return deny(err)
	}
	if manifest.HasManifest() {
		if err := manifest.Validate(snap.HostPolicy); err != nil {
			return deny(err)
		}
		digest := buildmanifest.Digest(manifest)
		if snap.Digest != "" && snap.Digest != digest {
			// The worktree manifest changed after the snapshot was
			// frozen. This is a policy denial: the build must not
			// start with a stale capability set, and the engine
			// cannot advance the snapshot.
			return deny(fmt.Errorf("manifest changed since the policy snapshot was frozen (snapshot digest %s, current %s) — re-approve and restart", shortDigest(snap.Digest), shortDigest(digest)))
		}
	}

	// Per-leaf queue lock (cancellable), acquired BEFORE any mutable
	// control state, generated control-state writes, proxy startup,
	// grants derivation, container scavenging, or execution (spec
	// §Serialization and control state, ticket 06). The lock is keyed
	// by the resolved Gradle cache leaf, NOT the worktree: requests
	// sharing a leaf serialize; requests on distinct leaves may run
	// concurrently. Brokered and direct host invocations derive the
	// same canonical-leaf key so they serialize across processes.
	//
	// When Options.CacheRoot is set (the parent / direct host path
	// with a resolved shared cache root), the lock lives at
	// <cacheRoot>/build-control/locks/<sha256(leaf)>.lock — host-only,
	// persistent, never unlinked, and never included in outer-agent or
	// executor grants. When CacheRoot is empty (tests and the unmigrated
	// no-parent direct path), the engine falls back to the legacy
	// in-leaf lock at <leaf>/.omac-build.lock so existing tests stay
	// behavior-preserving.
	cancel := opts.Cancel
	force := opts.ForceCancel
	lock, err := acquireLeafLock(opts.CacheRoot, leaf, cancel)
	if err != nil {
		if errors.Is(err, buildcontrol.ErrLockCancelled) {
			fmt.Fprintln(stderr, buildrun.CancelledMarker)
			return Result{Class: ClassCancelled, Exit: 4}
		}
		if errors.Is(err, buildrun.ErrLockCancelled) {
			fmt.Fprintln(stderr, buildrun.CancelledMarker)
			return Result{Class: ClassCancelled, Exit: 4}
		}
		return failService("%v", err)
	}
	defer lock.Release()

	// BuildConfig from the frozen snapshot. The engine threads the
	// frozen capability set through exactly as the current cli/build.go
	// does; the manifest's resource request (already validated <=
	// ceiling) narrows the Gradle daemon heap; images/registries are
	// carried for the proxy starter.
	approved := buildrun.BuildConfig{
		MaxHeap:            snap.Capabilities.Resources.MaxHeap,
		ApprovedImages:     snap.Capabilities.Images,
		ApprovedRegistries: snap.Capabilities.Registries,
	}
	approvedRegistries := snap.Capabilities.Registries

	// Proxies: start the three host proxies in the documented order
	// (filtered → credential → container). The engine owns the defer
	// chain for cleanup; the adapter starts them. A nil Proxies
	// starter (tests) skips all proxy startup.
	proxyStarter := opts.Proxies
	if proxyStarter == nil {
		proxyStarter = nopProxyStarter
	}
	penv := ProxyEnv{
		Workdir:            opts.Workdir,
		Worktree:           resolved.Worktree,
		CacheDir:           opts.CacheDir,
		Leaf:               leaf,
		ManifestRegistries: manifest.Registries,
		ApprovedRegistries: approvedRegistries,
		ApprovedImages:     approved.ApprovedImages,
		BuildRequestID:     buildrun.NewBuildRequestID(),
		Auditor:            opts.Auditor,
		Stderr:             stderr,
	}
	filtered, cred, container, perr := proxyStarter(&penv)
	if perr != nil {
		// A credential-lookup denial (missing keychain entry for an
		// approved private registry) is a *credproxy.RegistryCredentialError
		// — the adapter surfaces it as a policy denial (criterion 7,
		// exit 3). The engine maps it via ErrPolicyDenial.
		if filtered.Stop != nil {
			defer filtered.Stop()
		}
		if errors.Is(perr, ErrPolicyDenial) {
			return deny(perr)
		}
		return failService("build proxy: %v", perr)
	}
	if filtered.Stop != nil {
		defer filtered.Stop()
	}
	if cred.Stop != nil {
		defer cred.Stop()
	}
	if container.Stop != nil {
		defer container.Stop()
	}
	approved.ProxyURL = filtered.URL
	approved.ProxyPort = filtered.Port
	approved.RegistryProxyURLs = cred.URLs
	approved.ContainerProxyURL = container.URL
	approved.ContainerProxyEnabled = container.Enabled

	// Ticket 07 Phase 3: daemon ownership handshake. The engine wires
	// the pending-to-active handshake BEFORE GrantsFor so the marker +
	// socket path flow into BuildConfig → GradlePropertiesConfig →
	// PrepareControlState (which GrantsFor calls internally). The
	// handshake channel is started host-side (Option B supervisor:
	// host-side goroutine + in-sandbox `--stop` via a second sandboxed
	// invocation), the verify closure (procidentity + promote) runs
	// concurrently with RunBuild, and on failure the engine cancels
	// the wrapper so the build fails closed without waiting the init
	// script's 30s read timeout. When DaemonOwnership is disabled
	// (CacheRoot/CanonicalLeaf/RequestID zero — the existing tests and
	// the unmigrated no-parent direct path), the engine runs the
	// legacy Phase-2 path (RunBuild unchanged, the unsandboxed
	// daemonRecycle) — behavior-preserving.
	own := opts.DaemonOwnership
	if own.CanonicalLeaf == "" {
		own.CanonicalLeaf = leaf
	}
	if own.RequestID == "" {
		own.RequestID = penv.BuildRequestID
	}
	var (
		ownerMarker buildrun.DaemonOwnerMarker
		ownerCh     *buildrun.DaemonHandshakeChannel
		ownerReady  bool
	)
	if own.Enabled() {
		marker, ch, perr := buildrun.PrepareDaemonOwnership(own)
		if perr != nil {
			// Fail closed: a build that cannot establish ownership
			// must not start (spec.md §237 — the wrapper cannot
			// proceed without the acknowledgement, and the host
			// cannot acknowledge without the channel).
			return failService("prepare daemon ownership: %v", perr)
		}
		ownerMarker = marker
		ownerCh = ch
		ownerReady = true
		// Defer channel close + record retire so every return path
		// after this point cleans up. The retire is best-effort (a
		// failure is logged inside RetireDaemonOwnership).
		defer ownerCh.Close()
		defer buildrun.RetireDaemonOwnership(own, stderr)
		// Thread the marker + socket path into BuildConfig so
		// GrantsFor → PrepareControlState renders them into
		// gradle.properties (-Domac.daemon.owner) + the
		// daemon-handshake-sock control file.
		approved.DaemonOwnerMarker = ownerMarker
		approved.DaemonHandshakeSock = ownerCh.SockPath()
	}

	// Grants: derive the executor grant set (worktree + leaf + temp +
	// JDK + platform baseline). The engine reuses buildrun.GrantsFor —
	// the existing seam. Acquired AFTER the leaf lock per the spec.
	grants, err := buildrun.GrantsFor(resolved.Worktree, opts.CacheDir, approved)
	if err != nil {
		return failService("derive executor grants: %v", err)
	}
	defer grants.CleanupTmp()

	// Audit: emit build.request here (after the lock is acquired, as
	// the current cli/build.go does — the request is now active).
	auditor := opts.Auditor
	if auditor == nil {
		auditor = audit.Nop()
	}
	auditor.Emit(audit.ControlMutation("build.request", resolved.Worktree,
		fmt.Sprintf("request=%s adapter=gradle root=%s args=%d", penv.BuildRequestID, resolved.ProjectDir, len(resolved.Args))))

	// Resolve the JDK executable for the ownership verify closure
	// AFTER GrantsFor (GrantsFor owns JDK resolution). If the ownership
	// path is wired but no JDK could be resolved, the daemon cannot be
	// verified → fail closed as a service failure (spec.md §238 — the
	// executable match is a required identity field; an empty
	// JDKExecutable means procidentity.Verify would never match).
	if ownerReady {
		own.JDKExecutable = grants.JDKExecutable()
		if !own.VerifyReady() {
			return failService("daemon ownership wired but JDK executable unresolved — cannot verify the daemon")
		}
	}

	// Daemon recycle hook. The legacy Phase-2 closure runs the
	// UNSANDBOXED `gradlew --stop` (buildrun.StopGradleDaemon) — used
	// when DaemonOwnership is disabled (existing tests, the unmigrated
	// direct path). When DaemonOwnership is wired, the engine uses the
	// in-sandbox RunStopInSandbox instead (the Phase-3 supervisor
	// recycle: same sandbox grants, same Linux netns, own process
	// group), wired below after RunBuild returns.
	legacyDaemonRecycle := func(rstderr io.Writer) error {
		return buildrun.StopGradleDaemon(buildrun.StopDaemonOptions{
			Wrapper:    resolved.Wrapper,
			ProjectDir: resolved.ProjectDir,
			Leaf:       grants.GradleUserHome(),
			Grants:     grants,
			Stderr:     rstderr,
		})
	}

	// inSandboxRecycle runs the in-sandbox `gradlew --stop` (Phase 3
	// supervisor recycle). Returns the recycle error so the engine can
	// override the primary result with service_failure on a mandatory
	// cleanup failure (spec §Mandatory cleanup failure: a recycle
	// launch failure means the sandbox is unavailable, which is a
	// mandatory cleanup failure). A non-zero `--stop` exit (a wedged
	// daemon) is logged but does NOT override a successful build — the
	// daemon will be reconciled at the next parent startup. A timeout
	// or launch/IO error DOES override (the recycle could not complete
	// inside the sandbox lifecycle).
	inSandboxRecycle := func(rstderr io.Writer) error {
		return buildrun.RunStopInSandbox(buildrun.RunStopInSandboxOptions{
			Resolved: resolved,
			Grants:   grants,
			Stderr:   rstderr,
			Launcher: opts.Launcher,
			Auditor:  auditor,
		})
	}

	// Choose the recycle hook for the forced-cancel path (S3) and the
	// post-build path. When ownership is wired, both use the in-sandbox
	// recycle; when disabled, both use the legacy unsandboxed recycle.
	var recycleHook func(io.Writer) error
	if ownerReady {
		recycleHook = inSandboxRecycle
	} else {
		recycleHook = legacyDaemonRecycle
	}

	// Ticket 07: the ownership handshake runs concurrently with
	// RunBuild. The engine creates an internal cancel channel that
	// closes on EITHER the caller's opts.Cancel OR a handshake failure
	// (so a handshake failure cancels the wrapper without waiting the
	// init script's 30s read timeout — fail closed fast). RunBuild
	// receives the internal cancel; the engine's handshake goroutine
	// closes it on error.
	var internalCancel <-chan struct{}
	var handshakeErr error
	handshakeDone := make(chan struct{})
	if ownerReady {
		ic := make(chan struct{})
		internalCancel = ic
		// Forward the caller's cancel to the internal cancel so
		// RunBuild still honors opts.Cancel.
		if cancel != nil {
			go func() {
				select {
				case <-cancel:
					select {
					case <-ic:
					default:
						close(ic)
					}
				case <-handshakeDone:
					// RunBuild returned; stop forwarding.
				}
			}()
		}
		// Run the handshake in a goroutine. On error, close the
		// internal cancel so RunBuild tears down the wrapper (fail
		// closed). The result is read after RunBuild returns.
		go func() {
			res := buildrun.AwaitDaemonOwnership(own, ownerMarker, ownerCh)
			handshakeErr = res.Err
			if handshakeErr != nil {
				// Fail closed: cancel the wrapper. Non-blocking close
				// (RunBuild may have already returned / already
				// cancelled).
				select {
				case <-ic:
				default:
					close(ic)
				}
			}
			close(handshakeDone)
		}()
	} else {
		// Ownership disabled: RunBuild receives opts.Cancel directly
		// (nil → non-cancellable, matching the legacy contract). No
		// handshake goroutine runs.
		internalCancel = cancel
		close(handshakeDone)
	}

	// cancelled is the authoritative outcome-site flag RunBuild sets
	// when it actually cancelled the build (caller cancel signal OR
	// --max-duration expiry). The engine reads it after RunBuild
	// returns to disambiguate a raw wrapper exit 4 (flag stays false)
	// from an OMAC cancellation (flag set true) — the numeric code 4
	// alone is ambiguous. The flag is the signal the spec calls for
	// ("result class is assigned where the outcome occurs; callers
	// never infer it from a numeric code").
	var cancelled bool
	code, runErr := buildrun.RunBuild(buildrun.RunOptions{
		Resolved:       resolved,
		Grants:         grants,
		Stdout:         opts.Stdout,
		Stderr:         stderr,
		Cancel:         internalCancel,
		ForceCancel:    force,
		MaxDuration:    req.MaxDuration,
		OnForcedCancel: recycleHook,
		Auditor:        auditor,
		Launcher:       opts.Launcher,
		Cancelled:      &cancelled,
	})
	if ownerReady {
		// RunBuild has returned (the wrapper exited). Cancel the
		// handshake channel so a blocked AwaitHandshake does NOT wait
		// the full DefaultHandshakeDeadline (45s) for a daemon that
		// will never dial — the wrapper already exited. Without this,
		// every fast-failing brokered build would pay a 45s penalty.
		// If the handshake already completed (the common case for a
		// daemon build), Cancel is a no-op (the listener is already
		// closed by the handshake returning). AwaitHandshake maps the
		// closed-listener error to ErrHandshakeCancelled, which the
		// engine treats as "wrapper exited, no daemon" — not a
		// handshake failure in its own right (the wrapper's own exit
		// code is the authoritative outcome).
		ownerCh.Cancel()
	}
	if runErr != nil {
		// Service failure (sandbox unavailable, exec error, I/O). The
		// current cli/build.go prints "omac build: <err>" and returns
		// ExitServiceFailure; the engine preserves that but assigns
		// the explicit class.
		<-handshakeDone // let the handshake goroutine exit
		fmt.Fprintf(stderr, "omac build: %v\n", runErr)
		return Result{Class: ClassServiceFailure, Exit: 10, Err: runErr}
	}

	// Wait for the handshake goroutine to finish before deciding the
	// outcome. A handshake failure fails the build closed: the daemon
	// was not verified, so the build cannot be trusted. The wrapper
	// was already cancelled (internalCancel closed by the goroutine),
	// so RunBuild returned ExitCancelled; the engine overrides the
	// class to service_failure (the handshake failure is an OMAC
	// infrastructure failure, not a caller cancellation).
	//
	// ErrHandshakeCancelled is NOT a handshake failure: it means the
	// wrapper exited (and the engine called ownerCh.Cancel) before a
	// daemon dialed. The wrapper's own exit code is the authoritative
	// outcome (a build that finished without a daemon — e.g. a fast
	// wrapper error in init — should not be re-classified as a
	// service failure just because no daemon registered). Only marker
	// mismatch, verify failure, and timeout are handshake failures.
	<-handshakeDone
	if ownerReady && handshakeErr != nil && !errors.Is(handshakeErr, buildrun.ErrHandshakeCancelled) {
		fmt.Fprintf(stderr, "omac build: daemon ownership handshake failed: %v\n", handshakeErr)
		return Result{Class: ClassServiceFailure, Exit: 10, Err: fmt.Errorf("daemon ownership handshake: %w", handshakeErr)}
	}

	// Post-build daemon recycle (cold-start per build, ADR 0001). When
	// ownership is wired, the in-sandbox RunStopInSandbox runs — a
	// launch failure or timeout is a MANDATORY cleanup failure
	// (spec §Mandatory cleanup failure: the sandbox is unavailable)
	// and overrides the primary result with service_failure. A
	// non-zero `--stop` exit (a wedged daemon) is logged but does NOT
	// override a successful build (the daemon will be reconciled at
	// the next parent startup). When ownership is disabled, the legacy
	// unsandboxed recycle runs (best-effort, behavior-preserving).
	if ownerReady {
		if recycleErr := inSandboxRecycle(stderr); recycleErr != nil {
			// Distinguish a non-zero `--stop` exit (a wedged daemon —
			// log, do not override) from a launch/timeout/IO error
			// (mandatory cleanup failure — override to
			// service_failure).
			var ee interface{ ExitCode() int }
			if errors.As(recycleErr, &ee) {
				fmt.Fprintf(stderr, "omac build: warning: in-sandbox daemon recycle exited %d (wedged daemon — will be reconciled at next startup): %v\n", ee.ExitCode(), recycleErr)
			} else {
				fmt.Fprintf(stderr, "omac build: mandatory cleanup failure: in-sandbox daemon recycle failed: %v\n", recycleErr)
				return Result{Class: ClassServiceFailure, Exit: 10, Err: fmt.Errorf("in-sandbox daemon recycle: %w", recycleErr)}
			}
		}
	} else {
		if recycleErr := legacyDaemonRecycle(stderr); recycleErr != nil {
			fmt.Fprintf(stderr, "omac build: warning: post-build daemon recycle failed: %v\n", recycleErr)
		}
	}

	// Classify the wrapper exit. RunBuild returns:
	//   - 0 for success
	//   - the wrapper's exit code for a build failure (incl. 128+n on
	//     signals, AND incl. raw 3/4/10 if the wrapper happened to
	//     exit those — those are build failures, NOT OMAC outcomes)
	//   - ExitCancelled (4) when the build was cancelled (the
	//     CancelledMarker was already printed by RunBuild's
	//     takeResult)
	//
	// The numeric code alone is ambiguous: a raw wrapper exit 4
	// collides with ExitCancelled. The engine disambiguates via the
	// authoritative `cancelled` flag RunBuild sets through the
	// RunOptions.Cancelled out-param — the flag is the outcome-site
	// signal, never the numeric code. A raw wrapper exit 4 leaves the
	// flag false (RunBuild only sets it when it actually cancelled the
	// build), so it classifies as ClassBuildFailure.
	if code == 0 {
		return Result{Class: ClassSuccess, Exit: 0}
	}
	if cancelled {
		// RunBuild already printed the CancelledMarker to stderr.
		return Result{Class: ClassCancelled, Exit: 4}
	}
	// Every other code is a build failure: the wrapper exited
	// non-zero for a build reason (compile/test failure, signal
	// death, or a raw 3/4/10 that happens to collide with omac's
	// reserved codes — those are still build failures, distinguished
	// from OMAC outcomes by the class, not the code).
	return Result{Class: ClassBuildFailure, Exit: code}
}

// StopOptions bundles the engine inputs for one Stop invocation.
// `omac build stop` is a distinct engine operation: it does NOT execute
// the wrapper for an ordinary build, it runs `gradlew --stop` under the
// same isolated env as the build, then force-kills lingering wedged
// daemons, then removes the per-worktree queue lockfile (the prefactor
// preserves the current behavior; ticket 06 removes the lockfile
// deletion).
type StopOptions struct {
	// Workdir is the canonical worktree root.
	Workdir string
	// RawArgs are the arguments AFTER `omac build stop` (typically
	// `--root <rel>` or empty). The engine reparses them with the
	// stop-specific grammar (mirroring buildrun.ParseArgs via the
	// synthesized `--root <rel> -- gradle --stop` form the current
	// cli/build_stop.go uses).
	RawArgs []string
	// Stdout/Stderr receive the wrapper's output.
	Stdout io.Writer
	// Stderr must be non-nil.
	Stderr io.Writer
	// CacheDir is the resolved OMAC cache scope dir.
	CacheDir string
	// CloseScope releases the cache-scope lock; the engine defers it.
	CloseScope func()
	// Auditor receives the build.stop event; nil → audit.Nop().
	Auditor audit.Auditor
}

// Stop executes one complete `omac build stop` invocation. It is the
// prefactor extraction of the orchestration currently in
// internal/cli/build_stop.go's runBuildStop: parse --root, resolve the
// wrapper, run `gradlew --stop` under the same isolated env as the
// build (no host HOME, no host ~/.gradle, no host creds), force-kill
// lingering wedged daemons, and remove the per-worktree queue lockfile.
//
// Behavior-preserving prefactor (ticket 04): the lockfile removal stays
// (ticket 06 removes it); the wrapper-based stop stays (ticket 06
// replaces it with verified trusted daemon control). The engine returns
// a Result with an explicit class assigned at the outcome site.
func Stop(opts StopOptions) Result {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	deny := func(err error) Result {
		return Result{Class: ClassPolicyDenial, Exit: 3, Err: err}
	}
	failService := func(format string, args ...any) Result {
		return Result{Class: ClassServiceFailure, Exit: 10, Err: fmt.Errorf(format, args...)}
	}

	// Parse --root from the user's args (before any `--`), mirroring
	// buildrun.ParseArgs. There is no adapter token here — we
	// synthesize `--root <rel> -- gradle --stop` after extracting the
	// root, exactly as the current cli/build_stop.go does. This
	// preserves the stop grammar: `omac build stop [--root <rel>]`.
	root, perr := parseStopArgs(opts.RawArgs)
	if perr != nil {
		return deny(perr)
	}

	stopArgs := []string{"--root", root, "--", "gradle", "--stop"}
	req, err := buildrun.ParseArgs(stopArgs)
	if err != nil {
		return deny(err)
	}
	resolved, err := buildrun.Resolve(opts.Workdir, req)
	if err != nil {
		return deny(err)
	}

	if opts.CloseScope != nil {
		defer opts.CloseScope()
	}

	leaf := buildrun.GradleLeaf(opts.CacheDir)

	auditor := opts.Auditor
	if auditor == nil {
		auditor = audit.Nop()
	}
	auditor.Emit(audit.ControlMutation("build.stop", resolved.Worktree, "gradle --stop"))

	// Reuse the same isolated env the build executor gets. The engine
	// builds a Grants here so the isolated ChildEnv (JDK-resolved
	// PATH/JAVA_HOME, proxy GRADLE_OPTS if configured) is reused; the
	// kernel sandbox is NOT applied to --stop (it signals a daemon
	// across the process boundary). A Grants derivation failure is
	// non-fatal for stop: fall back to the minimal leaf-only env and
	// run the cooperative stop, as the current cli/build_stop.go does.
	grants, gerr := buildrun.GrantsFor(resolved.Worktree, opts.CacheDir, buildrun.BuildConfig{})
	if gerr != nil {
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
		Stdout:     opts.Stdout,
		Stderr:     stderr,
	}); err != nil {
		// A non-zero `gradlew --stop` exit code passes through as a
		// build failure (the wrapper's own exit code, incl. 128+n on
		// signals). An exec/IO error is a service failure.
		var ee exitError
		if errors.As(err, &ee) {
			return Result{Class: ClassBuildFailure, Exit: ee.ExitCode()}
		}
		return failService("gradle --stop: %v", err)
	}

	// Ticket 06: the lockfile is PERSISTENT and NEVER unlinked. The
	// build-control lock lives under the host-only build-control root
	// (never in executor grants); unlinking a flocked path can let
	// another request create and lock a second inode, defeating
	// serialization. `omac build stop` therefore no longer removes
	// the lockfile — the kernel released the flock on the prior
	// build's exit, and the persistent file is reused by the next
	// Acquire. The legacy in-leaf lockfile is also left in place for
	// the same reason (a future gate moves stop to verified trusted
	// daemon control and acquires the build-control lock itself).
	fmt.Fprintf(opts.Stdout, "omac build stop: stopped Gradle daemons for %s\n", resolved.Worktree)
	return Result{Class: ClassSuccess, Exit: 0}
}

// shortDigest returns the first 8 chars of a digest for diagnostics (the
// full digest is long; the short form is enough to identify a mismatch).
func shortDigest(d string) string {
	if len(d) > 8 {
		return d[:8]
	}
	return d
}
