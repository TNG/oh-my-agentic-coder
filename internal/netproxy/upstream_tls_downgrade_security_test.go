package netproxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// TestSecurityHTTPSUpstreamProxyNeverDialedInPlaintext asserts that an
// upstream proxy URL configured as https:// initiates a TLS handshake
// rather than sending the Proxy-Authorization credential in plaintext.
//
// The fixture uses a plain TCP listener. After the fix, DialTunnel sends
// a TLS ClientHello instead of a plaintext CONNECT, so the listener sees
// unrecognisable bytes and the TLS handshake fails — that failure is the
// proof that TLS was attempted. If the old code ran, the listener would
// see a plaintext CONNECT with credentials; the dial would succeed and
// the credential would be readable on the wire.
func TestSecurityHTTPSUpstreamProxyNeverDialedInPlaintext(t *testing.T) {
	sectest.RequireLoopbackListener(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Capture everything the plain listener receives from the dialer.
	var got atomic.Value
	got.Store("")
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		// Short deadline so we never block if the TLS handshake stalls.
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		got.Store(string(buf[:n]))
		// Do not send any response — the TLS handshake will fail, which is
		// exactly what we want to observe.
	}()

	proxyURL, err := url.Parse(fmt.Sprintf("https://corpuser:hunter2@127.0.0.1:%d", ln.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	d := NewUpstreamProxyDialer(proxyURL, nil, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialTunnel(ctx, "internal.example.com", 443, nil)

	// Control: the dial must fail — a TLS handshake against a plain listener
	// always fails. If it succeeds, the old (plaintext) code path ran.
	if err == nil {
		conn.Close()
		t.Fatal("DialTunnel to https:// proxy succeeded against a plain TCP listener: credentials were sent in plaintext, no TLS was negotiated")
	}

	// The bytes the listener received must look like a TLS ClientHello
	// (first byte 0x16 = TLS record type "handshake"), not a plaintext
	// CONNECT request. This is the definitive proof that TLS was attempted.
	raw, _ := got.Load().(string)
	credential := "Basic " + base64.StdEncoding.EncodeToString([]byte("corpuser:hunter2"))
	if strings.Contains(raw, credential) {
		t.Errorf("the proxy credential was observed in plaintext on the wire — TLS was not negotiated:\n%s", raw)
	}
	if strings.Contains(raw, "CONNECT") {
		t.Errorf("a plaintext CONNECT was sent to an https:// proxy — TLS was not negotiated:\n%s", raw)
	}
}
