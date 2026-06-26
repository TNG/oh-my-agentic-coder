package sandboxrun

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// GenerateSBPL renders the Seatbelt profile for the grant set. Pure
// string generation — unit-testable on every platform; only the exec
// path is darwin-specific.
//
// Rule order matters and mirrors nono (crates/nono/src/sandbox/macos.rs):
// read allows -> protected-path denies -> write allows, so a granted
// write path wins over a global deny while protected reads stay denied.
func GenerateSBPL(g *Grants) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n\n")

	// Process lifecycle. Exec/fork are required for any shell-out;
	// process-info and signals are scoped to the sandbox itself.
	b.WriteString("(allow process-exec*)\n")
	b.WriteString("(allow process-fork)\n")
	b.WriteString("(allow process-info* (target self))\n")
	b.WriteString("(allow process-info* (target same-sandbox))\n")
	b.WriteString("(allow signal (target self))\n")
	b.WriteString("(allow signal (target same-sandbox))\n\n")

	// Mach: allow lookups generally but wall off the Keychain daemons
	// (nono parity; `security` and friends fail cleanly).
	b.WriteString("(allow mach-lookup)\n")
	for _, svc := range []string{
		"com.apple.SecurityServer",
		"com.apple.securityd",
		"com.apple.security.keychaind",
		"com.apple.secd",
		"com.apple.security.agent",
	} {
		fmt.Fprintf(&b, "(deny mach-lookup (global-name %s))\n", sbplQuote(svc))
	}
	b.WriteString("\n")

	// Misc system facilities required by real-world toolchains.
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow ipc-posix-shm)\n")
	b.WriteString("(allow system-socket)\n\n")

	// The root directory inode itself must be readable (dyld stats and
	// reads "/" during startup; literal only — not subpath).
	b.WriteString("(allow file-read* (literal \"/\"))\n")

	// --- Read allows ---
	readable := append(append([]string{}, g.ReadPaths...), g.AllowPaths...)
	for _, p := range readable {
		for _, fp := range pathForms(p) {
			fmt.Fprintf(&b, "(allow file-read* (subpath %s))\n", sbplQuote(fp))
		}
	}
	// Metadata on ancestors so path resolution works.
	for _, p := range ancestorDirs(append(append(append([]string{}, g.ReadPaths...), g.WritePaths...), g.AllowPaths...)) {
		fmt.Fprintf(&b, "(allow file-read-metadata (literal %s))\n", sbplQuote(p))
	}
	// DYLD-injection defense: only readable paths may be mapped
	// executable. (allow file-read* implies open; mapping is separate.)
	for _, p := range readable {
		for _, fp := range pathForms(p) {
			fmt.Fprintf(&b, "(allow file-map-executable (subpath %s))\n", sbplQuote(fp))
		}
	}
	b.WriteString("\n")

	// --- Protected-path denies (between read and write allows) ---
	for _, p := range g.ProtectedPaths {
		for _, fp := range pathForms(p) {
			fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", sbplQuote(fp))
			fmt.Fprintf(&b, "(deny file-write* (subpath %s))\n", sbplQuote(fp))
		}
	}
	b.WriteString("\n")

	// --- Write allows ---
	writable := append(append([]string{}, g.WritePaths...), g.AllowPaths...)
	for _, p := range writable {
		for _, fp := range pathForms(p) {
			fmt.Fprintf(&b, "(allow file-write* (subpath %s))\n", sbplQuote(fp))
		}
	}
	b.WriteString("\n")

	// --- Devices every process needs ---
	for _, dev := range []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/dtracehelper"} {
		fmt.Fprintf(&b, "(allow file-read* file-write-data (literal %s))\n", sbplQuote(dev))
	}
	b.WriteString("(allow file-read* file-write-data (regex #\"^/dev/tty\"))\n")
	b.WriteString("(allow file-ioctl (regex #\"^/dev/\"))\n")
	b.WriteString("(allow pseudo-tty)\n\n")

	// --- Network ---
	b.WriteString(generateSBPLNetwork(g))
	return b.String()
}

