//go:build vuln

package netproxy

import (
	"context"
	"testing"
)

// Admission on the chained-proxy path.
//
// When omac is configured behind a corporate proxy it does not dial
// destinations itself: it hands the hostname to the upstream proxy, which
// does its own DNS. Admission then runs through CheckHost, which
// deliberately skips local resolution — pinning an IP omac will never dial
// would be meaningless.
//
// Skipping resolution is fine for rules that are about names. It is not fine
// for the hard-denies, which are about *addresses*: they exist because of
// what lives at 169.254.169.254, not because of how it is spelled. On the
// direct path Check catches those after resolving. On the chained path
// nothing does, so a name that merely points at a denied address is admitted
// and the upstream proxy dutifully connects it. Wildcard DNS services
// (nip.io, sslip.io) turn any address into such a name for free, and an
// /etc/hosts entry does the same offline.

// TestSecurityChainedProxyDeniesMetadataByResolvedAddress asserts that the
// chained path denies a hostname pointing at a hard-denied address, not just
// the literal address itself.
func TestSecurityChainedProxyDeniesMetadataByResolvedAddress(t *testing.T) {
	aliasOf := func(t *testing.T, ip string) Verdict {
		t.Helper()
		f := NewFilter(FilterConfig{Resolve: staticResolver(ip)})
		return f.CheckHost(context.Background(), "metadata.alias.example", 80)
	}

	// Control: written as a literal, the metadata address is denied on this
	// path too. That is the rule this test claims is evadable, so it has to
	// be in force before the evasion means anything.
	f := NewFilter(FilterConfig{Resolve: staticResolver("93.184.216.34")})
	if v := f.CheckHost(context.Background(), "169.254.169.254", 80); v.Decision != Deny {
		t.Fatal("CheckHost(169.254.169.254) was allowed: the hard-deny is absent on the chained path entirely")
	}

	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254", "100.100.100.200"} {
		if v := aliasOf(t, ip); v.Decision != Deny {
			t.Errorf("CheckHost admitted a hostname resolving to %s: behind an upstream proxy, any wildcard-DNS name (127.0.0.1.nip.io and friends) reaches a destination that must never be reachable", ip)
		}
	}
}

// TestSecurityChainedProxyDeniesLoopbackByResolvedAddress asserts the same
// for loopback.
//
// handleConnect and handleForward screen the raw host string before
// admission, so a literal "localhost" never gets this far. A name that
// resolves to 127.0.0.1 does: the string guard has nothing to match on, and
// CheckHost never looks at the address. The upstream proxy then opens the
// connection from the host side of the sandbox boundary.
func TestSecurityChainedProxyDeniesLoopbackByResolvedAddress(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1", "0.0.0.0"} {
		f := NewFilter(FilterConfig{Resolve: staticResolver(ip)})
		if v := f.CheckHost(context.Background(), "loopback.alias.example", 8000); v.Decision != Deny {
			t.Errorf("CheckHost admitted a hostname resolving to %s: the sandboxed agent reaches host-local services through the upstream proxy", ip)
		}
	}
}
