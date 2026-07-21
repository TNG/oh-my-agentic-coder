// Package origin attributes a proxied connection to the process that opened
// it, for display in the network prompt. Given the child's source address (the
// proxy's accepted conn.RemoteAddr) and the proxy's own address, it resolves
// the owning PID and process name.
//
// It is best-effort and display-only: any failure yields ok=false and the
// prompt simply omits the Origin line. It never influences the access verdict.
// Resolution is Linux-only (via /proc); other platforms return a noop resolver.
package origin

import "net/netip"

// Origin is the process that opened a proxied connection. Name is the basename
// (comm) only — never the full command line, which can carry secrets in argv.
// PID is the host PID (the supervisor's namespace), which is what `ps` on the
// host shows; it may differ from the number the child sees in its own PID
// namespace.
type Origin struct {
	PID  int
	Name string
}

// Resolver maps a proxied connection to its originating process.
type Resolver interface {
	// Resolve returns the process that owns the connection whose local
	// endpoint is src and whose remote endpoint is proxy (i.e. the child's
	// socket to the omac proxy). ok=false when it cannot be determined
	// (unsupported OS, closed socket, permission, ambiguity).
	Resolve(src, proxy netip.AddrPort) (Origin, bool)
}

// NewResolver returns the platform resolver: a /proc-backed resolver on Linux,
// a noop everywhere else.
func NewResolver() Resolver { return platformResolver() }