// generateSBPLNetwork emits the network section per NetworkMode.
func generateSBPLNetwork(g *Grants) string {
	var b strings.Builder
	switch g.NetworkMode {
	case sandboxprofile.ModeOpen:
		b.WriteString("(allow network*)\n")
		return b.String()
	case sandboxprofile.ModeBlocked:
		b.WriteString("(deny network*)\n")
	default: // filtered
		b.WriteString("(deny network*)\n")
		if g.ProxyPort > 0 {
			fmt.Fprintf(&b, "(allow network-outbound (remote tcp \"localhost:%d\"))\n", g.ProxyPort)
		}
		for _, p := range g.AllowTCPConnect {
			fmt.Fprintf(&b, "(allow network-outbound (remote tcp \"*:%d\"))\n", p)
		}
		// 0 is the "any loopback port" sentinel: for tools that connect
		// back over a random ephemeral port they can't predeclare.
		wildcardLoopback := false
		for _, p := range g.OpenPorts {
			if p == 0 {
				wildcardLoopback = true
				continue
			}
			fmt.Fprintf(&b, "(allow network-outbound (remote tcp \"localhost:%d\"))\n", p)
		}
		if wildcardLoopback {
			b.WriteString("(allow network-outbound (remote tcp \"localhost:*\"))\n")
		}
		// Seatbelt cannot filter bind by port: any listen/open port
		// grants bind+inbound generally (documented platform limit).
		if len(g.ListenPorts) > 0 || len(g.OpenPorts) > 0 {
			b.WriteString("(allow network-bind)\n")
			b.WriteString("(allow network-inbound)\n")
		}
	}
	// AF_UNIX: Seatbelt classifies unix-socket connect as network-outbound.
	// Grant the bridge socket explicitly (fixes nono's blanket-deny issue).
	for _, sock := range g.UnixSockets {
		for _, fp := range pathForms(sock) {
			fmt.Fprintf(&b, "(allow network-outbound (literal %s))\n", sbplQuote(fp))
		}
	}
	// AF_UNIX dirs: connect to any socket under the dir (subpath), for
	// daemons with dynamic socket names. Path-scoped, so it can't reach
	// other host sockets; a path filter matches only AF_UNIX, no TCP egress.
	for _, dir := range g.UnixSocketDirs {
		for _, fp := range pathForms(dir) {
			fmt.Fprintf(&b, "(allow network-outbound (subpath %s))\n", sbplQuote(fp))
		}
	}
	// mDNSResponder carve-out so DNS keeps working under deny network*.
	fmt.Fprintf(&b, "(allow network-outbound (literal %s))\n", sbplQuote("/private/var/run/mDNSResponder"))
	return b.String()
}

// pathForms returns the literal path plus its canonicalized form when
// they differ (/tmp vs /private/tmp). When the path doesn't exist yet
// (e.g. an --allow-unix-dir whose daemon mints it after launch),
// EvalSymlinks can't resolve it, so we canonicalize the deepest existing
// ancestor and re-graft the tail — otherwise a /tmp rule never matches
// the kernel's /private/tmp path.
func pathForms(p string) []string {
	out := []string{p}
	if resolved := canonicalPath(p); resolved != "" && resolved != p {
		out = append(out, resolved)
	}
	return out
}

func canonicalPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	dir, tail := filepath.Dir(p), filepath.Base(p)
	for dir != "/" && dir != "." {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, tail)
		}
		dir, tail = filepath.Dir(dir), filepath.Join(filepath.Base(dir), tail)
	}
	return ""
}

// ancestorDirs returns every distinct ancestor directory of the given
// paths (excluding "/" itself is fine to include; Seatbelt tolerates it).
func ancestorDirs(paths []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range paths {
		for _, form := range pathForms(p) {
			dir := filepath.Dir(form)
			for dir != "/" && dir != "." {
				if seen[dir] {
					break
				}
				seen[dir] = true
				out = append(out, dir)
				dir = filepath.Dir(dir)
			}
		}
	}
	if !seen["/"] {
		out = append(out, "/")
	}
	return out
}

// sbplQuote renders a Scheme string literal.
func sbplQuote(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}
