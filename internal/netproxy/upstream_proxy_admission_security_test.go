package netproxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
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

// TestSecurityChainedProxyPinsResolvedAddress asserts that on the
// upstream-proxy path the address actually connected is the address that was
// resolved and checked at admission time.
//
// The upstream proxy performs its own resolution of whatever target appears in
// the CONNECT line it receives. When that target is a hostname, the upstream's
// answer is independent of omac's admission-time answer and is not covered by
// the admission check at all. The address omac checked is therefore only
// meaningful if it is the address omac actually hands to the upstream proxy:
// the CONNECT line must carry the resolved IP literal, not the hostname. This
// test stands up an upstream proxy that resolves any hostname to an address
// the admission pipeline would never permit, and confirms the tunnel never
// reaches that address.
func TestSecurityChainedProxyPinsResolvedAddress(t *testing.T) {
	echo := startEchoListener(t)
	defer echo.Close()

	const approvedIP = "93.184.216.34"
	const hostname = "target.example"

	upstreamLn, dialed := startUpstreamProxyWithOwnResolution(t, approvedIP, echo.Addr().String(), func(string) string {
		return "169.254.169.254"
	})
	defer upstreamLn.Close()

	proxyURL, _ := url.Parse("http://" + upstreamLn.Addr().String())
	dialer := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	s := startProxyWithDialer(t, FilterConfig{
		AllowDomains:       []string{hostname},
		ResolveOnCheckHost: true,
		Resolve:            staticResolver(approvedIP),
	}, dialer)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := basicAuth("omac", s.Token())
	fmt.Fprintf(conn, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\nProxy-Authorization: %s\r\n\r\n", hostname, hostname, auth)

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	resp.Body.Close()

	// The address the upstream proxy actually opened a connection to must be
	// the admission-approved address. A hostname target would have let the
	// upstream resolve to an address the admission pipeline never permitted.
	select {
	case reached := <-dialed:
		if reached != approvedIP {
			t.Fatalf("upstream proxy reached %q, want the admission-approved address %q: the chained path handed the upstream a target whose resolved address was never checked", reached, approvedIP)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream proxy was never contacted")
	}

	// The tunnel must reach the approved destination behind the upstream proxy.
	payload := []byte("pinned-destination-echo")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("tunnel echo mismatch: got %q want %q", got, payload)
	}
}

// startUpstreamProxyWithOwnResolution stands up a fake upstream proxy that
// resolves the CONNECT target itself: an IP literal is connected to as-is, a
// hostname is resolved by resolve. The address the upstream actually connects
// to is reported on the returned channel (one value per accepted CONNECT).
// When that address equals approvedIP the tunnel is spliced to echoAddr;
// otherwise the connection is dropped after the address is reported, modeling
// an upstream that connected somewhere omac never approved.
func startUpstreamProxyWithOwnResolution(t *testing.T, approvedIP, echoAddr string, resolve func(string) string) (net.Listener, <-chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream proxy with own resolution: %v", err)
	}
	dialed := make(chan string, 16)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				var firstLine string
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					if firstLine == "" {
						firstLine = line
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				target := strings.TrimPrefix(strings.TrimSpace(firstLine), "CONNECT ")
				if i := strings.LastIndex(target, " "); i >= 0 {
					target = target[:i]
				}
				target = strings.TrimSpace(target)
				host, _, err := net.SplitHostPort(target)
				if err != nil {
					return
				}
				var reached string
				if net.ParseIP(host) != nil {
					reached = host
				} else {
					reached = resolve(host)
				}
				select {
				case dialed <- reached:
				default:
				}
				if reached != approvedIP {
					return
				}
				if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
					return
				}
				echo, err := net.Dial("tcp", echoAddr)
				if err != nil {
					return
				}
				defer echo.Close()
				if n := r.Buffered(); n > 0 {
					buf := make([]byte, n)
					_, _ = io.ReadFull(r, buf)
					_, _ = echo.Write(buf)
				}
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(echo, c); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, echo); done <- struct{}{} }()
				<-done
				<-done
			}(c)
		}
	}()
	return ln, dialed
}
