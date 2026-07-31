package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
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

	// Build manifest (ticket 05): Load `.omac/build.yaml` from the worktree,
	// validate against the host policy ceiling, run the frozen-for-session
	// approval gate, and thread the approved capability set into BuildConfig.
	// A missing manifest is the normal case (standard Gradle project) —
	// Load returns a zero manifest and the gate is skipped (no capabilities
	// to freeze). A present manifest that changes since last approval FAILS
	// here with ExitPolicyDenied + the consolidated diff + restart
	// instruction; the build never starts (the human reviews first).
	// The approval + active records live under the cache leaf's
	// `.omac-control/` (per-developer), NOT in the worktree.
	hostPolicy := buildrun.HostPolicy(req.MaxDuration)
	manifest, err := buildmanifest.Load(resolved.Worktree)
	if err != nil {
		// Parse / structural validation error (secret, forbidden field,
		// absolute root, bad version). All map to ExitPolicyDenied.
		return deny(err)
	}
	if err := manifest.Validate(hostPolicy); err != nil {
		// Host-ceiling violation (or a structural error re-surfaced for an
		// in-code manifest). ExitPolicyDenied before executor startup.
		return deny(err)
	}
	approved := buildrun.BuildConfig{}
	var approvedRegistries []string
	if manifest.HasManifest() {
		caps := manifest.CapabilitySet(hostPolicy)
		digest := buildmanifest.Digest(manifest)
		// The gate checks the active (frozen-for-session) record under the
		// cache leaf. GradleLeaf resolves <cacheDir>/gradle (the same leaf
		// GrantsFor uses), so the gate, the grants, and the control-state
		// protection all share one path source.
		leaf := buildrun.GradleLeaf(cacheDir)
		gateRes, gerr := buildmanifest.Gate(leaf, digest, caps)
		if gerr != nil {
			// Changed manifest (or first-ever): print the consolidated diff
			// + restart instruction and deny. The build does not start.
			fmt.Fprintln(env.Stderr, "omac build: manifest approval required")
			fmt.Fprintln(env.Stderr, gerr)
			return ExitBuildPolicyDenied
		}
		// Unattended: thread the frozen capability set into BuildConfig.
		// The manifest's resource request (already validated <= ceiling)
		// narrows the Gradle daemon heap; images/registries are carried for
		// tickets 06/08/09.
		approved.MaxHeap = gateRes.Capabilities.Resources.MaxHeap
		approved.ApprovedImages = gateRes.Capabilities.Images
		approved.ApprovedRegistries = gateRes.Capabilities.Registries
		approvedRegistries = gateRes.Capabilities.Registries
	}

	// Proxy: start the omac filtered proxy so public dependency resolution
	// works without printing a proxy password (GRADLE_OPTS, NEVER
	// JAVA_TOOL_OPTIONS). Best-effort configurable but ON by default for
	// the build path on macOS (Shape A). On Linux the kernel-blocked
	// posture makes the proxy unreachable, so it is not started.
	//
	// Ticket 06 tightens the filter from allow-all to an allowlist of
	// public Gradle/Maven endpoints ONLY, with build-scan upload hosts
	// denied. Private-registry upstreams are deliberately NOT allowed
	// here (they go through the credential-lift proxy below); allowing
	// them would be a bypass path (spec.md:174).
	proxyURL, proxyPort, stopProxy, proxyErr := startBuildProxy(env)
	if proxyErr != nil {
		return failService("build proxy: %v", proxyErr)
	}
	if stopProxy != nil {
		defer stopProxy()
	}
	approved.ProxyURL = proxyURL
	approved.ProxyPort = proxyPort

	// Credential-lift proxy (ticket 06): for the approved private Maven
	// registries, start a host-side loopback HTTP proxy that injects the
	// developer's keychain credential upstream while Gradle sees only a
	// non-secret local URL per alias. The credential NEVER enters the
	// executor (env/args/gradle.properties/logs/audit). A missing keychain
	// credential for an approved registry is a structured denial naming the
	// alias (criterion 7) — exit 3, never a crash, never the credential.
	credProxyURLs, stopCredProxy, credErr := startCredentialProxy(env, manifest.Registries, approvedRegistries)
	if credErr != nil {
		return deny(credErr)
	}
	if stopCredProxy != nil {
		defer stopCredProxy()
	}
	approved.RegistryProxyURLs = credProxyURLs

	// Container proxy (ticket 08, ADR 0002): start the mediated Docker
	// endpoint ONLY when the approved manifest declares container images
	// (macOS-only in v1; Linux kernel-blocked → not started). The executor
	// receives DOCKER_HOST=<loopback proxy URL>, NEVER the raw daemon
	// socket. The proxy authenticates by ownership (omac.executor=<id>
	// label); the URL carries no userinfo. The stop func tears down the
	// listener AND runs Cleanup (removes executor-owned containers + the
	// executor-owned internal network). Cleanup runs via the defer chain
	// below, which fires on BOTH normal completion and forced cancel (a
	// forced cancel returns through RunBuild's normal path after the
	// OnForcedCancel daemon-recycle hook, so deferred funcs still run).
	// It is NOT wired into OnForcedCancel itself (that hook recycles the
	// Gradle daemon); container cleanup relies on the defer, not the hook.
	auditor := buildAuditor(env)
	defer auditor.Close()
	// Build request id (ticket 09, spec §254): a short stable id
	// correlating this build's container-policy denials with the active
	// request. Generated once here, threaded into the container proxy
	// (so denials name the request) and emitted with build.request (so
	// the audit trail ties the id to the request metadata). Non-secret
	// (it appears in denial messages the agent reads).
	buildReqID := newBuildRequestID()
	containerProxyURL, containerProxyEnabled, stopContainerProxy, cpErr := containerProxyStarter(env, resolved.Worktree, buildrun.GradleLeaf(cacheDir), approved.ApprovedImages, buildReqID, auditor)
	if cpErr != nil {
		return failService("container proxy: %v", cpErr)
	}
	if stopContainerProxy != nil {
		defer stopContainerProxy()
	}
	approved.ContainerProxyURL = containerProxyURL
	approved.ContainerProxyEnabled = containerProxyEnabled

	grants, err := buildrun.GrantsFor(resolved.Worktree, cacheDir, approved)
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
	// start/serve path's concern). The auditor was opened earlier (before
	// the container proxy, which needs it for container create/denial/
	// cleanup events); emit the build.request event here.
	auditor.Emit(audit.ControlMutation("build.request", resolved.Worktree,
		fmt.Sprintf("request=%s adapter=gradle root=%s args=%d", buildReqID, resolved.ProjectDir, len(resolved.Args))))

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
              works — macOS is filesystem-confinement only, NO kernel
              network mediation (Shape A; raw-socket-capable build code can
              reach host loopback and external egress — no host-listener
              monitoring/guarding is claimed, ADR 0003 Revision). Linux —
              kernel-blocked (private sandbox loopback; warm-daemon
              cohabitation is a later Linux-validation item).
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

// newBuildRequestID generates a short, non-secret, time-ordered id for one
// `omac build` invocation (ticket 09, spec §254). It correlates the
// build.request audit event with container-policy denials emitted by the
// container proxy so the agent receives an actionable OMAC explanation
// naming the active request rather than only a wrapped Testcontainers
// failure. Format: b<unix-seconds-hex>-<4 random hex bytes>. Non-secret
// (it appears in denial messages the agent reads); collisions are
// negligible (4 random bytes + per-second ordering).
//
// A failing crypto/rand.Read means the host entropy source is broken — a
// host-fatal condition, not a recoverable build error. We panic (the build
// command cannot proceed without a request id to correlate denials against);
// this never happens on a healthy Linux/macOS host.
func newBuildRequestID() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		panic(fmt.Sprintf("omac build: generate build request id: crypto/rand.Read failed: %v (host entropy source broken)", err))
	}
	return fmt.Sprintf("b%s-%s", strconv.FormatInt(time.Now().Unix(), 16), hex.EncodeToString(buf[:]))
}
