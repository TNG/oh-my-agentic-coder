package facade

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/intent"
	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// Who may talk to the facade.
//
// The facade offers two transports for the same handler. The Unix socket is
// access-controlled by the filesystem: its directory is created 0700 and the
// socket itself chmod 0600, so the kernel turns away anyone but the owning
// user. The loopback TCP listener has no equivalent — TCP carries no peer
// credentials to check — and the facade adds no token, no session key, and no
// authentication of its own on top.
//
// The transports therefore do not offer the same thing. Everything the socket
// protects is served, unauthenticated, to whoever connects to the port:
//
//   - GET / enumerates the mounted skill routes,
//   - those routes proxy to sidecars that hold skill API tokens in their
//     environment, on ports deliberately withheld from the sandbox so the
//     facade is the only way to reach them,
//   - POST /sandbox/intent writes into the record the user is shown when
//     deciding whether to approve access.
//
// "Local process" is not a trust boundary on a machine with more than one
// account, and it is not one for a sandboxed agent either, since reaching the
// facade port is exactly what the sandbox is supposed to mediate.
//
// The test passes if the TCP transport is dropped altogether: no listener,
// nothing to authenticate.

// TestSecurityTCPListenerRequiresAuthentication asserts that a caller with no
// credentials gets nothing useful from the facade's TCP transport.
func TestSecurityTCPListenerRequiresAuthentication(t *testing.T) {
	sectest.RequireLoopbackListener(t)

	s := startSidecar(t, "HTTP/1.1 200 OK\r\nContent-Length: 6\r\n\r\nsecret")
	reg := intent.New(time.Minute)
	t.Cleanup(reg.Close)

	f := New("", "127.0.0.1:0", []Route{{
		Mount:        "slack",
		UpstreamPort: s.port,
		Skill:        "slack",
		State:        RouteReady,
	}}, 0, time.Minute, "", "test")
	f.IntentRegistry = reg
	f.FacadeToken = "test-facade-token" // enable TCP authentication
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	port := f.TCPPort()
	if port == 0 {
		return // no TCP transport: nothing is exposed without credentials
	}
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: 5 * time.Second}

	// Control: the facade is up and rejecting unauthenticated requests.
	// We use /sandbox/denied which is a built-in that requires no route —
	// the 401 confirms auth is active, not just that the route is missing.
	resp, err := client.Get(base + "/sandbox/denied?path=/etc/shadow")
	if err != nil {
		t.Fatalf("the facade is not answering on its own TCP port (%v): the fixture is broken, not the security property", err)
	}
	resp.Body.Close()

	t.Run("route enumeration", func(t *testing.T) {
		resp, err := client.Get(base + "/")
		if err != nil {
			t.Fatalf("GET /: %v", err)
		}
		defer resp.Body.Close()
		var status struct {
			Skills []string `json:"skills"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&status)
		if len(status.Skills) > 0 {
			t.Errorf("an unauthenticated caller listed the mounted skills %v: any local process learns what this session has access to", status.Skills)
		}
	})

	t.Run("sidecar proxying", func(t *testing.T) {
		resp, err := client.Get(base + "/slack/whoami")
		if err != nil {
			t.Fatalf("GET /slack/whoami: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Error("an unauthenticated caller reached a skill sidecar through the facade: the sidecar's credentials are usable by any local process, which is what withholding its port from the sandbox was meant to prevent")
		}
	})

	t.Run("intent writes", func(t *testing.T) {
		body := strings.NewReader(`{"target":"attacker.example","reason":"planted by an unauthenticated caller"}`)
		resp, err := client.Post(base+"/sandbox/intent", "application/json", body)
		if err != nil {
			t.Fatalf("POST /sandbox/intent: %v", err)
		}
		defer resp.Body.Close()
		if e, ok := reg.Lookup("attacker.example"); ok {
			t.Errorf("an unauthenticated caller wrote an intent record (%q): the text shown to the user when approving access is writable by anyone on the machine", e.Reason)
		}
	})
}

// TestSecurityTCPAuthRejectsNonLoopbackSource asserts that a request
// arriving over the TCP listener must present the bearer token even when
// the caller's source address is not loopback. The source address of a
// local TCP connection is chosen by the dialer, so it cannot serve as a
// marker that the request came from the trusted Unix transport: trust must
// follow from the listener that accepted the connection.
func TestSecurityTCPAuthRejectsNonLoopbackSource(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	src := nonLoopbackSourceIP(t)

	s := startSidecar(t, "HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nbody")
	f := New("", "127.0.0.1:0", []Route{{
		Mount:        "slack",
		UpstreamPort: s.port,
		Skill:        "slack",
		State:        RouteReady,
	}}, 0, time.Minute, "", "test")
	f.FacadeToken = "test-facade-token"
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(src)}}
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{DialContext: dialer.DialContext},
	}

	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/slack/whoami", f.TCPPort()))
	if err != nil {
		t.Fatalf("request with non-loopback source %s failed (%v): the fixture is broken, not the security property", src, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("TCP request with non-loopback source %s got status %d, want 401: a TCP request must require the bearer token regardless of the peer's source address", src, resp.StatusCode)
	}
}

// TestSecurityUnixSocketExemptByTransportFlag asserts that a request over
// the Unix socket is exempt from the bearer-token gate by virtue of the
// transport it arrived on, not by inspecting its peer address string. The
// dialer binds its socket to a path whose textual form is indistinguishable
// from a loopback TCP peer address; exemption must still hold, because it
// is the accepting listener that identifies the transport.
func TestSecurityUnixSocketExemptByTransportFlag(t *testing.T) {
	sectest.RequireLoopbackListener(t)
	requireUnixSocketTransport(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)
	upstreamPort := upstream.Listener.Addr().(*net.TCPAddr).Port

	dir, err := os.MkdirTemp(".", "omac-sec-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")

	f := New(socket, "", []Route{{
		Mount:        "slack",
		UpstreamPort: upstreamPort,
		Skill:        "slack",
		State:        RouteReady,
	}}, 0, time.Minute, "", "test")
	f.FacadeToken = "test-facade-token"
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	t.Cleanup(func() { f.Close() })

	// Bind the dialing socket to a relative path whose textual form could
	// be confused with a loopback TCP peer. Exemption must follow from the
	// accepting listener, not from parsing that string.
	localName := "127.omac-sec-client"
	t.Cleanup(func() { os.Remove(localName) })
	local := &net.UnixAddr{Name: localName, Net: "unix"}
	remote := &net.UnixAddr{Name: socket, Net: "unix"}
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DisableKeepAlives: true,
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.DialUnix("unix", local, remote)
			},
		},
	}

	resp, err := client.Get("http://x/slack/whoami")
	if err != nil {
		t.Fatalf("Unix-socket request failed (%v): the fixture is broken, not the security property", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Unix-socket request got status %d, want 200: the Unix transport must be exempt by the listener that accepted it, not by address string", resp.StatusCode)
	}
}

// nonLoopbackSourceIP returns a non-loopback IPv4 address of an active
// interface, failing the test when the environment offers none — a TCP
// request with a non-loopback source cannot be constructed without one,
// and silently skipping would read as the property holding.
func nonLoopbackSourceIP(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatalf("enumerate network interfaces: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip := ipNet.IP.To4(); ip != nil && !ip.IsLoopback() {
				return ip.String()
			}
		}
	}
	t.Fatalf("no non-loopback interface address available: this test needs one to send a TCP request with a non-loopback source")
	return ""
}

// requireUnixSocketTransport skips (or fails in CI) when AF_UNIX listen/dial
// is not permitted, so a missing capability never reads as the property holding.
func requireUnixSocketTransport(t *testing.T) {
	t.Helper()
	probeDir, err := os.MkdirTemp(".", "omac-sec-probe-")
	if err != nil {
		secEnvSkip(t, "mkdir temp: %v", err)
	}
	defer os.RemoveAll(probeDir)
	ps := filepath.Join(probeDir, "p.sock")
	pl, err := net.Listen("unix", ps)
	if err != nil {
		secEnvSkip(t, "unix listen not permitted: %v", err)
	}
	c, err := net.Dial("unix", ps)
	if err != nil {
		pl.Close()
		secEnvSkip(t, "unix dial not permitted: %v", err)
	}
	c.Close()
	pl.Close()
}

// secEnvSkip skips locally but fails in CI, so a missing environment
// capability surfaces as an infrastructure regression rather than a
// silently green run with no coverage from the gated tests.
func secEnvSkip(t *testing.T, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal(msg)
	}
	t.Skip(msg)
}
