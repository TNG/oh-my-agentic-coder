package netproxy

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync/atomic"
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
		f := NewFilter(FilterConfig{Resolve: staticResolver(ip), ResolveOnCheckHost: true})
		return f.CheckHost(context.Background(), "metadata.alias.example", 80)
	}

	// Control: written as a literal, the metadata address is denied on this
	// path too. That is the rule this test claims is evadable, so it has to
	// be in force before the evasion means anything.
	f := NewFilter(FilterConfig{Resolve: staticResolver("93.184.216.34"), ResolveOnCheckHost: true})
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
	// Control: this path admits an ordinary host, so a denial below is a
	// decision rather than a fixture that rejects everything.
	{
		f := NewFilter(FilterConfig{Resolve: staticResolver("93.184.216.34")})
		if v := f.CheckHost(context.Background(), "example.com", 443); v.Decision != Allow {
			t.Fatalf("an ordinary public host was denied on the chained path (%s): the fixture is broken, not the security property", v.Reason)
		}
	}

	for _, ip := range []string{"127.0.0.1", "::1", "0.0.0.0"} {
		f := NewFilter(FilterConfig{Resolve: staticResolver(ip), ResolveOnCheckHost: true})
		if v := f.CheckHost(context.Background(), "loopback.alias.example", 8000); v.Decision != Deny {
			t.Errorf("CheckHost admitted a hostname resolving to %s: the sandboxed agent reaches host-local services through the upstream proxy", ip)
		}
	}
}

// TestSecurityUpstreamProxyPinsResolvedAddressOrAborts asserts that the
// chained path refuses to tunnel a hostname whose resolved address lands in a
// private range, even when the hostname itself matches an allow rule.
//
// On the chained path the server admits on the hostname and the upstream proxy
// does its own DNS. A hostname that resolves to a private address is therefore
// admitted and the upstream dutifully connects — unless the resolved address is
// re-validated immediately before the CONNECT is issued. The test uses a
// flipping resolver: the first call (admission) returns a public address so
// the host is allowed, and the second call (pre-CONNECT re-validation) returns
// a private address. The dialer must abort before the upstream is contacted,
// closing the TOCTOU gap between admission and the upstream's own DNS.
func TestSecurityUpstreamProxyPinsResolvedAddressOrAborts(t *testing.T) {
	echo := startEchoListener(t)
	defer echo.Close()

	var upstreamConns int32
	proxyLn := startSplicingUpstreamProxy(t, echo.Addr().String(), &upstreamConns)
	defer proxyLn.Close()

	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	dialer := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	// flippingResolver returns a public address on the first call (admission)
	// and a private address on every subsequent call (pre-CONNECT
	// re-validation), simulating a DNS rebinding attack between admission and
	// CONNECT.
	var calls atomic.Int32
	resolve := func(_ context.Context, _ string) ([]netip.Addr, error) {
		if calls.Add(1) == 1 {
			return pin("93.184.216.34"), nil // public, admitted
		}
		return pin("10.0.0.1"), nil // private, must abort
	}

	s := startProxyWithDialer(t, FilterConfig{
		AllowDomains:       []string{"chained.example"},
		Resolve:            resolve,
		ResolveOnCheckHost: true,
	}, dialer)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := basicAuth("omac", s.Token())
	fmt.Fprintf(conn, "CONNECT chained.example:443 HTTP/1.1\r\nHost: chained.example:443\r\nProxy-Authorization: %s\r\n\r\n", auth)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Errorf("CONNECT was tunneled (200) for a host whose DNS flipped to 10.0.0.1: the chained path must abort before the upstream is contacted")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (pre-CONNECT re-validation denial, not an upstream error)", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&upstreamConns); got != 0 {
		t.Errorf("upstream proxy was contacted %d time(s), want 0: a hostname resolving to a private address must not reach the upstream CONNECT", got)
	}
}
