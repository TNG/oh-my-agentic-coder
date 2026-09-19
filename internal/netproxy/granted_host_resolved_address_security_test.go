package netproxy

import (
	"context"
	"testing"
)

// Granted-host resolved-address policy.
//
// A hostname grant (allow_domain, a learned allow, or a prompt allow) admits
// the name, not the address. DNS answers can change between requests, so a
// grant that was innocent at admission time can later resolve to an address
// the sandbox must never reach. The direct path resolves once and dials the
// pinned IPs, so it is the layer that must refuse when a granted hostname
// resolves into a private range: RFC1918, CGNAT, IPv6 unique-local, loopback,
// and link-local. Without that check a hostname grant silently becomes a grant
// to whatever private address a later DNS answer points at.

// TestSecurityGrantedHostNeverCoversPrivateResolvedAddresses asserts that a
// hostname matching allow_domain is still denied when its DNS answer points at
// a private address.
func TestSecurityGrantedHostNeverCoversPrivateResolvedAddresses(t *testing.T) {
	deniedFor := func(t *testing.T, ip string) bool {
		t.Helper()
		f := NewFilter(FilterConfig{
			AllowDomains: []string{"granted.example"},
			Resolve:      staticResolver(ip),
		})
		v, _ := f.Check(context.Background(), "granted.example", 443)
		return v.Decision == Deny
	}

	// Control: a granted host resolving to a public address is allowed. This
	// proves the grant is in force and the fixture is not blanket-denying,
	// so a denial below is a real policy decision rather than a broken setup.
	if deniedFor(t, "93.184.216.34") {
		t.Fatal("a granted host resolving to a public address was denied: the grant is missing, so the denials below prove nothing")
	}

	for _, ip := range []string{
		"127.0.0.1",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"100.64.0.1",
		"fc00::1",
	} {
		if !deniedFor(t, ip) {
			t.Errorf("a granted host resolving to %s was allowed: a hostname grant must not cover a destination in a private range", ip)
		}
	}
}
