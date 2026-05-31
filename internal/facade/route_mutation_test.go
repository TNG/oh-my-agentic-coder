package facade

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// requireLoopback skips the test when this environment forbids dialing
// 127.0.0.1 (some sandboxes deny loopback TCP connect). Mirrors the
// unix-socket probe-and-skip in facade_test.go.
func requireLoopback(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("loopback listen not permitted:", err)
	}
	defer ln.Close()
	c, err := net.DialTimeout("tcp", ln.Addr().String(), time.Second)
	if err != nil {
		t.Skip("loopback dial not permitted in this environment:", err)
	}
	c.Close()
}

// newUpstream returns an httptest server that echoes the request path it
// received (after facade rewriting) and its bound port.
func newUpstream(t *testing.T) (*httptest.Server, int) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "path=%s prefix=%s", r.URL.Path, r.Header.Get("X-Forwarded-Prefix"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return srv, port
}

// startTCPFacade starts a facade bound to an ephemeral TCP port (no unix
// socket) and returns its base URL.
func startTCPFacade(t *testing.T, routes []Route) (*Facade, string) {
	t.Helper()
	requireLoopback(t)
	f := New("", "127.0.0.1:0", routes, 1<<20, 30*time.Second, "", "test")
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	t.Cleanup(func() { f.Close() })
	return f, fmt.Sprintf("http://127.0.0.1:%d", f.TCPPort())
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestResolveUnit exercises the routing-table resolution logic directly,
// with no network — so it runs even in sandboxes that forbid loopback.
func TestResolveUnit(t *testing.T) {
	f := New("", "", nil, 0, 0, "", "test")
	f.routes = map[string]*Route{}
	add := func(ns, mount string) {
		r := Route{Mount: mount, Namespace: ns, UpstreamPort: 1}
		f.routes[r.key()] = &r
	}
	add("", "flat")
	add("tokAAAA", "slack")
	add(GlobalNamespace, "weather")

	cases := []struct {
		path    string
		wantKey string // route key we expect, or "" for miss
		wantRst string
	}{
		{"/flat/x/y", "flat", "x/y"},
		{"/flat", "flat", ""},
		{"/tokAAAA/slack/channels", "tokAAAA/slack", "channels"},
		{"/tokAAAA/slack", "tokAAAA/slack", ""},
		{"/__global__/weather/today", "__global__/weather", "today"},
		{"/tokZZZZ/slack/x", "", ""}, // unknown namespace
		{"/unknownflat/x", "", ""},   // unknown flat
		{"/", "", ""},                // root
	}
	for _, c := range cases {
		route, rest, ok := f.resolve(c.path)
		if c.wantKey == "" {
			if ok {
				t.Errorf("resolve(%q) unexpectedly matched key %q", c.path, route.key())
			}
			continue
		}
		if !ok {
			t.Errorf("resolve(%q) = miss, want key %q", c.path, c.wantKey)
			continue
		}
		if route.key() != c.wantKey {
			t.Errorf("resolve(%q) key = %q, want %q", c.path, route.key(), c.wantKey)
		}
		if rest != c.wantRst {
			t.Errorf("resolve(%q) rest = %q, want %q", c.path, rest, c.wantRst)
		}
	}
}

func TestRouteKey(t *testing.T) {
	if k := (Route{Mount: "slack"}).key(); k != "slack" {
		t.Errorf("flat key = %q, want slack", k)
	}
	if k := (Route{Mount: "slack", Namespace: "tok"}).key(); k != "tok/slack" {
		t.Errorf("ns key = %q, want tok/slack", k)
	}
}

func TestEmptyRouteTableStarts(t *testing.T) {
	f, base := startTCPFacade(t, nil)
	if f.TCPPort() == 0 {
		t.Fatal("expected a bound TCP port")
	}
	// Status endpoint works with zero routes.
	if code, _ := get(t, base+"/"); code != 200 {
		t.Fatalf("status code = %d, want 200", code)
	}
	// Any mount is unknown.
	if code, _ := get(t, base+"/nope/x"); code != 404 {
		t.Fatalf("unknown mount code = %d, want 404", code)
	}
}

func TestAddRemoveRouteFlat(t *testing.T) {
	_, port := newUpstream(t)
	f, base := startTCPFacade(t, nil)

	code, _ := get(t, base+"/demo/hello")
	if code != 404 {
		t.Fatalf("before AddRoute: code = %d, want 404", code)
	}

	f.AddRoute(Route{Mount: "demo", UpstreamPort: port, Skill: "demo"})
	code, body := get(t, base+"/demo/hello/world")
	if code != 200 {
		t.Fatalf("after AddRoute: code = %d, want 200 (body=%q)", code, body)
	}
	if !strings.Contains(body, "path=/hello/world") {
		t.Errorf("rewritten path wrong: %q", body)
	}
	if !strings.Contains(body, "prefix=/demo") {
		t.Errorf("X-Forwarded-Prefix wrong: %q", body)
	}

	f.RemoveRoute("", "demo")
	if code, _ := get(t, base+"/demo/hello"); code != 404 {
		t.Fatalf("after RemoveRoute: code = %d, want 404", code)
	}
}

func TestNamespacedRouting(t *testing.T) {
	_, portA := newUpstream(t)
	_, portB := newUpstream(t)
	f, base := startTCPFacade(t, nil)

	// Two directories each with a "slack" skill, plus one global skill.
	f.AddRoute(Route{Mount: "slack", Namespace: "tokAAAA", UpstreamPort: portA, Skill: "slack"})
	f.AddRoute(Route{Mount: "slack", Namespace: "tokBBBB", UpstreamPort: portB, Skill: "slack"})

	codeA, bodyA := get(t, base+"/tokAAAA/slack/channels")
	if codeA != 200 || !strings.Contains(bodyA, "path=/channels") {
		t.Fatalf("ns A: code=%d body=%q", codeA, bodyA)
	}
	if !strings.Contains(bodyA, "prefix=/tokAAAA/slack") {
		t.Errorf("ns A prefix wrong: %q", bodyA)
	}
	codeB, bodyB := get(t, base+"/tokBBBB/slack/channels")
	if codeB != 200 || !strings.Contains(bodyB, "prefix=/tokBBBB/slack") {
		t.Fatalf("ns B: code=%d body=%q", codeB, bodyB)
	}

	// A token that was never activated cannot reach either skill.
	if code, _ := get(t, base+"/tokZZZZ/slack/channels"); code != 404 {
		t.Fatalf("unknown ns: code=%d, want 404", code)
	}

	// Removing one namespace leaves the other intact.
	f.RemoveRoute("tokAAAA", "slack")
	if code, _ := get(t, base+"/tokAAAA/slack/x"); code != 404 {
		t.Fatalf("removed ns A still routes: code=%d", code)
	}
	if code, _ := get(t, base+"/tokBBBB/slack/x"); code != 200 {
		t.Fatalf("ns B should still route: code=%d", code)
	}
}

func TestGlobalNamespaceRouting(t *testing.T) {
	_, port := newUpstream(t)
	f, base := startTCPFacade(t, nil)
	f.AddRoute(Route{Mount: "weather", Namespace: GlobalNamespace, UpstreamPort: port, Skill: "weather"})

	code, body := get(t, base+"/__global__/weather/today")
	if code != 200 || !strings.Contains(body, "path=/today") {
		t.Fatalf("global route: code=%d body=%q", code, body)
	}
	if !strings.Contains(body, "prefix=/__global__/weather") {
		t.Errorf("global prefix wrong: %q", body)
	}
}

func TestStubRoutes(t *testing.T) {
	f, base := startTCPFacade(t, nil)

	f.AddRoute(Route{
		Mount: "email", Namespace: "tokAAAA", Skill: "email",
		State: RoutePendingCredentials, Detail: "set EMAIL_API_KEY",
	})
	code, body := get(t, base+"/tokAAAA/email/send")
	if code != http.StatusConflict {
		t.Fatalf("pending: code=%d, want 409 (body=%q)", code, body)
	}
	if !strings.Contains(body, "pending-credentials") || !strings.Contains(body, "EMAIL_API_KEY") {
		t.Errorf("pending body missing detail: %q", body)
	}

	f.AddRoute(Route{
		Mount: "broke", Namespace: "tokAAAA", Skill: "broke",
		State: RouteBroken, Detail: "omac.yaml invalid",
	})
	code, body = get(t, base+"/tokAAAA/broke/x")
	if code != http.StatusBadGateway {
		t.Fatalf("broken: code=%d, want 502 (body=%q)", code, body)
	}
	if !strings.Contains(body, "skill-broken") {
		t.Errorf("broken body wrong: %q", body)
	}
}

func TestStubSwappedForLive(t *testing.T) {
	_, port := newUpstream(t)
	f, base := startTCPFacade(t, nil)

	// Start as pending-credentials.
	f.AddRoute(Route{Mount: "slack", Namespace: "tokAAAA", Skill: "slack", State: RoutePendingCredentials, Detail: "x"})
	if code, _ := get(t, base+"/tokAAAA/slack/x"); code != http.StatusConflict {
		t.Fatalf("expected 409 before swap")
	}
	// Swap to live (AddRoute replaces by key).
	f.AddRoute(Route{Mount: "slack", Namespace: "tokAAAA", Skill: "slack", UpstreamPort: port})
	if code, body := get(t, base+"/tokAAAA/slack/x"); code != 200 {
		t.Fatalf("expected 200 after swap, got %d (%q)", code, body)
	}
}
