//go:build vuln

package netproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// TestSecurityHTTPSUpstreamProxyNeverDialedInPlaintext asserts that an
// upstream proxy URL configured as https:// results in a TLS-negotiated
// connection to that proxy — not a plain TCP socket carrying the
// Proxy-Authorization credential in the clear.
//
// dialer.go's chained-proxy dial path is bare net.Dialer.DialContext
// followed directly by writing the CONNECT request and the
// Proxy-Authorization header; the package imports no crypto/tls at all.
// An "https://" scheme in the upstream_proxy value is accepted and
// silently treated exactly like "http://".
func TestSecurityHTTPSUpstreamProxyNeverDialedInPlaintext(t *testing.T) {
	sectest.RequireLoopbackListener(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	var got atomic.Value
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		head, _ := br.ReadString('\n')
		var headers strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil || line == "\r\n" || line == "\n" {
				break
			}
			headers.WriteString(line)
		}
		got.Store(head + headers.String())
		io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		time.Sleep(200 * time.Millisecond)
	}()

	proxyURL, err := url.Parse(fmt.Sprintf("https://corpuser:hunter2@127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	d := NewUpstreamProxyDialer(proxyURL, nil, nil)

	conn, err := d.DialTunnel(context.Background(), "internal.example.com", 443, nil)
	if err != nil {
		t.Fatalf("DialTunnel to https:// proxy: %v", err)
	}
	conn.Close()

	v, _ := got.Load().(string)

	// Control: the plaintext listener did receive a CONNECT request at
	// all, so the fixture actually reached the proxy dial path.
	if !strings.Contains(v, "CONNECT internal.example.com:443") {
		t.Fatalf("control: no plaintext CONNECT observed at all (%q): the fixture is broken, not the security property", v)
	}

	credential := "Basic " + base64.StdEncoding.EncodeToString([]byte("corpuser:hunter2"))
	if strings.Contains(v, credential) {
		t.Errorf("an https:// upstream_proxy URL was dialed as plain TCP: the proxy credential was written to the wire in the clear, no TLS ever negotiated:\n%s", v)
	}
}
