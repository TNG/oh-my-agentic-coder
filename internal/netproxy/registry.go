package netproxy

import (
	"fmt"
	"strings"
)

// knownRegistrySuffixes are well-known public package-registry hosts. They are
// matched as exact hosts or dot-suffixes (so "foo.pypi.org" also matches).
var knownRegistrySuffixes = []string{
	"registry.npmjs.org",
	"registry.yarnpkg.com",
	"npm.pkg.github.com",
	"pypi.org",
	"files.pythonhosted.org",
	"crates.io",
	"static.crates.io",
	"rubygems.org",
	"proxy.golang.org",
	"sum.golang.org",
}

// isPackageRegistry reports whether host looks like a package registry. It is a
// heuristic used only to enrich an already-emitted denial/error message, so a
// false positive is harmless (it appends a "looks like a registry" note to a
// request that was already going to fail). Beyond the curated public hosts it
// also recognizes common private-registry shapes (a "registry"/"npm" label, or
// an ecosystem name in the host), so a VPN-scoped `registry.<corp>` still hits.
func isPackageRegistry(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, s := range knownRegistrySuffixes {
		if host == s || strings.HasSuffix(host, "."+s) {
			return true
		}
	}
	labels := strings.Split(host, ".")
	for _, l := range labels {
		switch l {
		case "registry", "npm":
			return true
		}
	}
	for _, frag := range []string{"pypi", "pythonhosted", "crates", "rubygems", "goproxy"} {
		if strings.Contains(host, frag) {
			return true
		}
	}
	return false
}

// registryDenyHint returns an actionable note appended to a policy-denial body
// when the denied host looks like a package registry — the common shape of a
// harness fetching a config-declared plugin/provider into a cold cache. The
// guidance is tailored to why the request was denied (reason), because the
// remedy differs: no interactive prompt was available vs. the prompt is
// disabled vs. the user declined vs. an explicit deny rule. Returns "" for
// non-registry hosts so callers can append unconditionally.
func registryDenyHint(host, reason string) string {
	if !isPackageRegistry(host) {
		return ""
	}
	var next string
	switch {
	case strings.Contains(reason, "prompt unavailable"):
		next = "This launch has no interactive network prompt (e.g. `omac serve` or a non-TTY\n" +
			"session), so the request was auto-denied (on_unavailable=deny). Pre-allow it with\n" +
			"network.allow_domain, or pre-warm the cache once via an interactive `omac start`."
	case strings.Contains(reason, "not in allowlist"):
		next = "The network prompt is disabled for this profile, so unlisted hosts are denied.\n" +
			"Add it to network.allow_domain."
	case strings.Contains(reason, "prompt:deny"):
		next = "It was denied at the network prompt. Re-run and choose Allow (optionally persist)\n" +
			"to let the install proceed."
	case strings.Contains(reason, "dns resolution failed"):
		next = "Its DNS lookup failed, so nothing was denied by policy. If it is a private/VPN-scoped\n" +
			"registry, check that your VPN is connected and the host is reachable."
	case strings.HasPrefix(reason, "hard-deny"):
		next = "It resolved to a blocked link-local/internal address (SSRF guard), which is unusual\n" +
			"for a real registry — verify the host and your DNS/hosts configuration."
	default:
		next = "It matches a deny rule; remove it from network.deny_domain or the learned\n" +
			"<profile>.pages.json policy file."
	}
	return fmt.Sprintf(`
Note: %q looks like a package registry. A tool or harness plugin/provider
install likely tried to fetch from it into a cold cache. %s
Once the fetch succeeds, the plugin is cached and reused on later launches.
`, host, next)
}

// registryUpstreamHint returns a note for a *connection* failure (not a policy
// denial) to a registry host — most often a private/VPN-scoped registry that is
// unreachable. Returns "" for non-registry hosts.
func registryUpstreamHint(host string) string {
	if !isPackageRegistry(host) {
		return ""
	}
	return fmt.Sprintf("omac sandbox: %q looks like a package registry and was allowed by policy but "+
		"the connection failed — if it is a private/VPN-scoped registry, check that your VPN is "+
		"connected and the host is reachable.", host)
}
