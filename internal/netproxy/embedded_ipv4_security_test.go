package netproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// Address-spelling invariance of the egress policy.
//
// The supported /96 layouts (64:ff9b::/96 and 64:ff9b:1::/96) carry IPv4
// in their low 32 bits. RFC 8215 allocates the broader 64:ff9b:1::/48;
// unsupported layouts in that allocation must be refused, not guessed.
// Wherever the network translates between the two address families, a
// connection to such a spelling is delivered to the embedded IPv4
// address, so it denotes the same destination as the plain IPv4 form.
// A verdict that depends on the spelling rather than on the destination
// is not a verdict about the destination at all: every hard deny that
// applies to the plain literal must apply to these spellings too, and
// destinations the policy permits must not pick up new denials.

// TestSecurityNAT64WellKnownPrefixSpellingsDenied asserts that a target
// the proxy refuses outright in IPv4 form is refused, unpromptable, in
// both supported /96 spellings as well: loopback, the unspecified
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
		// An IPv6 address may carry a %zone suffix naming an outgoing
		// interface. The zone does not change which IPv4 destination the
		// spelling embeds, so these denote the same targets as above.
		"64:ff9b::127.0.0.1%eth0",
		"64:ff9b:1::127.0.0.1%lo",
		"64:ff9b::169.254.169.254%eth0",
		"64:ff9b:1::169.254.169.254%lo",
		"64:ff9b::%25",
		"64:ff9b:1::%lo",
	} {
		if !denied(host) {
			t.Errorf("Check(%q) was allowed: this spelling denotes a hard-denied IPv4 destination and must be refused exactly like the plain form", host)
		}
	}
	if p.calls != 0 {
		t.Errorf("the prefixed spellings prompted the user %d times: a hard-denied destination must never be offered as a choice", p.calls)
	}
}

