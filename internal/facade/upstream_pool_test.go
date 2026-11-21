package facade

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingListener counts accepted and closed connections so tests can see
// whether the facade reuses and releases its upstream connections.
type countingListener struct {
	net.Listener
	accepted atomic.Int64
	closed   atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.accepted.Add(1)
	return &countingConn{Conn: c, l: l}, nil
}

type countingConn struct {
	net.Conn
	l    *countingListener
	once sync.Once
}

func (c *countingConn) Close() error {
	c.once.Do(func() { c.l.closed.Add(1) })
	return c.Conn.Close()
}

// countingUpstream is a sidecar stand-in whose connections are counted.
type countingUpstream struct {
	ln   *countingListener
	port int
}

func newCountingUpstream(t *testing.T, h http.Handler) *countingUpstream {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		skipOrFailCI(t, "loopback listen not permitted: %v", err)
	}
	ln := &countingListener{Listener: inner}
	srv := &httptest.Server{Listener: ln, Config: &http.Server{Handler: h}}
	srv.Start()
	t.Cleanup(srv.Close)
	return &countingUpstream{ln: ln, port: inner.Addr().(*net.TCPAddr).Port}
}

func (u *countingUpstream) accepted() int64 { return u.ln.accepted.Load() }
func (u *countingUpstream) closed() int64   { return u.ln.closed.Load() }

// open is the number of connections accepted and not yet closed.
func (u *countingUpstream) open() int64 { return u.accepted() - u.closed() }

// eventually polls cond for up to one second.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// poolFacade serves the given routes through f.handle behind an httptest
// server, so tests can set unexported fields before the first request.
func poolFacade(t *testing.T, routes ...Route) (*Facade, string) {
	t.Helper()
	f := New("", "", routes, 1<<20, 30*time.Second, "", "test")
	srv := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { f.Close() })
	return f, srv.URL
}

func echoRoute(port int) Route {
	return Route{Mount: "echo", UpstreamPort: port, State: RouteReady}
}

// getStatus issues a GET and drains the body, returning the status.
func getStatus(t *testing.T, client *http.Client, url string) int {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "pong")
	})
}

func TestProxyReusesUpstreamConnections(t *testing.T) {
	up := newCountingUpstream(t, okHandler())
	_, base := poolFacade(t, echoRoute(up.port))

	for i := 0; i < 300; i++ {
		if code := getStatus(t, http.DefaultClient, base+"/echo/ping"); code != 200 {
			t.Fatalf("request %d: status %d", i, code)
		}
	}
	if n := up.accepted(); n > 2 {
		t.Errorf("accepted %d upstream connections for 300 sequential requests, want <= 2", n)
	}
	if n := up.open(); n > 2 {
		t.Errorf("%d upstream connections still open, want <= 2", n)
	}
}

func TestRouteRemovalClosesIdleUpstreamConns(t *testing.T) {
	up := newCountingUpstream(t, okHandler())
	f, base := poolFacade(t, echoRoute(up.port))

	getStatus(t, http.DefaultClient, base+"/echo/ping")
	if up.accepted() != 1 || up.closed() != 0 {
		t.Fatalf("accepted=%d closed=%d, want 1 and 0", up.accepted(), up.closed())
	}
	f.RemoveRoute("", "echo")
	eventually(t, "upstream connection to close", func() bool { return up.closed() == 1 })
}

func TestRouteReplacementClosesOldUpstream(t *testing.T) {
	a := newCountingUpstream(t, okHandler())
	b := newCountingUpstream(t, okHandler())
	f, base := poolFacade(t, echoRoute(a.port))

	getStatus(t, http.DefaultClient, base+"/echo/ping")
	f.AddRoute(echoRoute(b.port))
	eventually(t, "old upstream connection to close", func() bool { return a.closed() == 1 })
	if code := getStatus(t, http.DefaultClient, base+"/echo/ping"); code != 200 {
		t.Fatalf("status %d after replacement", code)
	}
	if b.accepted() != 1 {
		t.Errorf("new upstream accepted %d connections, want 1", b.accepted())
	}
}

