package netproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// pin is the server-resolved, filter-approved address set the Dialer
// receives. The direct dialer dials these; the upstream-proxy dialer
// ignores them on the CONNECT path and uses them only on a NO_PROXY
// bypass. Admission control and DNS resolution are the server's job
// (Filter.Check) — the Dialer is pure transport.
func pin(ips ...string) []netip.Addr {
	addrs := make([]netip.Addr, 0, len(ips))
	for _, ip := range ips {
		addrs = append(addrs, netip.MustParseAddr(ip))
	}
	return addrs
}

func startEchoListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listener: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return ln
}

func readCONNECTRequest(t *testing.T, conn net.Conn) string {
	t.Helper()
	br := bufio.NewReader(conn)
	var b strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading CONNECT request: %v", err)
		}
		b.WriteString(line)
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if n := br.Buffered(); n > 0 {
		buf := make([]byte, n)
		_, _ = io.ReadFull(br, buf)
		b.WriteString("(buffered: ")
		b.Write(buf)
		b.WriteString(")")
	}
	return b.String()
}

// startFakeUpstreamProxy stands up a raw TCP listener that speaks the
// upstream-proxy CONNECT contract: read the CONNECT head, optionally assert
// on host/auth, respond 200 + splice as echo (or write rejectWith and close).
// The raw first request is captured in *gotRequest; *connections counts
// accepted conns. The contract is non-obvious enough to warrant this doc.
func startFakeUpstreamProxy(t *testing.T, expectedHost string, expectedAuthHeader string, rejectWith string, gotRequest *string, connections *int32) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake upstream proxy: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(connections, 1)
			go handleFakeUpstreamConn(t, conn, expectedHost, expectedAuthHeader, rejectWith, gotRequest)
		}
	}()
	return ln
}

func handleFakeUpstreamConn(t *testing.T, conn net.Conn, expectedHost string, expectedAuthHeader string, rejectWith string, gotRequest *string) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	var reqStr strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		reqStr.WriteString(line)
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	raw := reqStr.String()
	if gotRequest != nil {
		*gotRequest = raw
	}

	if expectedHost != "" {
		if !strings.Contains(raw, "CONNECT "+expectedHost+":") && !strings.Contains(raw, "CONNECT "+expectedHost+" ") {
			t.Errorf("fake upstream: CONNECT target not %q in %q", expectedHost, raw)
		}
		if !strings.Contains(raw, "Host: "+expectedHost) {
			t.Errorf("fake upstream: Host header not %q in %q", expectedHost, raw)
		}
	}
	if expectedAuthHeader != "" {
		if !strings.Contains(raw, "Proxy-Authorization: "+expectedAuthHeader) {
			t.Errorf("fake upstream: missing Proxy-Authorization %q in %q", expectedAuthHeader, raw)
		}
	} else {
		if strings.Contains(raw, "Proxy-Authorization:") {
			t.Errorf("fake upstream: unexpected Proxy-Authorization header in %q", raw)
		}
	}

	if rejectWith != "" {
		_, _ = conn.Write([]byte(rejectWith))
		return
	}

	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	if n := br.Buffered(); n > 0 {
		buf := make([]byte, n)
		_, _ = io.ReadFull(br, buf)
		_, _ = conn.Write(buf)
	}
	_, _ = io.Copy(conn, conn)
}

// TestDirectDialer verifies the direct dialer dials the pinned addresses
// it is handed and splices the connection. It performs no filtering or
// resolution of its own — that boundary is exercised at the server level
// (TestConnectDenied403NamesHost / TestServerFilterDenyBeforeUpstream).
func TestDirectDialer(t *testing.T) {
	ln := startEchoListener(t)
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	d := NewDirectDialer()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := d.DialTunnel(ctx, "localhost", port, pin("127.0.0.1"))
	if err != nil {
		t.Fatalf("DialTunnel localhost:%d: %v", port, err)
	}
	defer conn.Close()

	payload := []byte("direct-dialer-echo")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(payload) {
		t.Errorf("echo mismatch: got %q want %q", buf, payload)
	}
}

