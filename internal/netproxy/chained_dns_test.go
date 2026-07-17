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

// TestChainedPathAllowsInternalOnlyHost pins the chained-path DNS
// behavior: on the upstream-proxy path the server admits the host on the
// hostname alone (Filter.CheckHost) and does NOT resolve it locally, so a
// corporate-internal host that only resolves behind the proxy still
// tunnels through. Regression guard for the local-pre-resolution gap.
func TestChainedPathAllowsInternalOnlyHost(t *testing.T) {
	echo := startEchoListener(t)
	defer echo.Close()
	targetAddr := echo.Addr().String()

	var conns int32
	proxyLn := startSplicingUpstreamProxy(t, targetAddr, &conns)
	defer proxyLn.Close()

	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	dialer := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	var resolves atomic.Int32
	s := startProxyWithDialer(t, FilterConfig{
		AllowDomains: []string{"internal.corp"},
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			resolves.Add(1)
			return nil, fmt.Errorf("lookup %s: no such host", host)
		},
	}, dialer)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := basicAuth("omac", s.Token())
	fmt.Fprintf(conn, "CONNECT internal.corp:443 HTTP/1.1\r\nHost: internal.corp:443\r\nProxy-Authorization: %s\r\n\r\n", auth)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (chained host admitted on hostname, upstream does DNS)", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&conns); got != 1 {
		t.Fatalf("upstream proxy connections = %d, want 1 (request chained through)", got)
	}
	if got := resolves.Load(); got != 0 {
		t.Fatalf("local DNS resolve ran %d times on the chained path, want 0", got)
	}
}

// TestNoProxyBypassStillResolvesLocally pins the complementary boundary:
// a NO_PROXY-bypassed host is dialed directly, so it MUST still be
// resolved and pinned locally (anti-DNS-rebinding). CheckHost's
// no-resolve path must not leak onto the bypass path.
func TestNoProxyBypassStillResolvesLocally(t *testing.T) {
	echo := startEchoListener(t)
	defer echo.Close()
	echoPort := echo.Addr().(*net.TCPAddr).Port

	// Upstream proxy that must NOT be contacted for the bypassed host.
	var upstreamConns int32
	proxyLn := startSplicingUpstreamProxy(t, echo.Addr().String(), &upstreamConns)
	defer proxyLn.Close()

	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	dialer := NewUpstreamProxyDialer(proxyURL, []string{"registry.internal"}, t.Logf)

	var resolves atomic.Int32
	s := startProxyWithDialer(t, FilterConfig{
		AllowDomains: []string{"registry.internal"},
		Resolve: func(_ context.Context, _ string) ([]netip.Addr, error) {
			resolves.Add(1)
			return pin("127.0.0.1"), nil
		},
	}, dialer)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := basicAuth("omac", s.Token())
	fmt.Fprintf(conn, "CONNECT registry.internal:%d HTTP/1.1\r\nHost: registry.internal:%d\r\nProxy-Authorization: %s\r\n\r\n", echoPort, echoPort, auth)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (bypassed host dialed direct)", resp.StatusCode)
	}
	if got := resolves.Load(); got != 1 {
		t.Fatalf("local DNS resolve ran %d times on the NO_PROXY bypass path, want 1 (pinning preserved)", got)
	}
	if got := atomic.LoadInt32(&upstreamConns); got != 0 {
		t.Fatalf("upstream proxy connections = %d, want 0 (host was NO_PROXY-bypassed)", got)
	}
}
