package sandboxrun

import (
	"bytes"
	"context"
	"encoding/base64"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/netproxy"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestResolveUpstreamProxySchemeLess(t *testing.T) {
	cases := []struct {
		name         string
		env          string
		wantDirect   bool
		wantUpstream bool
	}{
		{"schemeless", "proxy.corp:8080", false, true},
		{"with_scheme", "http://proxy.corp:8080", false, true},
		{"empty", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HTTPS_PROXY", tc.env)
			t.Setenv("HTTP_PROXY", "")
			t.Setenv("https_proxy", "")
			t.Setenv("http_proxy", "")
			t.Setenv("NO_PROXY", "")
			t.Setenv("no_proxy", "")

			p := &sandboxprofile.Profile{}
			d := resolveUpstreamProxy(p, &bytes.Buffer{}, func(string, ...any) {})

			_, isUpstream := d.(netproxy.ProxyAuthenticator)

			if tc.wantUpstream && !isUpstream {
				t.Fatalf("got %T, want upstream proxy dialer", d)
			}
			if tc.wantDirect && isUpstream {
				t.Fatalf("got upstream proxy dialer, want direct dialer")
			}
		})
	}
}

func TestResolveUpstreamProxySchemeLessDialsProxyHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var gotConn atomic.Bool
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		gotConn.Store(true)
		c.Close()
	}()

	t.Setenv("HTTPS_PROXY", ln.Addr().String())
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("http_proxy", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	p := &sandboxprofile.Profile{}
	d := resolveUpstreamProxy(p, &bytes.Buffer{}, t.Logf)

	filter := netproxy.NewFilter(netproxy.FilterConfig{
		AllowDomains: []string{"example.com"},
		Resolve: func(_ context.Context, _ string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
	})
	srv, err := netproxy.NewServer(filter, d, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(srv.Port())))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte("omac:"+srv.Token()))
	if _, err := conn.Write([]byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: " + auth + "\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1024)
	n, _ := conn.Read(buf)
	if n == 0 {
		t.Fatal("no response from proxy")
	}

	time.Sleep(50 * time.Millisecond)
	if !gotConn.Load() {
		t.Error("upstream proxy was not contacted — scheme-less URL did not resolve to the proxy host")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
