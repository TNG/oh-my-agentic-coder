//go:build vuln

package netproxy

import (
	"context"
	"testing"
)

// Loopback admission.
//
// The proxy runs unsandboxed, on the host side of the boundary, so any
// connection it opens on the agent's behalf reaches host-local services
// directly. Reaching host loopback through the proxy is therefore a full
// escape: the agent talks to whatever the user runs on 127.0.0.1 — a
// database, another omac session's control plane, an SSH agent forwarder.
// In-sandbox loopback traffic is supposed to go direct via a granted
// open_port, never through the proxy, so the proxy must refuse every way of
// naming a loopback destination.
//
// Two independent layers have to hold, because each sees a different
// representation of the target:
//
//   - isLoopbackHost inspects the raw host string from the CONNECT line or
//     the forward URL, before any DNS. Both handleConnect and handleForward
//     call it, so it is the single chokepoint for literal spellings.
//   - Filter.Check inspects the addresses the name actually resolves to.
//     Only that layer can catch a hostname whose DNS record points at
//     127.0.0.1, which no amount of string matching will see.

// TestSecurityLoopbackGuardRejectsHostnameVariants asserts that the raw-string
// guard recognises a loopback destination regardless of how it is spelled.
//
// Two evasions matter. A trailing root-zone dot ("localhost.") is valid DNS
// and resolves identically, but it defeats both an exact string comparison
// and netip.ParseAddr, which rejects the dotted form. And the unspecified
// address ("0.0.0.0", "::") parses fine and is not loopback by any standard
// predicate, yet connecting to it on Linux lands on 127.0.0.1 all the same.
func TestSecurityLoopbackGuardRejectsHostnameVariants(t *testing.T) {
	// Spellings the guard already recognises. They are asserted alongside the
	// evasions so a guard that stopped working entirely fails here too,
	// instead of appearing to "pass" the evasion cases for the wrong reason.
	for _, host := range []string{
		"localhost",
		"LocalHost",
		"app.localhost",
		"127.0.0.1",
		"127.0.0.53",
		"::1",
		"::ffff:127.0.0.1",
	} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true: the guard no longer recognises a plain loopback destination", host)
		}
	}

	for _, host := range []string{
		"localhost.",
		"LOCALHOST.",
		"app.localhost.",
		"127.0.0.1.",
		"::1.",
		"0.0.0.0",
		"::",
		"::ffff:0.0.0.0",
	} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true: the proxy admits this spelling and tunnels the sandboxed agent to a host-local service", host)
		}
	}
}

// TestSecurityFilterDeniesHostsResolvingToLoopback asserts that a name which
// resolves to a loopback or unspecified address is denied after resolution.
//
// The raw-string guard cannot cover this: an attacker-controlled DNS record
// (a wildcard resolver such as 127.0.0.1.nip.io, or an /etc/hosts alias) is
// an ordinary-looking hostname that happens to point home. Check already
// resolves once and pins the result to defeat DNS rebinding, so it holds the
// only addresses anyone will dial — and it is the only place this can be
// caught.
func TestSecurityFilterDeniesHostsResolvingToLoopback(t *testing.T) {
	deniedFor := func(t *testing.T, ip string) bool {
		t.Helper()
		f := NewFilter(FilterConfig{Resolve: staticResolver(ip)})
		v, _ := f.Check(context.Background(), "alias.example", 8000)
		return v.Decision == Deny
	}

	// Link-local is already hard-denied post-resolution. It is the control:
	// it proves this test reaches the resolved-address check at all, so a
	// failure below is a real gap and not a broken fixture.
	if !deniedFor(t, "169.254.169.254") {
		t.Fatal("a name resolving to link-local was admitted: the resolved-address hard-deny is gone entirely")
	}

	for _, ip := range []string{"127.0.0.1", "::1", "0.0.0.0", "::"} {
		if !deniedFor(t, ip) {
			t.Errorf("a name resolving to %s was admitted: any attacker-controlled DNS record pointing at the host tunnels the sandboxed agent to host-local services", ip)
		}
	}
}
