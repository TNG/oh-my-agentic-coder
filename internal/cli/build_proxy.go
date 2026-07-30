package cli

import (
	"fmt"
	"runtime"

	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
)

// startBuildProxy starts the omac filtered proxy for the build path so
// public dependency resolution works without printing a proxy password.
// Returns the proxy URL + port and a stop func. The proxy carries no
// password for public resolution in this ticket (ticket 06 adds private
// registry credentials).
//
// Posture: on macOS (Shape A) the build executor is env-only filtered, so
// the loopback proxy is reachable; the proxy is started. On Linux the
// build executor is kernel-blocked, so the proxy would be unreachable —
// it is not started (returns empty URL/zero port, no-op stop). Proxy
// startup failure is a service failure (the build path depends on it for
// dependency resolution on macOS).
//
// The proxy is injected into the child via GRADLE_OPTS (see grants.go),
// NEVER JAVA_TOOL_OPTIONS — the JVM prints that env var on every launch,
// leaking any token (spec.md:180).
func startBuildProxy(env *Env) (proxyURL string, proxyPort int, stop func(), err error) {
	if runtime.GOOS != "darwin" {
		// Linux kernel-blocked build path: the proxy would be unreachable.
		return "", 0, nil, nil
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(env.Stderr, "omac build: proxy: "+format+"\n", args...)
	}
	// Public-resolution filter: allow all egress (the omac proxy's value
	// here is audit/observability + a single egress chokepoint, not
	// per-host prompting). A deny-all filter with no prompter would block
	// all dependency downloads; ticket 06 tightens this with the mediated
	// registry. For now, public Maven repos resolve straight through.
	filter := netproxy.NewFilter(netproxy.FilterConfig{Logf: logf})
	srv, err := netproxy.NewServer(filter, netproxy.NewDirectDialer(), logf)
	if err != nil {
		return "", 0, nil, fmt.Errorf("create proxy: %w", err)
	}
	if err := srv.Start(); err != nil {
		return "", 0, nil, fmt.Errorf("start proxy: %w", err)
	}
	return srv.ProxyURL(), srv.Port(), func() { srv.Close() }, nil
}