// TestDirectDialerNoAddrs verifies the direct dialer fails cleanly when
// the server hands it no addresses (e.g. an Allow verdict with an empty
// pin set should never happen, but the transport must not panic).
func TestDirectDialerNoAddrs(t *testing.T) {
	d := NewDirectDialer()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := d.DialTunnel(ctx, "anything.example", 443, nil); err == nil {
		t.Fatal("DialTunnel with no pinned addresses should return an error")
	}
}

func TestUpstreamProxyDialerCONNECT(t *testing.T) {
	var gotReq string
	var conns int32
	ln := startFakeUpstreamProxy(t, "example.com", "", "", &gotReq, &conns)
	defer ln.Close()

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Pass a pin the dialer must IGNORE: the upstream proxy does its own DNS.
	conn, err := d.DialTunnel(ctx, "example.com", 443, pin("203.0.113.9"))
	if err != nil {
		t.Fatalf("DialTunnel example.com:443: %v", err)
	}
	defer conn.Close()

	if !strings.Contains(gotReq, "CONNECT example.com:443 HTTP/1.1") {
		t.Errorf("CONNECT line missing hostname; got:\n%s", gotReq)
	}
	if !strings.Contains(gotReq, "Host: example.com:443") {
		t.Errorf("Host header missing hostname; got:\n%s", gotReq)
	}
	if strings.Contains(gotReq, "Proxy-Authorization:") {
		t.Errorf("unexpected Proxy-Authorization header; got:\n%s", gotReq)
	}

	payload := []byte("upstream-tunnel-data")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write through tunnel: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read through tunnel: %v", err)
	}
	if string(buf) != string(payload) {
		t.Errorf("tunnel echo mismatch: got %q want %q", buf, payload)
	}
}

func TestUpstreamProxyDialerAuth(t *testing.T) {
	var gotReq string
	var conns int32
	ln := startFakeUpstreamProxy(t, "example.com", "Basic "+base64.StdEncoding.EncodeToString([]byte("user:pass")), "", &gotReq, &conns)
	defer ln.Close()

	proxyURL := &url.URL{
		Scheme: "http",
		Host:   ln.Addr().String(),
		User:   url.UserPassword("user", "pass"),
	}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := d.DialTunnel(ctx, "example.com", 443, nil)
	if err != nil {
		t.Fatalf("DialTunnel: %v", err)
	}
	defer conn.Close()

	if !strings.Contains(gotReq, "Proxy-Authorization: Basic ") {
		t.Errorf("missing Proxy-Authorization header; got:\n%s", gotReq)
	}
	wantCreds := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	if !strings.Contains(gotReq, "Proxy-Authorization: Basic "+wantCreds) {
		t.Errorf("wrong Proxy-Authorization creds; got:\n%s", gotReq)
	}
}

func TestUpstreamProxyDialerNoAuthWhenNoUserinfo(t *testing.T) {
	var gotReq string
	var conns int32
	ln := startFakeUpstreamProxy(t, "example.com", "", "", &gotReq, &conns)
	defer ln.Close()

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := d.DialTunnel(ctx, "example.com", 443, nil)
	if err != nil {
		t.Fatalf("DialTunnel: %v", err)
	}
	defer conn.Close()

	if strings.Contains(gotReq, "Proxy-Authorization:") {
		t.Errorf("should not emit Proxy-Authorization without userinfo; got:\n%s", gotReq)
	}
}

