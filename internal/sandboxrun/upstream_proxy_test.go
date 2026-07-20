package sandboxrun

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// startCountingListener starts a TCP listener on 127.0.0.1:0 that counts
// accepted connections and immediately closes them.
func startCountingListener(t *testing.T) (net.Listener, func() int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var conns atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			c.Close()
		}
	}()
	return ln, conns.Load
}

func dialThroughOmacProxy(t *testing.T, srv *netproxy.Server, targetHost string, targetPort int) []byte {
	t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", srv.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("omac:"+srv.Token()))
	fmt.Fprintf(conn, "CONNECT %s:%d HTTP/1.1\r\nHost: %s:%d\r\nProxy-Authorization: %s\r\n\r\n",
		targetHost, targetPort, targetHost, targetPort, auth)
	buf := make([]byte, 4096)
	n, _ := conn.Read(buf)
	if n == 0 {
		t.Fatal("no response from omac proxy")
	}
	return buf[:n]
}

func makeProxyServer(t *testing.T, dialer netproxy.Dialer) *netproxy.Server {
	t.Helper()
	filter := netproxy.NewFilter(netproxy.FilterConfig{
		AllowDomains: []string{"example.com"},
		Resolve: func(_ context.Context, _ string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	})
	srv, err := netproxy.NewServer(filter, dialer, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

func clearProxyEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"HTTPS_PROXY", "HTTP_PROXY", "https_proxy", "http_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(k, "")
	}
}

// TestResolveUpstreamProxySchemed verifies that a proxy URL with an
// explicit http:// or https:// scheme is parsed correctly and the
// dialer contacts the right host — not "http:" (the double-prefix bug).
func TestResolveUpstreamProxySchemed(t *testing.T) {
	cases := []struct {
		name   string
		envVal string
	}{
		{"http_scheme", ""},
		{"https_scheme", ""},
	}
	for i, tc := range cases {
		ln, connCount := startCountingListener(t)
		addr := ln.Addr().String()
		defer ln.Close()

		if i == 0 {
			tc.envVal = "http://" + addr
		} else {
			tc.envVal = "https://" + addr
		}

		t.Run(tc.name, func(t *testing.T) {
			clearProxyEnv(t)
			t.Setenv("HTTPS_PROXY", tc.envVal)

			p := &sandboxprofile.Profile{}
			d := resolveUpstreamProxy(p, &bytes.Buffer{}, t.Logf)

			_, isUpstream := d.(netproxy.ProxyAuthenticator)
			if !isUpstream {
				t.Fatalf("got %T, want upstream proxy dialer for %q", d, tc.envVal)
			}

			srv := makeProxyServer(t, d)
			resp := dialThroughOmacProxy(t, srv, "example.com", 443)

			time.Sleep(50 * time.Millisecond)
			if connCount() == 0 {
				t.Errorf("upstream proxy at %q was not contacted (response: %q)", addr, resp)
			}
		})
	}
}

// TestResolveUpstreamProxySchemeLess verifies that a proxy URL without
// a scheme (e.g. "proxy.corp:8080" or "127.0.0.1:8080") is handled
// correctly: http:// is prepended and the dialer contacts the right host.
func TestResolveUpstreamProxySchemeLess(t *testing.T) {
	ln, connCount := startCountingListener(t)
	addr := ln.Addr().String()
	defer ln.Close()

	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", addr)

	p := &sandboxprofile.Profile{}
	d := resolveUpstreamProxy(p, &bytes.Buffer{}, t.Logf)

	_, isUpstream := d.(netproxy.ProxyAuthenticator)
	if !isUpstream {
		t.Fatalf("got %T, want upstream proxy dialer for scheme-less %q", d, addr)
	}

	srv := makeProxyServer(t, d)
	resp := dialThroughOmacProxy(t, srv, "example.com", 443)

	time.Sleep(50 * time.Millisecond)
	if connCount() == 0 {
		t.Errorf("upstream proxy at %q was not contacted (response: %q)", addr, resp)
	}
}

// TestResolveUpstreamProxyEmpty verifies that an empty proxy value
// produces a direct dialer — not an upstream proxy dialer that would
// try to connect to "http:" (the double-prefix bug's failure mode).
func TestResolveUpstreamProxyEmpty(t *testing.T) {
	clearProxyEnv(t)

	p := &sandboxprofile.Profile{}
	d := resolveUpstreamProxy(p, &bytes.Buffer{}, t.Logf)

	_, isUpstream := d.(netproxy.ProxyAuthenticator)
	if isUpstream {
		t.Fatalf("got upstream proxy dialer for empty proxy env — should be direct dialer")
	}
}

// TestResolveUpstreamProxyEmptyDoesNotDialHttpColon is the regression
// guard for the original bug: HTTPS_PROXY="" must not produce a dialer
// that dials "http:" as a host. It launches the full omac proxy server
// with empty proxy env, sends a CONNECT, and verifies the response is
// NOT a 502 mentioning "http:" as the upstream proxy host.
func TestResolveUpstreamProxyEmptyDoesNotDialHttpColon(t *testing.T) {
	clearProxyEnv(t)

	p := &sandboxprofile.Profile{}
	d := resolveUpstreamProxy(p, &bytes.Buffer{}, t.Logf)

	srv := makeProxyServer(t, d)
	resp := dialThroughOmacProxy(t, srv, "example.com", 443)
	respStr := string(resp)

	if bytes.Contains(resp, []byte("http:")) && bytes.Contains(resp, []byte("upstream-error")) {
		t.Fatalf("response mentions 'http:' as upstream proxy — empty proxy value should not produce a dialer that dials http:\n%s", respStr)
	}
}
