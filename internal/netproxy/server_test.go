package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingPrompter blocks until its channel closes; counts invocations.
type blockingPrompter struct {
	block   chan struct{}
	res     PromptResult
	started atomic.Int32
}

func (p *blockingPrompter) Prompt(host string, port int) PromptResult {
	p.started.Add(1)
	<-p.block
	return p.res
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}

// startProxy spins up a Server whose filter resolves every host to the
// given upstream listener.
func startProxy(t *testing.T, cfg FilterConfig) *Server {
	t.Helper()
	filter := NewFilter(cfg)
	s, err := NewServer(filter, NewDirectDialer(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// resolveTo maps every hostname to addr.
func resolveTo(addr string) func(context.Context, string) ([]netip.Addr, error) {
	return func(_ context.Context, _ string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(addr)}, nil
	}
}

// proxyClient builds an http.Client that uses the proxy with the token.
func proxyClient(s *Server) *http.Client {
	proxyURL, _ := url.Parse(s.ProxyURL())
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // test certs
			},
		},
		Timeout: 5 * time.Second,
	}
}

// testUpstreamAddr extracts host (always 127.0.0.1) and port.
func upstreamHostPort(t *testing.T, u string) (string, int) {
	t.Helper()
	parsed, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(parsed.Port())
	return parsed.Hostname(), port
}

func TestConnectTunnelAllowed(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "tls-hello")
	}))
	defer upstream.Close()
	_, port := upstreamHostPort(t, upstream.URL)

	// The filter "resolves" fake.example to 127.0.0.1... but the proxy
	// refuses loopback CONNECT. So resolve to the host's non-loopback
	// form is not possible in a hermetic test; instead allow loopback by
	// dialing through the pinned address path with a non-loopback name
	// mapped to 127.0.0.1 — the loopback refusal is on the *hostname*,
	// and the resolved-IP link-local check doesn't cover loopback.
	s := startProxy(t, FilterConfig{
		AllowDomains: []string{"fake.example"},
		Resolve:      resolveTo("127.0.0.1"),
	})

	client := proxyClient(s)
	resp, err := client.Get(fmt.Sprintf("https://fake.example:%d/", port))
	if err != nil {
		t.Fatalf("GET via CONNECT: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "tls-hello" {
		t.Errorf("body = %q", body)
	}
}

func TestConnectDenied403NamesHost(t *testing.T) {
	s := startProxy(t, FilterConfig{
		AllowDomains: []string{"github.com"},
		Resolve:      resolveTo("127.0.0.1"),
	})
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	auth := basicAuth("omac", s.Token())
	fmt.Fprintf(conn, "CONNECT tracker.example:443 HTTP/1.1\r\nHost: tracker.example:443\r\nProxy-Authorization: %s\r\n\r\n", auth)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tracker.example") {
		t.Errorf("deny body should name the host: %q", body)
	}
	// The denial must be clearly attributed to the sandbox, both in
	// the body and via a machine-readable header.
	if !strings.Contains(string(body), "DENIED BY THE SANDBOX") {
		t.Errorf("deny body should attribute the denial to the sandbox: %q", body)
	}
	if !strings.Contains(string(body), "allow_domain") {
		t.Errorf("deny body should point at the profile knobs: %q", body)
	}
	if resp.Header.Get("X-Omac-Sandbox") != "denied" {
		t.Errorf("X-Omac-Sandbox header missing, got %v", resp.Header)
	}
}

func TestMissingToken407(t *testing.T) {
	s := startProxy(t, FilterConfig{Resolve: resolveTo("127.0.0.1")})
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("status = %d, want 407", resp.StatusCode)
	}
}

func TestWrongToken407(t *testing.T) {
	s := startProxy(t, FilterConfig{Resolve: resolveTo("127.0.0.1")})
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nProxy-Authorization: %s\r\n\r\n",
		basicAuth("omac", strings.Repeat("0", 64)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("status = %d, want 407", resp.StatusCode)
	}
}

