package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
	"github.com/tngtech/oh-my-agentic-coder/internal/containerproxy"
	"github.com/tngtech/oh-my-agentic-coder/internal/credproxy"
	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
)

// startBuildProxy starts the omac filtered proxy for the build path so
// public dependency resolution works without printing a proxy password,
// AND tightens the filter from allow-all (ticket 04) to an allowlist of
// public Gradle/Maven endpoints, with build-scan upload hosts denied
// (ticket 06).
//
// Returns the proxy URL + port and a stop func. The proxy carries no
// password for public resolution in this ticket (ticket 06 adds private
// registry credentials via the SEPARATE credential-lift proxy, started
// by startCredentialProxy after the manifest gate).
//
// Posture: on macOS (Shape A) the build executor is env-only filtered,
// so the loopback proxy is reachable; the proxy is started. On Linux the
// build executor is kernel-blocked, so the proxy would be unreachable —
// it is not started (returns empty URL/zero port, no-op stop). Proxy
// startup failure is a service failure (the build path depends on it for
// dependency resolution on macOS).
//
// The proxy is injected into the child via GRADLE_OPTS (see grants.go),
// NEVER JAVA_TOOL_OPTIONS — the JVM prints that env var on every launch,
// leaking any token (spec.md:180).
//
// Private-registry UPSTREAM hosts are deliberately NOT on this allowlist:
// the init.d control script rewrites all private-registry requests to the
// loopback credential-lift proxy, so Gradle never needs to reach an
// upstream directly. Allowing the upstream through the filtered proxy
// would be a bypass path — build code that ignored the injected mirror
// and hit the upstream directly would reach a private host without the
// credential, contradicting spec.md:174 ("Direct external networking
// remains denied") and the fail-closed posture. The credential-lift
// proxy (startCredentialProxy) is the ONLY served path for private
// registries.
func startBuildProxy(env *Env) (proxyURL string, proxyPort int, stop func(), err error) {
	if runtime.GOOS != "darwin" {
		// Linux kernel-blocked build path: the proxy would be unreachable.
		return "", 0, nil, nil
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(env.Stderr, "omac build: proxy: "+format+"\n", args...)
	}
	// Tightened filter (ticket 06): allowlist of public Gradle/Maven
	// endpoints ONLY; deny build-scan upload hosts; prompting disabled
	// (the manifest approval IS the prompt replacement — unattended).
	// With a non-empty AllowDomains the default decision is "not in
	// allowlist" → deny, so anything outside the allowlist is blocked
	// fail-closed — including private-registry upstreams, which must go
	// through the credential-lift proxy.
	filter := netproxy.NewFilter(netproxy.FilterConfig{
		AllowDomains: publicGradleMavenAllowlist,
		DenyDomains:  buildScanDenylist,
		Logf:         logf,
	})
	srv, err := netproxy.NewServer(filter, netproxy.NewDirectDialer(), logf)
	if err != nil {
		return "", 0, nil, fmt.Errorf("create proxy: %w", err)
	}
	if err := srv.Start(); err != nil {
		return "", 0, nil, fmt.Errorf("start proxy: %w", err)
	}
	return srv.ProxyURL(), srv.Port(), func() { srv.Close() }, nil
}

// credentialLookup is the host-side keychain read seam used by
// startCredentialProxy. Production wires credproxy.KeychainLookup; tests
// inject a fake to assert the missing-credential denial (criterion 7)
// without touching the real keychain. nil selects credproxy.KeychainLookup.
var credentialLookup = credproxy.KeychainLookup