func TestUpstreamProxyDialerNon200(t *testing.T) {
	var gotReq string
	var conns int32
	reject := "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic realm=\"corp\"\r\n\r\n"
	ln := startFakeUpstreamProxy(t, "", "", reject, &gotReq, &conns)
	defer ln.Close()

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := d.DialTunnel(ctx, "example.com", 443, nil)
	if err == nil {
		t.Fatal("DialTunnel should fail on 407")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("error should be *UpstreamError, got %T: %v", err, err)
	}
	if !strings.Contains(ue.StatusLine, "407") {
		t.Errorf("StatusLine should contain 407; got %q", ue.StatusLine)
	}
	if ue.ProxyHost == "" {
		t.Errorf("ProxyHost should be set")
	}
	if ue.ProxyHost != ln.Addr().String() {
		t.Errorf("ProxyHost = %q, want %q", ue.ProxyHost, ln.Addr().String())
	}
	if ue.Err != nil {
		t.Errorf("Err should be nil for non-200 response, got %v", ue.Err)
	}
}

func TestUpstreamProxyDialerNoProxyBypass(t *testing.T) {
	var conns int32
	ln := startFakeUpstreamProxy(t, "", "", "", nil, &conns)
	defer ln.Close()

	echoLn := startEchoListener(t)
	defer echoLn.Close()
	echoPort := echoLn.Addr().(*net.TCPAddr).Port

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	d := NewUpstreamProxyDialer(proxyURL, []string{"localhost"}, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// NO_PROXY match → dial the server-pinned IPs directly, never the proxy.
	conn, err := d.DialTunnel(ctx, "localhost", echoPort, pin("127.0.0.1"))
	if err != nil {
		t.Fatalf("DialTunnel localhost bypass: %v", err)
	}
	defer conn.Close()

	payload := []byte("bypass-echo")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != string(payload) {
		t.Errorf("echo mismatch: got %q want %q", buf, payload)
	}

	if got := atomic.LoadInt32(&conns); got != 0 {
		t.Errorf("upstream connections for bypass = %d, want 0", got)
	}

	// Non-match → chained through the upstream proxy (pin ignored).
	conn2, err := d.DialTunnel(ctx, "nonproxy.test", 443, pin("203.0.113.9"))
	if err != nil {
		t.Fatalf("DialTunnel nonproxy.test via upstream: %v", err)
	}
	conn2.Close()

	if got := atomic.LoadInt32(&conns); got != 1 {
		t.Errorf("upstream connections for non-bypass = %d, want 1", got)
	}
}

func TestProxyAuthHeader(t *testing.T) {
	proxyURL := &url.URL{
		Scheme: "http",
		Host:   "proxy.example:8080",
		User:   url.UserPassword("user", "pass"),
	}
	d := NewUpstreamProxyDialer(proxyURL, nil, nil)
	up, ok := d.(ProxyAuthenticator)
	if !ok {
		t.Fatal("upstreamProxyDialer should implement ProxyAuthenticator")
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:pass"))
	if got := up.ProxyAuthHeader(); got != want {
		t.Errorf("ProxyAuthHeader() = %q, want %q", got, want)
	}

	proxyURL2 := &url.URL{Scheme: "http", Host: "proxy.example:8080"}
	d2 := NewUpstreamProxyDialer(proxyURL2, nil, nil)
	up2, ok := d2.(ProxyAuthenticator)
	if !ok {
		t.Fatal("upstreamProxyDialer should implement ProxyAuthenticator")
	}
	if got := up2.ProxyAuthHeader(); got != "" {
		t.Errorf("ProxyAuthHeader() = %q, want empty", got)
	}

	dDirect := NewDirectDialer()
	if _, ok := dDirect.(ProxyAuthenticator); ok {
		t.Error("directDialer should NOT implement ProxyAuthenticator")
	}
}

func TestHostMatchesNoProxy(t *testing.T) {
	cases := []struct {
		host    string
		entries []string
		want    bool
	}{
		{"localhost", []string{"localhost"}, true},
		{"foo.localhost", []string{"localhost"}, true},
		{"example.com", []string{"example.com"}, true},
		{"sub.example.com", []string{"example.com"}, true},
		{"notexample.com", []string{"example.com"}, false},
		{"example.com", []string{"other.com"}, false},
		{"EXAMPLE.COM", []string{"example.com"}, true},
		{"example.com", []string{" EXAMPLE.com "}, true},
		{"example.com", []string{""}, false},
		{"a.b.c.test", []string{"c.test"}, true},
	}
	for _, c := range cases {
		got := hostMatchesNoProxy(c.host, c.entries)
		if got != c.want {
			t.Errorf("hostMatchesNoProxy(%q, %v) = %v, want %v", c.host, c.entries, got, c.want)
		}
	}
}

// TestUpstreamProxyDialerBufferedConn guards the bufferedConn path: bytes a
// corporate proxy pipelines right after the 200 head must reach the caller
// before raw socket reads. This is a subtle correctness invariant.
func TestUpstreamProxyDialerBufferedConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		resp := []byte("HTTP/1.1 200 Connection Established\r\n\r\nPREFIX")
		if _, err := conn.Write(resp); err != nil {
			return
		}
		_, _ = io.Copy(conn, conn)
	}()

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := d.DialTunnel(ctx, "example.com", 443, nil)
	if err != nil {
		t.Fatalf("DialTunnel: %v", err)
	}
	defer conn.Close()

	buf := make([]byte, 6)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("reading buffered prefix: %v", err)
	}
	if string(buf) != "PREFIX" {
		t.Errorf("buffered prefix = %q, want PREFIX", buf)
	}

	payload := []byte("after-prefix")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(echo) != string(payload) {
		t.Errorf("echo = %q, want %q", echo, payload)
	}
}

func TestUpstreamProxyDialerDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	proxyURL := &url.URL{Scheme: "http", Host: addr}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = d.DialTunnel(ctx, "example.com", 443, nil)
	if err == nil {
		t.Fatal("DialTunnel should fail when upstream is unreachable")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("error should be *UpstreamError, got %T: %v", err, err)
	}
	if ue.Err == nil {
		t.Errorf("Err should be set on dial failure")
	}
	if ue.StatusLine != "" {
		t.Errorf("StatusLine should be empty on dial failure, got %q", ue.StatusLine)
	}
	if ue.ProxyHost != addr {
		t.Errorf("ProxyHost = %q, want %q", ue.ProxyHost, addr)
	}
}

func TestUpstreamProxyDialer503(t *testing.T) {
	var gotReq string
	var conns int32
	reject := "HTTP/1.1 503 Service Unavailable\r\nContent-Length: 0\r\n\r\n"
	ln := startFakeUpstreamProxy(t, "", "", reject, &gotReq, &conns)
	defer ln.Close()

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := d.DialTunnel(ctx, "example.com", 443, nil)
	if err == nil {
		t.Fatal("DialTunnel should fail on 503")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("error should be *UpstreamError, got %T: %v", err, err)
	}
	if !strings.Contains(ue.StatusLine, "503") {
		t.Errorf("StatusLine should contain 503; got %q", ue.StatusLine)
	}
}

func TestUpstreamProxyDialerContextCanceled(t *testing.T) {
	var conns int32
	ln := startFakeUpstreamProxy(t, "", "", "", nil, &conns)
	defer ln.Close()

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := d.DialTunnel(ctx, "example.com", 443, nil)
	if err == nil {
		t.Fatal("DialTunnel should fail with canceled context")
	}
}

// TestUpstreamProxyDialerFilterNotEnforcedAtDialerLevel documents the layering:
// the upstream dialer itself does not enforce the filter — the Server does that
// via Filter.Check before ever calling DialTunnel. A request that reaches the
// dialer therefore always proceeds to the upstream proxy. This pins the
// boundary so a future refactor doesn't reintroduce dialer-level filtering.
func TestUpstreamProxyDialerFilterNotEnforcedAtDialerLevel(t *testing.T) {
	var conns int32
	ln := startFakeUpstreamProxy(t, "", "", "", nil, &conns)
	defer ln.Close()

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, err := d.DialTunnel(ctx, "example.com", 443, nil)
	if err != nil {
		t.Fatalf("DialTunnel: %v", err)
	}
	conn.Close()

	if got := atomic.LoadInt32(&conns); got != 1 {
		t.Errorf("upstream connections = %d, want 1 (filter enforced at server level)", got)
	}
}