func TestLoopbackConnectRefused(t *testing.T) {
	s := startProxy(t, FilterConfig{Resolve: resolveTo("127.0.0.1")})
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT 127.0.0.1:8080 HTTP/1.1\r\nHost: 127.0.0.1:8080\r\nProxy-Authorization: %s\r\n\r\n",
		basicAuth("omac", s.Token()))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestPlainHTTPForward(t *testing.T) {
	var gotPath atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		fmt.Fprint(w, "plain-hello")
	}))
	defer upstream.Close()
	_, port := upstreamHostPort(t, upstream.URL)

	s := startProxy(t, FilterConfig{
		AllowDomains: []string{"fake.example"},
		Resolve:      resolveTo("127.0.0.1"),
	})
	client := proxyClient(s)
	resp, err := client.Get(fmt.Sprintf("http://fake.example:%d/some/path", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "plain-hello" {
		t.Errorf("body = %q", body)
	}
	if gotPath.Load() != "/some/path" {
		t.Errorf("path = %v", gotPath.Load())
	}
}

func TestSSEStreamingThroughForward(t *testing.T) {
	// The upstream sends two SSE events with a flush between them; the
	// client must see the first before the second is written.
	firstSeen := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: one\n\n")
		fl.Flush()
		select {
		case <-firstSeen:
		case <-time.After(3 * time.Second):
		}
		fmt.Fprint(w, "data: two\n\n")
		fl.Flush()
	}))
	defer upstream.Close()
	_, port := upstreamHostPort(t, upstream.URL)

	s := startProxy(t, FilterConfig{
		AllowDomains: []string{"sse.example"},
		Resolve:      resolveTo("127.0.0.1"),
	})
	client := proxyClient(s)
	resp, err := client.Get(fmt.Sprintf("http://sse.example:%d/events", port))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "one") {
		t.Errorf("first line = %q", line)
	}
	close(firstSeen) // unblock event two only after we got event one
	var sawTwo bool
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		if strings.Contains(line, "two") {
			sawTwo = true
			break
		}
	}
	if !sawTwo {
		t.Error("second SSE event not streamed")
	}
}

func TestEnvVars(t *testing.T) {
	s := startProxy(t, FilterConfig{Resolve: resolveTo("127.0.0.1")})
	env := s.EnvVars()
	want := s.ProxyURL()
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		if env[k] != want {
			t.Errorf("%s = %q", k, env[k])
		}
	}
	if !strings.Contains(env["NO_PROXY"], "127.0.0.1") {
		t.Errorf("NO_PROXY = %q", env["NO_PROXY"])
	}
	if !strings.Contains(want, s.Token()) || !strings.Contains(want, strconv.Itoa(s.Port())) {
		t.Errorf("ProxyURL = %q", want)
	}
}

func TestCloseTearsDownTunnels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the connection open.
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	_, port := upstreamHostPort(t, upstream.URL)

	s := startProxy(t, FilterConfig{
		AllowDomains: []string{"hold.example"},
		Resolve:      resolveTo("127.0.0.1"),
	})
	client := proxyClient(s)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, err := client.Get(fmt.Sprintf("http://hold.example:%d/", port))
		if err == nil {
			_, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	}()
	time.Sleep(100 * time.Millisecond)
	s.Close()
	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not tear down the active tunnel")
	}
}

func basicAuth(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

// startProxyWithDialer spins up a Server using the given dialer (for
// upstream-proxy chaining tests).
func startProxyWithDialer(t *testing.T, cfg FilterConfig, dialer Dialer) *Server {
	t.Helper()
	filter := NewFilter(cfg)
	s, err := NewServer(filter, dialer, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// startSplicingUpstreamProxy starts a fake upstream proxy that accepts a
// CONNECT, responds 200, and splices to the real target at targetAddr.
// Returns the listener. The number of accepted connections is tracked in
// *connections (if non-nil).
func startSplicingUpstreamProxy(t *testing.T, targetAddr string, connections *int32) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("splicing upstream proxy: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if connections != nil {
				atomic.AddInt32(connections, 1)
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				if _, err := c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
					return
				}
				target, err := net.Dial("tcp", targetAddr)
				if err != nil {
					return
				}
				defer target.Close()
				if n := br.Buffered(); n > 0 {
					buf := make([]byte, n)
					_, _ = io.ReadFull(br, buf)
					_, _ = target.Write(buf)
				}
				done := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(target, c); done <- struct{}{} }()
				go func() { _, _ = io.Copy(c, target); done <- struct{}{} }()
				<-done
				<-done
			}(conn)
		}
	}()
	return ln
}

// startRejectingUpstreamProxy starts a fake upstream proxy that responds
// to every CONNECT with rejectWith and closes.
func startRejectingUpstreamProxy(t *testing.T, rejectWith string) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("rejecting upstream proxy: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				_, _ = c.Write([]byte(rejectWith))
			}(conn)
		}
	}()
	return ln
}

