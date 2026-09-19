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
	"strings"
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

// TestChainedPathDeniesOnLocalDNSFailure pins the fall-through hardening on
// the chained-proxy admission path: when ResolveOnCheckHost is set and the
// local resolve fails (resolver error OR empty address slice), CheckHost /
// checkHostPinned must return Deny with reason "dns resolution failed" even
// when an allow_domain rule would otherwise admit the host.
//
// This is security-relevant: an attacker controlling a domain's authoritative
// DNS can answer omac's admission-time lookup with NXDOMAIN/SERVFAIL. Under
// the old fall-through (admit on the hostname alone) the upstream proxy then
// resolves the same name at connect time — possibly to a hard-denied address
// — reopening the DNS-rebinding TOCTOU this change exists to close. The
// deny-on-failure lines in checkHostPinned (filter.go, the
// `else if f.cfg.ResolveOnCheckHost` branch) must not be silently reverted.
//
// This test is mechanically red if those `err != nil || len(addrs) == 0 ->
// Deny` lines are removed: checkRules would then admit the host via the
// allow_domain rule and return Allow.
func TestChainedPathDeniesOnLocalDNSFailure(t *testing.T) {
	const host = "internal.corp"

	cases := []struct {
		name    string
		resolve func(context.Context, string) ([]netip.Addr, error)
	}{
		{
			name: "resolver_error",
			resolve: func(context.Context, string) ([]netip.Addr, error) {
				return nil, fmt.Errorf("lookup internal.corp: no such host")
			},
		},
		{
			name: "empty_address_slice",
			resolve: func(context.Context, string) ([]netip.Addr, error) {
				return nil, nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// allow_domain would admit the host on the hostname alone; the
			// deny-on-failure must outrank it when ResolveOnCheckHost is set.
			f := NewFilter(FilterConfig{
				AllowDomains:       []string{host},
				ResolveOnCheckHost: true,
				Resolve:            tc.resolve,
			})

			v, pinned := f.checkHostPinned(context.Background(), host, 443)
			if v.Decision != Deny {
				t.Fatalf("checkHostPinned(%s) decision = %v, want Deny: a hostname whose local resolve fails must not be admitted on the chained path (TOCTOU rebind window)", tc.name, v.Decision)
			}
			if v.Reason != "dns resolution failed" {
				t.Fatalf("checkHostPinned(%s) reason = %q, want %q", tc.name, v.Reason, "dns resolution failed")
			}
			if pinned != nil {
				t.Fatalf("checkHostPinned(%s) returned pinned addrs on a deny, want nil", tc.name)
			}

			// The public CheckHost entrypoint must agree.
			if got := f.CheckHost(context.Background(), host, 443); got.Decision != Deny || got.Reason != "dns resolution failed" {
				t.Fatalf("CheckHost(%s) = %+v, want Deny/\"dns resolution failed\"", tc.name, got)
			}

			// Sanity: the allow rule really would admit the host when
			// resolution succeeds, so the denial above is attributable to
			// the failure branch and not a fixture that rejects everything.
			allowF := NewFilter(FilterConfig{
				AllowDomains:       []string{host},
				ResolveOnCheckHost: true,
				Resolve:            staticResolver("93.184.216.34"),
			})
			if v := allowF.CheckHost(context.Background(), host, 443); v.Decision != Allow {
				t.Fatalf("control: CheckHost with a succeeding resolver = %v (%s), want Allow (the fixture must otherwise admit the host)", v.Decision, v.Reason)
			}
		})
	}
}

// TestChainedPathDeniesOnLocalDNSFailureEndToEnd pins the same property at
// the proxy boundary: a CONNECT to a host whose local resolve fails (with
// ResolveOnCheckHost set) is refused with 403 and the resolution-failure
// body, and the upstream proxy is never contacted.
func TestChainedPathDeniesOnLocalDNSFailureEndToEnd(t *testing.T) {
	const host = "internal.corp"

	// Upstream proxy that must never be reached for this host.
	var upstreamConns int32
	proxyLn := startSplicingUpstreamProxy(t, "127.0.0.1:1", &upstreamConns)
	defer proxyLn.Close()

	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	dialer := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	s := startProxyWithDialer(t, FilterConfig{
		AllowDomains:       []string{host},
		ResolveOnCheckHost: true,
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return nil, fmt.Errorf("lookup %s: server misbehaving", host)
		},
	}, dialer)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := basicAuth("omac", s.Token())
	fmt.Fprintf(conn, "CONNECT %s:443 HTTP/1.1\r\nHost: %s:443\r\nProxy-Authorization: %s\r\n\r\n", host, host, auth)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (chained host whose local resolve failed must be refused, not admitted on the hostname)", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "did not resolve") {
		t.Fatalf("response body does not carry the resolution-failure explanation:\n%s", body)
	}
	if got := atomic.LoadInt32(&upstreamConns); got != 0 {
		t.Fatalf("upstream proxy connections = %d, want 0 (request must be refused at admission, never chained)", got)
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
	dialer := newUpstreamProxyDialerAllowLoopback(proxyURL, []string{"registry.internal"}, t.Logf)

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
