package netproxy

import (
	"testing"
)

// Hostname validator and non-canonical IPv4 spellings.
//
// validateHostname is the first gate a CONNECT or forward target passes
// through. It exempts IP literals (netip.ParseAddr succeeds) and then checks
// the remaining string against DNS-label rules. Go's netip parser rejects the
// non-canonical IPv4 spellings that libc's inet_aton accepts — hex, octal,
// and single-integer forms — so those strings fall through to the DNS-label
// path, where every character they contain (0-9, a-f, the letter x, dots) is
// legal. The proxy then treats them as ordinary hostnames: the loopback string
// guard does not match, and the filter resolves them as DNS names. On a
// resolver that honours inet_aton (or via an /etc/hosts entry) the dial lands
// on the decoded address, bypassing every address-level check.
//
// The fix rejects any string that decodes as an IPv4 address under inet_aton
// rules, so these spellings never enter the hostname path at all.

// TestSecurityHostnameValidatorRejectsInetAtonSpellings asserts that the
// validator rejects non-canonical IPv4 spellings that netip.ParseAddr does not
// accept but libc's inet_aton decodes to a real address.
func TestSecurityHostnameValidatorRejectsInetAtonSpellings(t *testing.T) {
	// Control: canonical dotted-decimal is accepted as an IP literal.
	if err := validateHostname("127.0.0.1"); err != nil {
		t.Fatalf("validateHostname(127.0.0.1) = %v, want nil: the canonical IP-literal exemption must still work", err)
	}
	// Control: a plain DNS name is accepted.
	if err := validateHostname("example.com"); err != nil {
		t.Fatalf("validateHostname(example.com) = %v, want nil: a plain hostname must be accepted", err)
	}

	for _, host := range []string{
		"0x7f.0.0.1",
		"0177.0.0.1",
		"2130706433",
		"127.1",
		"0x7f000001",
	} {
		if err := validateHostname(host); err == nil {
			t.Errorf("validateHostname(%q) = nil, want error: a non-canonical IPv4 spelling must not pass as a hostname", host)
		}
	}
}
