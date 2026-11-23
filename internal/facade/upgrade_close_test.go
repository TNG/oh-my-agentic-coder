package facade

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sidecarauth"
)

// dialWS opens a WebSocket through the facade at addr and completes the
// handshake.
func dialWS(t *testing.T, network, addr, path string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial(network, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	fmt.Fprintf(c, "GET %s HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, key)
	br := bufio.NewReader(c)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("handshake read: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Sec-WebSocket-Accept") != wsAccept(key) {
		t.Fatalf("handshake failed: status %d accept %q", resp.StatusCode, resp.Header.Get("Sec-WebSocket-Accept"))
	}
	_ = c.SetReadDeadline(time.Time{})
	return c, br
}

// busyWS keeps a WebSocket busy with a ping every 200 ms, well inside the
// idle time, and reports when the connection ends.
type busyWS struct {
	ended chan error
	stop  chan struct{}
}

func startBusyWS(t *testing.T, f *Facade, path string) *busyWS {
	t.Helper()
	c, br := dialWS(t, "tcp", fmt.Sprintf("127.0.0.1:%d", f.TCPPort()), path)
	b := &busyWS{ended: make(chan error, 1), stop: make(chan struct{})}
	go func() {
		for {
			if _, _, err := readFrame(br); err != nil {
				b.ended <- err
				return
			}
		}
	}()
	go func() {
		tk := time.NewTicker(200 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-b.stop:
				return
			case <-tk.C:
				if writeFrame(c, opPing, []byte("p"), true) != nil {
					return
				}
			}
		}
	}()
	t.Cleanup(func() { close(b.stop) })
	return b
}

func (b *busyWS) requireClosed(t *testing.T, what string) {
	t.Helper()
	select {
	case <-b.ended:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s: busy WebSocket still open", what)
	}
}

func (b *busyWS) requireOpen(t *testing.T, what string) {
	t.Helper()
	select {
	case err := <-b.ended:
		t.Fatalf("%s: busy WebSocket was closed: %v", what, err)
	case <-time.After(500 * time.Millisecond):
	}
}

// startTwoRouteFacade serves the routes "skill" and "other", each in front
// of its own echo upstream, with an idle time far longer than any test.
func startTwoRouteFacade(t *testing.T) *Facade {
	t.Helper()
	a, b := startWSUpstream(t, false), startWSUpstream(t, false)
	f := New("", "127.0.0.1:0", []Route{
		{Mount: "skill", Upstream: sidecarauth.Upstream{Port: a.port}, State: RouteReady, Owner: "skill"},
		{Mount: "other", Upstream: sidecarauth.Upstream{Port: b.port}, State: RouteReady, Owner: "other"},
	}, 0, time.Hour, "", "test")
	if err := f.Start(context.Background()); err != nil {
		t.Skipf("tcp listen not possible: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func trackedUpgrades(f *Facade) int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	n := 0
	for _, set := range f.upgrades {
		n += len(set)
	}
	return n
}

func requireTracked(t *testing.T, f *Facade, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for trackedUpgrades(f) != want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := trackedUpgrades(f); got != want {
		t.Fatalf("tracked upgraded connections: got %d, want %d", got, want)
	}
}

// Removing a route closes the WebSocket connections opened through it, even
// busy ones, and leaves those of other routes open.
func TestRemoveRouteClosesItsWebSockets(t *testing.T) {
	t.Parallel()
	f := startTwoRouteFacade(t)
	victim := startBusyWS(t, f, "/skill/ws")
	bystander := startBusyWS(t, f, "/other/ws")
	requireTracked(t, f, 2)

	f.RemoveRoute("", "skill")
	victim.requireClosed(t, "after RemoveRoute")
	bystander.requireOpen(t, "other route after RemoveRoute")
	requireTracked(t, f, 1)
}

// Replacing a route (AddRoute with the same key) closes the connections
// opened through the old one.
func TestReplaceRouteClosesItsWebSockets(t *testing.T) {
	t.Parallel()
	f := startTwoRouteFacade(t)
	victim := startBusyWS(t, f, "/skill/ws")
	bystander := startBusyWS(t, f, "/other/ws")
	requireTracked(t, f, 2)

	port := f.upstreamPortOf("", "skill")
	f.AddRoute(Route{Mount: "skill", Upstream: sidecarauth.Upstream{Port: port}, State: RouteReady, Owner: "skill"})
	victim.requireClosed(t, "after the route was replaced")
	bystander.requireOpen(t, "other route after the replacement")
	requireTracked(t, f, 1)
}

// A refused replacement (different owner) leaves the route and its
// connections alone.
func TestRefusedReplaceKeepsWebSockets(t *testing.T) {
	t.Parallel()
	f := startTwoRouteFacade(t)
	ws := startBusyWS(t, f, "/skill/ws")
	requireTracked(t, f, 1)

	f.AddRoute(Route{Mount: "skill", Upstream: sidecarauth.Upstream{Port: 1}, State: RouteReady, Owner: "forged"})
	ws.requireOpen(t, "after a refused replacement")
	requireTracked(t, f, 1)
}

// Closing the facade closes every WebSocket it spliced.
func TestCloseClosesWebSockets(t *testing.T) {
	t.Parallel()
	f := startTwoRouteFacade(t)
	a := startBusyWS(t, f, "/skill/ws")
	b := startBusyWS(t, f, "/other/ws")
	requireTracked(t, f, 2)

	_ = f.Close()
	a.requireClosed(t, "after Close")
	b.requireClosed(t, "after Close")
	requireTracked(t, f, 0)
}

// A connection that ends on its own is forgotten: nothing stays tracked.
func TestEndedWebSocketIsUntracked(t *testing.T) {
	t.Parallel()
	f := startTwoRouteFacade(t)
	for i := 0; i < 3; i++ {
		c, _ := dialWS(t, "tcp", fmt.Sprintf("127.0.0.1:%d", f.TCPPort()), "/skill/ws")
		requireTracked(t, f, 1)
		c.Close()
		requireTracked(t, f, 0)
	}
	f.mu.RLock()
	n := len(f.upgrades)
	f.mu.RUnlock()
	if n != 0 {
		t.Fatalf("upgrade registry keeps %d empty route entries", n)
	}
}

// installedRoute is the route the facade currently has for "skill".
func installedRoute(f *Facade) *Route {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.routes[routeKey("", "skill")]
}

// tryTrack offers a throwaway connection pair for route to trackUpgrade.
func tryTrack(t *testing.T, f *Facade, route *Route) bool {
	t.Helper()
	c1, c2 := net.Pipe()
	u1, u2 := net.Pipe()
	t.Cleanup(func() { c1.Close(); c2.Close(); u1.Close(); u2.Close() })
	return f.trackUpgrade(route, &upgradedConn{client: c1, upstream: u1})
}

// An upgrade that resolved its route just before the route was removed or
// replaced, or the facade was closed, must not be registered: nothing would
// ever close it. trackUpgrade refuses it and the proxy drops the connection.
func TestTrackUpgradeRefusesAGoneRoute(t *testing.T) {
	t.Parallel()
	t.Run("installed route is accepted", func(t *testing.T) {
		f := startTwoRouteFacade(t)
		if !tryTrack(t, f, installedRoute(f)) {
			t.Fatal("trackUpgrade refused the installed route")
		}
	})
	t.Run("removed route", func(t *testing.T) {
		f := startTwoRouteFacade(t)
		old := installedRoute(f)
		f.RemoveRoute("", "skill")
		if tryTrack(t, f, old) {
			t.Fatal("trackUpgrade accepted a connection for a removed route")
		}
		requireTracked(t, f, 0)
	})
	t.Run("replaced route", func(t *testing.T) {
		f := startTwoRouteFacade(t)
		old := installedRoute(f)
		f.AddRoute(Route{Mount: "skill", Upstream: sidecarauth.Upstream{Port: f.upstreamPortOf("", "skill")}, State: RouteReady, Owner: "skill"})
		if tryTrack(t, f, old) {
			t.Fatal("trackUpgrade accepted a connection for a replaced route")
		}
		if !tryTrack(t, f, installedRoute(f)) {
			t.Fatal("trackUpgrade refused the replacement route")
		}
	})
	t.Run("closed facade", func(t *testing.T) {
		f := startTwoRouteFacade(t)
		route := installedRoute(f)
		f.Close()
		if tryTrack(t, f, route) {
			t.Fatal("trackUpgrade accepted a connection after Close")
		}
		requireTracked(t, f, 0)
	})
}

// upstreamPortOf is the port of the route for namespace/mount, 0 if absent.
func (f *Facade) upstreamPortOf(namespace, mount string) int {
	up, _ := f.Upstream(namespace, mount)
	return up.Port
}
