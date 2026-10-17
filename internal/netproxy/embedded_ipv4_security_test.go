package netproxy

import (
	"context"
	"testing"
)

// Address-spelling invariance of the egress policy.
//
// The well-known translation prefixes of RFC 6052 (64:ff9b::/96) and
// RFC 8215 (64:ff9b:1::/96) carry an IPv4 literal in their low 32 bits.
// Wherever the network translates between the two address families, a
// connection to such a spelling is delivered to the embedded IPv4
// address, so it denotes the same destination as the plain IPv4 form.
// A verdict that depends on the spelling rather than on the destination
// is not a verdict about the destination at all: every hard deny that
// applies to the plain literal must apply to these spellings too, and
// destinations the policy permits must not pick up new denials.

// TestSecurityNAT64WellKnownPrefixSpellingsDenied asserts that a target
// the proxy refuses outright in IPv4 form is refused, unpromptable, in
// both well-known-prefix spellings as well: loopback, the unspecified
// address, link-local, cloud metadata, and the explicit entries of the
// hard-deny list.
func TestSecurityNAT64WellKnownPrefixSpellingsDenied(t *testing.T) {
	p := &fakePrompter{res: PromptResult{Allow: true}}
	f := NewFilter(FilterConfig{
		PromptEnabled: true,
		Prompter:      p,
		// Never consulted for an IP literal; set so a spelling that
		// stopped parsing could not fall out on a DNS error and look
		// denied for the wrong reason.
		Resolve: staticResolver("93.184.216.34"),
	})

	denied := func(host string) bool {
		v, _ := f.Check(context.Background(), host, 80)
		return v.Decision == Deny
	}

	// Controls: the plain targets are already hard-denied. If one of them
	// is not, the prefixed cases below would pass vacuously, so bail out.
	for _, host := range []string{
		"127.0.0.1",       // loopback
		"0.0.0.0",         // unspecified
		"169.254.169.254", // link-local cloud metadata
		"100.100.100.200", // explicit hard-deny list entry
		"192.0.0.192",     // explicit hard-deny list entry
	} {
		if !denied(host) {
			t.Fatalf("Check(%q) was allowed: the plain target is no longer hard-denied, so the prefixed spellings below would prove nothing", host)
		}
	}

	for _, host := range []string{
		"64:ff9b::127.0.0.1",
		"64:ff9b:1::127.0.0.1",
		"64:ff9b::0.0.0.0",
		"64:ff9b:1::0.0.0.0",
		"64:ff9b::", // the prefix address itself embeds the unspecified address
		"64:ff9b:1::",
		"64:ff9b::169.254.169.254",
		"64:ff9b:1::169.254.169.254",
		"64:ff9b::169.254.170.2",
		"64:ff9b:1::169.254.170.2",
		"64:ff9b::100.100.100.200",
		"64:ff9b:1::100.100.100.200",
		"64:ff9b::192.0.0.192",
		"64:ff9b:1::192.0.0.192",
	} {
		if !denied(host) {
			t.Errorf("Check(%q) was allowed: this spelling denotes a hard-denied IPv4 destination and must be refused exactly like the plain form", host)
		}
	}
	if p.calls != 0 {
		t.Errorf("the prefixed spellings prompted the user %d times: a hard-denied destination must never be offered as a choice", p.calls)
	}
}