func TestServerHandleConnectThroughUpstream(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "chained-tls-hello")
	}))
	defer upstream.Close()
	_, port := upstreamHostPort(t, upstream.URL)
	targetAddr := fmt.Sprintf("127.0.0.1:%d", port)

	proxyLn := startSplicingUpstreamProxy(t, targetAddr, nil)
	defer proxyLn.Close()

	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	dialer := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	s := startProxyWithDialer(t, FilterConfig{
		AllowDomains: []string{"fake.example"},
		Resolve:      resolveTo("127.0.0.1"),
	}, dialer)

	client := proxyClient(s)
	resp, err := client.Get(fmt.Sprintf("https://fake.example:%d/", port))
	if err != nil {
		t.Fatalf("GET through chained upstream: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "chained-tls-hello" {
		t.Errorf("body = %q, want chained-tls-hello", body)
	}
}

func TestServerFilterDenyBeforeUpstream(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "should-not-reach")
	}))
	defer upstream.Close()
	_, port := upstreamHostPort(t, upstream.URL)
	targetAddr := fmt.Sprintf("127.0.0.1:%d", port)

	var conns int32
	proxyLn := startSplicingUpstreamProxy(t, targetAddr, &conns)
	defer proxyLn.Close()

	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	dialer := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	s := startProxyWithDialer(t, FilterConfig{
		DenyDomains: []string{"blocked.test"},
		Resolve:     resolveTo("127.0.0.1"),
	}, dialer)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT blocked.test:443 HTTP/1.1\r\nHost: blocked.test:443\r\nProxy-Authorization: %s\r\n\r\n",
		basicAuth("omac", s.Token()))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if resp.Header.Get("X-Omac-Sandbox") != "denied" {
		t.Errorf("X-Omac-Sandbox = %q, want denied", resp.Header.Get("X-Omac-Sandbox"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "blocked.test") {
		t.Errorf("deny body should name the host: %q", body)
	}
	if got := atomic.LoadInt32(&conns); got != 0 {
		t.Errorf("upstream proxy got %d connections, want 0 (filter denies before chaining)", got)
	}
}

func TestServer502OnUpstreamFailure(t *testing.T) {
	reject := "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
	proxyLn := startRejectingUpstreamProxy(t, reject)
	defer proxyLn.Close()

	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	dialer := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	s := startProxyWithDialer(t, FilterConfig{
		AllowDomains: []string{"fake.example"},
		Resolve:      resolveTo("127.0.0.1"),
	}, dialer)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT fake.example:443 HTTP/1.1\r\nHost: fake.example:443\r\nProxy-Authorization: %s\r\n\r\n",
		basicAuth("omac", s.Token()))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if resp.Header.Get("X-Omac-Sandbox") != "upstream-error" {
		t.Errorf("X-Omac-Sandbox = %q, want upstream-error", resp.Header.Get("X-Omac-Sandbox"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "upstream proxy") {
		t.Errorf("502 body should mention upstream proxy: %q", body)
	}
}

func TestServer502OnUpstreamDialFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	proxyURL, _ := url.Parse("http://" + addr)
	dialer := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	s := startProxyWithDialer(t, FilterConfig{
		AllowDomains: []string{"fake.example"},
		Resolve:      resolveTo("127.0.0.1"),
	}, dialer)

	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", s.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT fake.example:443 HTTP/1.1\r\nHost: fake.example:443\r\nProxy-Authorization: %s\r\n\r\n",
		basicAuth("omac", s.Token()))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if resp.Header.Get("X-Omac-Sandbox") != "upstream-error" {
		t.Errorf("X-Omac-Sandbox = %q, want upstream-error", resp.Header.Get("X-Omac-Sandbox"))
	}
}

func TestServerForwardThroughUpstreamWithAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Marker") != "forwarded" {
			t.Errorf("upstream missing X-Marker header")
		}
		fmt.Fprint(w, "forwarded-plain")
	}))
	defer upstream.Close()
	_, port := upstreamHostPort(t, upstream.URL)
	targetAddr := fmt.Sprintf("127.0.0.1:%d", port)

	var conns int32
	proxyLn := startSplicingUpstreamProxy(t, targetAddr, &conns)
	defer proxyLn.Close()

	proxyURL, _ := url.Parse("http://" + proxyLn.Addr().String())
	proxyURL.User = url.UserPassword("corp", "secret")
	dialer := NewUpstreamProxyDialer(proxyURL, nil, t.Logf)

	s := startProxyWithDialer(t, FilterConfig{
		AllowDomains: []string{"fake.example"},
		Resolve:      resolveTo("127.0.0.1"),
	}, dialer)

	client := proxyClient(s)
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://fake.example:%d/fwd", port), nil)
	req.Header.Set("X-Marker", "forwarded")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET through upstream forward: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "forwarded-plain" {
		t.Errorf("body = %q, want forwarded-plain", body)
	}
}