// startCredentialProxy starts the credential-lift proxy (ticket 06) for
// the approved private Maven registries. The proxy runs host-side,
// unsandboxed, reads each registry's keychain credential once at startup,
// and authenticates upstream on Gradle's behalf — Gradle sees only the
// non-secret local loopback URL per alias (http://127.0.0.1:<port>/<alias>/).
//
// Returns the alias→URL map Gradle is pointed at (via the OMAC-authored
// init.d script) and a stop func. Empty map + nil stop when no private
// registries are approved (the common case) or on Linux (the credential
// proxy, like the filtered proxy, is macOS-only in v1 — the build
// executor is kernel-blocked on Linux).
//
// A missing keychain credential for an approved registry yields a
// *credproxy.RegistryCredentialError (criterion 7) — the build fails
// closed with exit 3 naming the alias, never the credential. The lookup
// runs on EVERY platform (including Linux): an approved private registry
// with no credential is a fail-closed policy denial even where the proxy
// itself cannot serve it — the build could not resolve the registry's
// private dependencies either way. Only the proxy SERVER is macOS-only;
// the credential absence is platform-independent.
//
// The credential never enters executor env/args/gradle.properties/logs/audit.
//
// Stable port: the proxy binds a DETERMINISTIC loopback port derived from
// the canonical worktree path (stableport.For, range [30000,40000))
// instead of a random ephemeral port each run, so the init-script
// repository URL Gradle is pointed at (rendered by PrepareControlState)
// stays valid across runs (the bug being fixed: a new random port each run
// left requests hitting a dead port — it9a's "Read timed out"). The
// assigned port is recorded at
// <controlLeaf>/.omac-control/credproxy-port and preferred on the next
// run. On a rare collision (the whole [30000,40000) window occupied) the
// proxy falls back to a random ephemeral port and logs a warning —
// correctness over determinism (the stale-URL bug may resurface in that
// rare case, but the build still runs).
func startCredentialProxy(env *Env, worktree, controlLeaf string, manifestRegistries []buildmanifest.RegistryEntry, approvedAliases []string) (map[string]string, func(), error) {
	regs, err := credproxy.LookupRegistries(manifestRegistries, approvedAliases, credentialLookup)
	if err != nil {
		return nil, nil, err
	}
	if len(regs) == 0 {
		// No private registries approved — common case; nothing to start.
		return nil, nil, nil
	}
	if runtime.GOOS != "darwin" {
		// Linux kernel-blocked: the credential proxy (loopback HTTP) is
		// unreachable from the executor. v1 does not start it on Linux —
		// but the lookup above ALREADY ran: a missing credential was a
		// fail-closed denial on every platform. Here the credential is
		// present, yet the proxy cannot serve it on Linux, so there is
		// nothing to start.
		return nil, nil, nil
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(env.Stderr, "omac build: credproxy: "+format+"\n", args...)
	}
	srv, err := credproxy.NewServerWithConfig(credproxy.Config{
		Registries:   regs,
		WorktreePath: worktree,
		ControlLeaf:  controlLeaf,
		Logf:         logf,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create credential proxy: %w", err)
	}
	if err := srv.Start(); err != nil {
		return nil, nil, fmt.Errorf("start credential proxy: %w", err)
	}
	urls := map[string]string{}
	for _, r := range regs {
		urls[r.Alias] = srv.URL(r.Alias)
	}
	return urls, func() { srv.Close() }, nil
}

// containerProxyStarter is the seam for starting the mediated Docker
// container proxy (ticket 08). Production wires startContainerProxy; tests
// inject a fake to assert the proxy is started only when images are
// approved (macOS) and to avoid touching a real Docker/Colima daemon. The
// seam signature matches startContainerProxy:
// (env, worktree, controlLeaf, approvedImages, buildReqID, auditor) -> (url, enabled, stop, error).
// buildReqID (ticket 09, spec §254) is threaded into the proxy so
// container-policy denials are correlated with the active build request.
// controlLeaf is the OMAC cache leaf (GRADLE_USER_HOME) where the proxy
// records its assigned port at .omac-control/containerproxy-port so the
// DOCKER_HOST set for a build stays valid across runs.
var containerProxyStarter = startContainerProxy

// startContainerProxy starts the mediated Docker-compatible endpoint
// (ticket 08, ADR 0002) for the approved container images. The proxy
// runs host-side, unsandboxed, forwards only the ticket-02 measured
// allowlist to the existing Docker/Colima daemon, and points the executor
// at it via DOCKER_HOST (NEVER the raw socket). The executor authenticates
// by ownership (omac.executor=<id> label), not token.
//
// Ticket 09: at startup the proxy runs a scavenger removing abandoned
// resources from a PREVIOUS crashed executor with the same id (checkbox 6),
// and threads buildReqID so denials carry the active request id (spec §254).
//
// Stable port: the proxy binds a DETERMINISTIC loopback port derived from
// the canonical worktree path (stableport.For, range [30000,40000)) instead
// of a random ephemeral port each run, so the DOCKER_HOST set for a build
// stays valid across runs (the bug being fixed: a new random port each run
// pointed a later build at a dead port, surfacing as "Connection refused"
// until the stale URL was cleared). The assigned
// port is recorded at <controlLeaf>/.omac-control/containerproxy-port and
// preferred on the next run. On a rare collision (the whole [30000,40000)
// window occupied) the proxy falls back to a random ephemeral port and logs
// a warning — correctness over determinism (the stale-URL issue may
// resurface in that rare case, but the build still runs).
//
// Returns the DOCKER_HOST URL, an enabled flag, the daemon's maximum
// supported Engine API version (empty when the startup /version probe
// failed — the executor then omits api.version and the proxy's
// clampAPIVersion handles version mismatches), a stop func that tears down
// the listener AND runs Cleanup (best-effort removal of executor-owned
// containers + the executor-owned internal network), and an error. Empty
// URL + nil stop when no images are approved (the common case — a standard
// Gradle project needs no Docker mediation) or on Linux (the build
// executor is kernel-blocked, so the loopback proxy is unreachable).
//
// macOS-only in v1 (Shape A, env-only network) — same gate as the filtered
// /credential proxies. The executor ID is a stable per-worktree value
// (derived from the canonical worktree path) so one executor's resources
// are distinct from another's across concurrent worktrees.
func startContainerProxy(env *Env, worktree, controlLeaf string, approvedImages []string, buildReqID string, auditor audit.Auditor) (url string, enabled bool, apiVersion string, stop func(), err error) {
	tw := env.traceWriter()
	ctrace := func(format string, args ...any) {
		if os.Getenv("OMAC_BUILD_TRACE") != "1" {
			return
		}
		fmt.Fprintf(tw, "omac build: containerproxy: "+format+"\n", args...)
	}
	ctrace("startContainerProxy entry: goos=%s worktree=%s controlLeaf=%s approvedImages=%v buildReqID=%s",
		runtime.GOOS, worktree, controlLeaf, approvedImages, buildReqID)
	if runtime.GOOS != "darwin" {
		// Linux kernel-blocked: the loopback proxy is unreachable from
		// the executor. v1 does not start it on Linux.
		ctrace("NOT starting: runtime.GOOS=%q != darwin (Linux kernel-blocked)", runtime.GOOS)
		return "", false, "", nil, nil
	}
	if len(approvedImages) == 0 {
		// No approved images — common case; nothing to mediate.
		ctrace("NOT starting: len(approvedImages)==0 (standard Gradle project, no Docker mediation)")
		return "", false, "", nil, nil
	}
	execID := containerExecutorID(worktree)
	// Resolve the upstream socket the proxy will forward to, so the
	// trace shows what os.UserHomeDir() resolves to in the parent.
	// Mirror containerproxy.New's default: unix://$HOME/.colima/default/docker.sock
	home, _ := os.UserHomeDir()
	const colimaSocketRel = ".colima/default/docker.sock"
	upstream := "unix://" + home + "/" + colimaSocketRel
	ctrace("resolved upstream (os.UserHomeDir=%q): %s", home, upstream)
	// Stat the upstream socket so the trace shows whether it exists +
	// is a socket BEFORE the proxy starts (a missing/staged socket
	// here surfaces as a forward-time failure later).
	if fi, err := os.Stat(home + "/" + colimaSocketRel); err != nil {
		ctrace("upstream socket stat: %v", err)
	} else {
		ctrace("upstream socket stat: mode=%s isSocket=%v", fi.Mode(), fi.Mode()&os.ModeSocket != 0)
	}
	// The proxy's own logf is ALWAYS wired (containerproxy.New/Start
	// and request tracing flow through it); when OMAC_BUILD_TRACE is
	// off, logf is a no-op so unit tests are unaffected.
	var logf func(string, ...any)
	if os.Getenv("OMAC_BUILD_TRACE") == "1" {
		logf = func(format string, args ...any) {
			fmt.Fprintf(tw, "omac build: containerproxy: "+format+"\n", args...)
		}
	}
	p, err := containerproxy.New(containerproxy.Config{
		ApprovedImages: approvedImages,
		ExecutorID:     execID,
		WorktreePath:   worktree,
		ControlLeaf:    controlLeaf,
		Auditor:        auditor,
		Logf:           logf,
	})
	if err != nil {
		ctrace("containerproxy.New FAILED: %v", err)
		return "", false, "", nil, fmt.Errorf("create container proxy: %w", err)
	}
	p.SetBuildRequestID(buildReqID)
	ctrace("containerproxy.New ok (execID=%s), calling Start()...", execID)
	dockerHost, stopFn, err := p.Start()
	if err != nil {
		ctrace("p.Start() FAILED: %v", err)
		return "", false, "", nil, fmt.Errorf("start container proxy: %w", err)
	}
	apiVer := p.APIVersion()
	ctrace("p.Start() ok, DOCKER_HOST=%s enabled=true apiVersion=%s", dockerHost, apiVer)
	return dockerHost, true, apiVer, stopFn, nil
}

// containerExecutorID derives a stable, unforgeable executor ownership
// label value from the canonical worktree path. One executor's resources
// (containers, network) are distinct from another's across concurrent
// worktrees. The value is non-secret (it appears as a Docker label on
// executor-owned containers) and stable for the worktree across builds.
// Uses the base name of the resolved worktree path so linked worktrees
// (which share a repo but have distinct worktree dirs) get distinct IDs.
func containerExecutorID(worktree string) string {
	if worktree == "" {
		return "omac-exec"
	}
	base := filepath.Base(worktree)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "omac-exec"
	}
	return "omac-" + base
}