// TestSecurityEmbeddedIPv4FormsMatchPlainIPv4Policy sweeps a table of
// destinations through Filter.Check in blocklist mode, where the default
// is allow, and asserts that the IPv4-mapped spelling and both
// well-known-prefix spellings return the same verdict as the plain IPv4
// literal. Every destination in the table is one the policy refuses
// outright, so a spelling that slips past normalization shows up as a
// verdict mismatch rather than a silent allow.
func TestSecurityEmbeddedIPv4FormsMatchPlainIPv4Policy(t *testing.T) {
	f := NewFilter(FilterConfig{})
	verdict := func(host string) Decision {
		v, _ := f.Check(context.Background(), host, 443)
		return v.Decision
	}

	// Controls: ordinary public destinations keep their allow verdict in
	// the spellings the policy already accepts, so a filter that denied
	// everything could not pass the sweep below by accident.
	if verdict("93.184.216.34") != Allow {
		t.Fatal("an ordinary public IPv4 host was denied: the fixture is broken, not the security property")
	}
	if verdict("::ffff:93.184.216.34") != Allow {
		t.Fatal("the IPv4-mapped spelling of an ordinary public host was denied: verdicts for the mapped form must not change")
	}
	if verdict("2606:4700:4700::1111") != Allow {
		t.Fatal("an ordinary public IPv6 host was denied: only spellings that embed an IPv4 destination are covered here")
	}

	for _, target := range []struct{ ip, what string }{
		{"127.0.0.1", "loopback"},
		{"0.0.0.0", "the unspecified address, which Linux routes to loopback"},
		{"169.254.0.1", "link-local"},
		{"169.254.169.254", "link-local cloud metadata"},
		{"169.254.170.2", "link-local task metadata"},
		{"100.100.100.200", "an explicit hard-deny list entry"},
		{"192.0.0.192", "an explicit hard-deny list entry"},
	} {
		want := verdict(target.ip)
		if want != Deny {
			t.Fatalf("Check(%q) = %v, want Deny: the plain target is not refused, so the spelling sweep for it proves nothing", target.ip, want)
		}
		for _, spelling := range []string{
			"::ffff:" + target.ip,
			"64:ff9b::" + target.ip,
			"64:ff9b:1::" + target.ip,
		} {
			if got := verdict(spelling); got != want {
				t.Errorf("Check(%q) = %v, but Check(%q) = %v: %s gets a different verdict in an embedded spelling than in the destination it denotes", spelling, got, target.ip, want, target.what)
			}
		}
	}

	// The well-known prefix addresses themselves embed the unspecified
	// address, so they must inherit its verdict.
	for _, host := range []string{"64:ff9b::", "64:ff9b:1::"} {
		if got := verdict(host); got != Deny {
			t.Errorf("Check(%q) = %v, want Deny: the prefix address itself denotes the unspecified address and must share its verdict", host, got)
		}
	}
}

// TestSecurityIsLoopbackHostRejectsNAT64Spellings asserts that the raw
// destination guard agrees with the filter: a well-known-prefix spelling
// of a loopback, unspecified, or link-local destination names a local
// destination and is refused before the request is admitted.
func TestSecurityIsLoopbackHostRejectsNAT64Spellings(t *testing.T) {
	// Controls: the plain spellings are already recognised. If these fail,
	// the guard is gone and the prefixed cases below would pass vacuously.
	for _, host := range []string{"127.0.0.1", "::1", "0.0.0.0", "::ffff:127.0.0.1"} {
		if !IsLoopbackHost(host) {
			t.Fatalf("IsLoopbackHost(%q) = false, want true: the guard no longer recognises the plain spelling", host)
		}
	}
	if IsLoopbackHost("93.184.216.34") {
		t.Fatal("IsLoopbackHost(93.184.216.34) = true, want false: a public address must not be treated as local")
	}

	for _, host := range []string{
		"64:ff9b::127.0.0.1",
		"64:ff9b:1::127.0.0.1",
		"64:ff9b::0.0.0.0",
		"64:ff9b:1::0.0.0.0",
		"64:ff9b::",
		"64:ff9b:1::",
		"64:ff9b::169.254.169.254",
		"64:ff9b:1::169.254.169.254",
	} {
		if !IsLoopbackHost(host) {
			t.Errorf("IsLoopbackHost(%q) = false, want true: the raw-destination guard and the filter must agree that this spelling names a destination the proxy refuses to reach", host)
		}
	}
}