func TestNewDirectDialerType(t *testing.T) {
	d := NewDirectDialer()
	if _, ok := d.(*directDialer); !ok {
		t.Errorf("NewDirectDialer should return *directDialer, got %T", d)
	}
}

func TestNewUpstreamProxyDialerType(t *testing.T) {
	proxyURL := &url.URL{Scheme: "http", Host: "proxy.example:8080"}
	d := NewUpstreamProxyDialer(proxyURL, nil, nil)
	if _, ok := d.(*upstreamProxyDialer); !ok {
		t.Errorf("NewUpstreamProxyDialer should return *upstreamProxyDialer, got %T", d)
	}
	var _ Dialer = d
	var _ ProxyAuthenticator = d.(*upstreamProxyDialer)
}

func TestUpstreamErrorError(t *testing.T) {
	ue := &UpstreamError{ProxyHost: "proxy:8080", StatusLine: "", Err: fmt.Errorf("connection refused")}
	msg := ue.Error()
	if !strings.Contains(msg, "proxy:8080") || !strings.Contains(msg, "connection refused") {
		t.Errorf("dial-failure Error() = %q", msg)
	}
	ue2 := &UpstreamError{ProxyHost: "proxy:8080", StatusLine: "HTTP/1.1 407 Proxy Authentication Required"}
	msg2 := ue2.Error()
	if !strings.Contains(msg2, "proxy:8080") || !strings.Contains(msg2, "407") {
		t.Errorf("non-200 Error() = %q", msg2)
	}
}

func TestUpstreamProxyDialerWithBodyInResponse(t *testing.T) {
	var conns int32
	reject := "HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/html\r\nContent-Length: 20\r\n\r\n<h1>Bad Gateway</h1>"
	ln := startFakeUpstreamProxy(t, "", "", reject, nil, &conns)
	defer ln.Close()

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := d.DialTunnel(ctx, "example.com", 443, nil)
	if err == nil {
		t.Fatal("DialTunnel should fail on 502")
	}
	var ue *UpstreamError
	if !errors.As(err, &ue) {
		t.Fatalf("error should be *UpstreamError, got %T: %v", err, err)
	}
	if !strings.Contains(ue.StatusLine, "502") {
		t.Errorf("StatusLine should contain 502; got %q", ue.StatusLine)
	}
}

// TestDirectDialerDialsPin verifies the direct dialer dials exactly the
// pinned address it is handed (anti-DNS-rebinding: the server resolved and
// pinned; the dialer must not re-resolve).
func TestDirectDialerDialsPin(t *testing.T) {
	ln := startEchoListener(t)
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	d := NewDirectDialer()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialTunnel(ctx, "anything.example", port, pin("127.0.0.1"))
	if err != nil {
		t.Fatalf("DialTunnel: %v", err)
	}
	defer conn.Close()

	payload := []byte("pin-test")
	_, _ = conn.Write(payload)
	buf := make([]byte, len(payload))
	_, _ = io.ReadFull(conn, buf)
	if string(buf) != string(payload) {
		t.Errorf("echo mismatch: got %q", buf)
	}
}

func TestUpstreamProxyDialerUpstreamClosesConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if line == "\r\n" || line == "\n" {
				break
			}
		}
		_, _ = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	}()

	proxyURL := &url.URL{Scheme: "http", Host: ln.Addr().String()}
	d := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := d.DialTunnel(ctx, "example.com", 443, nil)
	if err != nil {
		t.Fatalf("DialTunnel: %v", err)
	}
	defer conn.Close()

	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err == nil && n > 0 {
	} else if err != nil && err != io.EOF {
		t.Errorf("read error = %v, want io.EOF", err)
	}
}

var _ = readCONNECTRequest
