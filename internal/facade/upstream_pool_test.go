package facade

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
