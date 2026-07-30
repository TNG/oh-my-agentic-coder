package cli

import (
	"fmt"
	"runtime"

	"github.com/tngtech/oh-my-agentic-coder/internal/buildmanifest"
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
// closed with exit 3 naming the alias, never the credential. The
// credential never enters executor env/args/gradle.properties/logs/audit.
func startCredentialProxy(env *Env, manifestRegistries []buildmanifest.RegistryEntry, approvedAliases []string) (map[string]string, func(), error) {
	if runtime.GOOS != "darwin" {
		// Linux kernel-blocked: the credential proxy (loopback HTTP) is
		// unreachable from the executor. v1 does not start it on Linux.
		return nil, nil, nil
	}
	regs, err := credproxy.LookupRegistries(manifestRegistries, approvedAliases, credentialLookup)
	if err != nil {
		return nil, nil, err
	}
	if len(regs) == 0 {
		// No private registries approved — common case; nothing to start.
		return nil, nil, nil
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(env.Stderr, "omac build: credproxy: "+format+"\n", args...)
	}
	srv, err := credproxy.NewServer(regs, logf)
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