func TestSecurityCanonicalAddressPolicy(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want Decision
	}{
		{"93.184.216.34", Allow},
		{"::ffff:93.184.216.34%lo0", Allow},
		{"64:ff9b::93.184.216.34", Allow},
		{"64:ff9b:1::93.184.216.34%lo0", Allow},
		{"2606:4700:4700::1111%0", Allow},
		{"64:ff9b:2::93.184.216.34", Allow}, // outside RFC 8215's /48
		{"10.1.2.3", Allow},
		{"fd12::1", Allow},
		{"::1%0", Deny},
		{"fe80::1%lo0", Deny},
		{"::ffff:127.0.0.1%0", Deny},
		{"64:ff9b::100.100.100.200", Deny},
		{"64:ff9b:1::127.0.0.1%lo0", Deny},
		{"fd00:ec2::254%0", Deny},
		{"fd00:ec2::254%lo0", Deny},
		{"64:ff9b:1:fffe::100.100.100.200", Deny},
		{"64:ff9b:1:fffe::93.184.216.34%lo0", Deny},
		// RFC 6052 /48 and /64 layouts encode metadata outside the low 32 bits.
		{"64:ff9b:1:6464:64:c800::", Deny},
		{"64:ff9b:1:1234:64:6464:c800:0", Deny},
	} {
		t.Run(tc.ip, func(t *testing.T) {
			for _, chained := range []bool{false, true} {
				for _, host := range []string{tc.ip, "alias.example", "mixed.example"} {
					p := &fakePrompter{res: PromptResult{Allow: true}}
					answers := pin(tc.ip)
					if host == "mixed.example" {
						answers = pin("93.184.216.34", tc.ip)
					}
					f := NewFilter(FilterConfig{
						PromptEnabled: true, Prompter: p, ResolveOnCheckHost: true,
						Resolve: func(context.Context, string) ([]netip.Addr, error) {
							return slices.Clone(answers), nil
						},
					})
					check := f.Check
					if chained {
						check = f.checkHostPinned
					}
					v, pinned := check(context.Background(), host, 80)
					if v.Decision != tc.want {
						t.Fatalf("host=%s chained=%v: %+v, want %v", host, chained, v, tc.want)
					}
					if tc.want == Deny {
						if !strings.HasPrefix(v.Reason, "hard-deny") || p.calls != 0 || len(pinned) != 0 {
							t.Fatalf("hard deny prompted or pinned: %+v, calls=%d, pinned=%v", v, p.calls, pinned)
						}
					} else if p.calls != 1 || !slices.Equal(pinned, answers) {
						t.Fatalf("admission changed transport: calls=%d pinned=%v want=%v", p.calls, pinned, answers)
					}
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // Never dial an external endpoint, even with a broken guard.
			_, err := dialPinned(ctx, pin(tc.ip), 80, false)
			if err == nil || (strings.Contains(err.Error(), "dial refused: hard-deny") != (tc.want == Deny)) {
				t.Fatalf("pre-dial policy for %s: %v", tc.ip, err)
			}
		})
	}
}

type canonicalPolicyDialer struct {
	chained bool
	pins    chan []netip.Addr
}

func (d *canonicalPolicyDialer) ChainsHost(string) bool { return d.chained }
func (d *canonicalPolicyDialer) DialTunnel(_ context.Context, _ string, _ int, addrs []netip.Addr) (net.Conn, error) {
	d.pins <- slices.Clone(addrs)
	return nil, fmt.Errorf("test transport: no external dial")
}

func TestSecurityNAT64TransportPinned(t *testing.T) {
	for _, ip := range []string{"64:ff9b::5db8:d822", "64:ff9b:1::5db8:d822"} {
		t.Run(ip, func(t *testing.T) {
			echo := startEchoListener(t)
			defer echo.Close()
			upstream, dialed := startUpstreamProxyWithOwnResolution(t, ip, echo.Addr().String(), func(string) string {
				return "169.254.169.254" // A re-resolved hostname must not reach this branch.
			})
			defer upstream.Close()
			u, _ := url.Parse("http://" + upstream.Addr().String())
			s := startProxyWithDialer(t, FilterConfig{
				AllowDomains: []string{"public.example"}, ResolveOnCheckHost: true,
				Resolve: staticResolver(ip),
			}, NewUpstreamProxyDialer(u, nil, t.Logf))
			c, v, err := s.checkAndDial(context.Background(), "public.example", 80)
			if err != nil || v.Decision != Allow {
				t.Fatalf("public DNS64 admission/dial: %+v, %v", v, err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			select {
			case reached := <-dialed:
				if reached != ip {
					t.Fatalf("upstream target = %s, want original NAT64 %s", reached, ip)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("upstream never received the pinned target")
			}
			// Real CONNECT transport to a simulated translator, not native NAT64.
			if _, err := io.WriteString(c, "pinned"); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len("pinned"))
			if _, err := io.ReadFull(c, got); err != nil || string(got) != "pinned" {
				t.Fatalf("tunnel echo = %q, %v", got, err)
			}
		})
	}
}

func TestSecurityCanonicalAddressWire(t *testing.T) {
	for _, chained := range []bool{false, true} {
		for _, forward := range []bool{false, true} {
			for _, tc := range []struct {
				host string
				want int
			}{
				{"64:ff9b::93.184.216.34", 502},
				{"64:ff9b:1::93.184.216.34%0", 502},
				{"2606:4700:4700::1111%0", 502},
				{"::ffff:93.184.216.34", 502},
				{"64:ff9b:1:fffe::100.100.100.200", 403},
				{"fd00:ec2::254%0", 403},
				{"64:ff9b::127.0.0.1", 403},
			} {
				t.Run(fmt.Sprintf("chained=%v/forward=%v/%s", chained, forward, tc.host), func(t *testing.T) {
					d := &canonicalPolicyDialer{chained: chained, pins: make(chan []netip.Addr, 1)}
					s := startProxyWithDialer(t, FilterConfig{ResolveOnCheckHost: true}, d)
					c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
					if err != nil {
						t.Fatal(err)
					}
					defer c.Close()
					c.SetDeadline(time.Now().Add(5 * time.Second))
					target := net.JoinHostPort(tc.host, "80")
					start := "CONNECT " + strings.ReplaceAll(target, "%", "%25")
					if forward {
						u := &url.URL{Scheme: "http", Host: target, Path: "/"}
						start = "GET " + u.String()
					}
					fmt.Fprintf(c, "%s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: %s\r\n\r\n", start, target, basicAuth("omac", s.Token()))
					resp, err := http.ReadResponse(bufio.NewReader(c), nil)
					if err != nil {
						t.Fatal(err)
					}
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if resp.StatusCode != tc.want {
						t.Fatalf("HTTP %d want %d", resp.StatusCode, tc.want)
					}
					select {
					case pinned := <-d.pins:
						if tc.want == 403 || !slices.Equal(pinned, pin(tc.host)) {
							t.Fatalf("unexpected transport target: %v", pinned)
						}
					default:
						if tc.want == 502 {
							t.Fatal("allowed target never reached transport")
						}
					}
				})
			}
		}
	}
}

// TestSecurityEmbeddedIPv4FormsMatchPlainIPv4Policy sweeps a table of
// destinations through Filter.Check in blocklist mode, where the default
// is allow, and asserts that the IPv4-mapped spelling and both
// supported /96 spellings return the same verdict as the plain IPv4
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
			"::ffff:" + target.ip + "%eth0",
			"64:ff9b::" + target.ip,
			"64:ff9b::" + target.ip + "%eth0",
			"64:ff9b:1::" + target.ip,
			"64:ff9b:1::" + target.ip + "%lo",
		} {
			if got := verdict(spelling); got != want {
				t.Errorf("Check(%q) = %v, but Check(%q) = %v: %s gets a different verdict in an embedded spelling than in the destination it denotes", spelling, got, target.ip, want, target.what)
			}
		}
	}

	// The supported /96 prefix addresses themselves embed the unspecified
	// address, so they must inherit its verdict, with or without a zone.
	for _, host := range []string{"64:ff9b::", "64:ff9b:1::", "64:ff9b::%25", "64:ff9b:1::%lo"} {
		if got := verdict(host); got != Deny {
			t.Errorf("Check(%q) = %v, want Deny: the prefix address itself denotes the unspecified address and must share its verdict", host, got)
		}
	}
}

// TestSecurityIsLoopbackHostRejectsNAT64Spellings asserts that the raw
// destination guard agrees with the filter: a supported /96 spelling
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
		// Zone-suffixed spellings name the same destinations.
		"64:ff9b::127.0.0.1%eth0",
		"64:ff9b:1::127.0.0.1%lo",
		"64:ff9b::169.254.169.254%eth0",
		"64:ff9b::%25",
		"64:ff9b:1::%lo",
	} {
		if !IsLoopbackHost(host) {
			t.Errorf("IsLoopbackHost(%q) = false, want true: the raw-destination guard and the filter must agree that this spelling names a destination the proxy refuses to reach", host)
		}
	}
}