func TestAliasRemovalKeepsSharedUpstream(t *testing.T) {
	up := newCountingUpstream(t, okHandler())
	ns := echoRoute(up.port)
	ns.Namespace = "dir1"
	alias := echoRoute(up.port)
	f, base := poolFacade(t, ns, alias)

	getStatus(t, http.DefaultClient, base+"/dir1/echo/ping")
	f.RemoveRoute("", "echo")
	time.Sleep(200 * time.Millisecond)
	if up.closed() != 0 {
		t.Fatalf("shared upstream connection closed while a route still uses the port")
	}
	if code := getStatus(t, http.DefaultClient, base+"/dir1/echo/ping"); code != 200 {
		t.Fatalf("status %d", code)
	}
	if up.accepted() != 1 {
		t.Errorf("accepted %d connections, want 1", up.accepted())
	}
}

func TestCloseClosesIdleUpstreamConns(t *testing.T) {
	up := newCountingUpstream(t, okHandler())
	f, base := poolFacade(t, echoRoute(up.port))

	getStatus(t, http.DefaultClient, base+"/echo/ping")
	if up.closed() != 0 {
		t.Fatalf("closed=%d before Close", up.closed())
	}
	_ = f.Close()
	eventually(t, "upstream connection to close", func() bool { return up.closed() == 1 })
}

var _ = context.Background

// busySidecar is a sidecar whose /block route holds each request until
// release is closed, so tests can fill the facade's request slots.
type busySidecar struct {
	*countingUpstream
	inflight atomic.Int64
	max      atomic.Int64
	release  chan struct{}
	once     sync.Once
}

func newBusySidecar(t *testing.T) *busySidecar {
	t.Helper()
	b := &busySidecar{release: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/block", func(w http.ResponseWriter, r *http.Request) {
		n := b.inflight.Add(1)
		defer b.inflight.Add(-1)
		for m := b.max.Load(); n > m && !b.max.CompareAndSwap(m, n); m = b.max.Load() {
		}
		select {
		case <-b.release:
		case <-r.Context().Done():
		}
		_, _ = io.WriteString(w, "done")
	})
	mux.HandleFunc("/work", func(w http.ResponseWriter, _ *http.Request) {
		n := b.inflight.Add(1)
		defer b.inflight.Add(-1)
		for m := b.max.Load(); n > m && !b.max.CompareAndSwap(m, n); m = b.max.Load() {
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = io.WriteString(w, "done")
	})
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		c, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		_ = rw.Flush()
	})
	b.countingUpstream = newCountingUpstream(t, mux)
	t.Cleanup(b.unblock)
	return b
}

func (b *busySidecar) unblock() {
	b.once.Do(func() { close(b.release) })
}

// fill starts n requests to /echo/block and waits until the sidecar holds
// all of them. The returned channel yields their statuses once they finish.
func (b *busySidecar) fill(t *testing.T, base string, n int) <-chan int {
	t.Helper()
	t.Cleanup(b.unblock) // runs before the facade server's Close, which waits for these
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, err := http.Get(base + "/echo/block")
			if err != nil {
				codes <- -1
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	eventually(t, "sidecar to hold all requests", func() bool { return b.inflight.Load() == int64(n) })
	return codes
}

func TestCapLimitsConcurrentRequestsPerSidecar(t *testing.T) {
	b := newBusySidecar(t)
	_, base := poolFacade(t, echoRoute(b.port))

	client := &http.Client{Timeout: 5 * time.Second}
	const n = 100
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, err := client.Get(base + "/echo/work")
			if err != nil {
				codes <- -1
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	for i := 0; i < n; i++ {
		if code := <-codes; code != 200 {
			t.Fatalf("status %d, want 200", code)
		}
	}
	if m := b.max.Load(); m > maxConnsPerSidecar {
		t.Errorf("sidecar saw %d concurrent requests, want <= %d", m, maxConnsPerSidecar)
	}
	if m := b.max.Load(); m < 2 {
		t.Errorf("sidecar saw %d concurrent requests, the test did not exercise concurrency", m)
	}
}

func TestQueueDeadlineReturns503(t *testing.T) {
	b := newBusySidecar(t)
	f, base := poolFacade(t, echoRoute(b.port))
	f.queueWait = 100 * time.Millisecond
	codes := b.fill(t, base, maxConnsPerSidecar)

	client := &http.Client{Timeout: 2 * time.Second}
	start := time.Now()
	resp, err := client.Get(base + "/echo/block")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", resp.StatusCode)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if got := resp.Header.Get("X-Omac-Reason"); got != "sidecar-busy" {
		t.Errorf("X-Omac-Reason = %q, want sidecar-busy", got)
	}
	if elapsed < 100*time.Millisecond || elapsed >= time.Second {
		t.Errorf("503 after %v, want between 100ms and 1s", elapsed)
	}

	b.unblock()
	for i := 0; i < maxConnsPerSidecar; i++ {
		if code := <-codes; code != 200 {
			t.Errorf("queued request finished with %d, want 200", code)
		}
	}
}

func TestSSEStreamReleasesSlot(t *testing.T) {
	up := newCountingUpstream(t, sseHandler(nil))
	f, base := poolFacade(t, echoRoute(up.port))
	f.queueWait = 100 * time.Millisecond

	for i := 0; i < maxConnsPerSidecar; i++ {
		resp, err := http.Get(base + "/echo/sse")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { resp.Body.Close() })
		if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
			t.Fatalf("stream %d: first frame: %v", i, err)
		}
	}
	client := &http.Client{Timeout: 2 * time.Second}
	if code := getStatus(t, client, base+"/echo/ping"); code != 200 {
		t.Fatalf("plain request while %d streams are open: status %d, want 200", maxConnsPerSidecar, code)
	}
}

// sseHandler streams one frame immediately, then frames every 50 ms until
// the request ends. done, when non-nil, is called once the handler returns.
func sseHandler(done func(context.Context)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sse" {
			_, _ = io.WriteString(w, "pong")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: 0\n\n")
		fl.Flush()
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-r.Context().Done():
				if done != nil {
					done(r.Context())
				}
				return
			case <-tick.C:
				if _, err := io.WriteString(w, "data: tick\n\n"); err != nil {
					return
				}
				fl.Flush()
			}
		}
	})
}

func TestSSEStreamOutlivesPoolIdleTimeout(t *testing.T) {
	up := newCountingUpstream(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; i < 12; i++ {
			if _, err := io.WriteString(w, "data: x\n\n"); err != nil {
				return
			}
			fl.Flush()
			time.Sleep(50 * time.Millisecond)
		}
	}))
	f, base := poolFacade(t, echoRoute(up.port))
	f.upstreamIdle = 100 * time.Millisecond

	resp, err := http.Get(base + "/echo/sse")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frames := 0
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == "data: x" {
			frames++
		}
	}
	if frames != 12 {
		t.Errorf("received %d frames, want 12 (stream cut off by the pool idle timeout?)", frames)
	}
	eventually(t, "upstream connection to close", func() bool { return up.open() == 0 })
}

func TestUpgradeNotBlockedByFullPool(t *testing.T) {
	b := newBusySidecar(t)
	f, base := poolFacade(t, echoRoute(b.port))
	f.queueWait = 100 * time.Millisecond
	b.fill(t, base, maxConnsPerSidecar)

	c, err := net.Dial("tcp", strings.TrimPrefix(base, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = io.WriteString(c, "GET /echo/ws HTTP/1.1\r\nHost: facade\r\nConnection: upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n")
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatalf("read handshake: %v", err)
	}
	if !strings.Contains(line, "101") {
		t.Errorf("handshake answered %q, want 101", line)
	}
}

func TestClientDisconnectClosesUpstreamStream(t *testing.T) {
	gone := make(chan struct{})
	up := newCountingUpstream(t, sseHandler(func(context.Context) { close(gone) }))
	_, base := poolFacade(t, echoRoute(up.port))

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/echo/sse", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := bufio.NewReader(resp.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-gone:
	case <-time.After(time.Second):
		t.Fatal("sidecar request context was not canceled after the client left")
	}
}

func TestHealthCheckBypassesFullPool(t *testing.T) {
	b := newBusySidecar(t)
	route := echoRoute(b.port)
	route.HealthPath = "/status"
	f, base := poolFacade(t, route)
	f.queueWait = 100 * time.Millisecond
	b.fill(t, base, maxConnsPerSidecar)

	client := &http.Client{Timeout: time.Second}
	if code := getStatus(t, client, base+"/echo/status"); code != 200 {
		t.Errorf("health check on a full pool: status %d, want 200", code)
	}
	if code := getStatus(t, client, base+"/echo/other"); code != http.StatusServiceUnavailable {
		t.Errorf("other route on a full pool: status %d, want 503", code)
	}
}
