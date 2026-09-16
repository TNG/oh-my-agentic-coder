package netproxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sectest"
)

// noProxyDialer routes a specific host directly to a fixed address,
// simulating NO_PROXY bypass without touching loopback-blocked resolved IPs.
type noProxyDialer struct {
	proxyURL   *url.URL
	noProxyFor string // host that bypasses the upstream proxy
	originAddr string // real address to dial for the no-proxy host
}

func (d *noProxyDialer) DialTunnel(ctx context.Context, host string, port int, _ []netip.Addr) (net.Conn, error) {
	if host == d.noProxyFor {
		return net.Dial("tcp", d.originAddr)
	}
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.proxyURL.Host)
}

func (d *noProxyDialer) ChainsHost(host string) bool { return host != d.noProxyFor }
func (d *noProxyDialer) ProxyAuthHeader() string     { return "" }

// TestSecurityUpstreamProxyCredentialNeverReachesOrigin asserts that the
// upstream (corporate) proxy's own Basic credential never leaves the hop
// to that proxy — in particular, it must not reach the ORIGIN server on a
// NO_PROXY match, where the connection goes direct.
//
// server.go attaches Proxy-Authorization based on the dialer's TYPE
// (whether it happens to implement ProxyAuthenticator), not on whether the
// connection actually goes to the upstream proxy. NO_PROXY makes
// DialTunnel return a direct, origin-terminated connection while the
// dialer is still the same authenticated one, so the header is written
// straight to the origin.
func TestSecurityUpstreamProxyCredentialNeverReachesOrigin(t *testing.T) {
	sectest.RequireLoopbackListener(t)

	var originSaw atomic.Value
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originSaw.Store(r.Header.Get("Proxy-Authorization"))
		fmt.Fprint(w, "origin-ok")
	}))
	defer origin.Close()

	var proxyHits atomic.Int32
	pln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pln.Close()
	go func() {
		for {
			c, err := pln.Accept()
			if err != nil {
				return
			}
			proxyHits.Add(1)
			c.Close()
		}
	}()

	originPort := strings.TrimPrefix(origin.URL, "http://127.0.0.1:")
	proxyURL, err := url.Parse(fmt.Sprintf("http://corpuser:hunter2@127.0.0.1:%d", pln.Addr().(*net.TCPAddr).Port))
	if err != nil {
		t.Fatal(err)
	}
	// A dialer that knows origin.test bypasses the upstream proxy (ChainsHost
	// returns false) and routes directly to the real origin server.
	// Resolving to a documentation-range IP keeps Filter.Check happy.
	originAddr := origin.Listener.Addr().String()
	dialer := &noProxyDialer{
		proxyURL:   proxyURL,
		noProxyFor: "origin.test",
		originAddr: originAddr,
	}

	filter := NewFilter(FilterConfig{Resolve: resolveTo("192.0.2.1")})
	s, err := NewServer(filter, dialer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	client := proxyClient(s)
	resp, err := client.Get("http://origin.test:" + originPort + "/")
	if err != nil {
		t.Fatalf("GET via proxy: %v", err)
	}
	resp.Body.Close()

	// Control: the request actually went direct (NO_PROXY matched, the
	// upstream proxy was never contacted), so a failure below is about the
	// credential reaching the origin, not about the routing itself.
	if proxyHits.Load() != 0 {
		t.Fatalf("control: the request was routed through the upstream proxy (%d hits): the fixture is broken, not the security property", proxyHits.Load())
	}

	saw, _ := originSaw.Load().(string)
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("corpuser:hunter2"))
	if saw == want {
		t.Errorf("the origin server received the upstream proxy's Proxy-Authorization credential over a direct (NO_PROXY) connection: %q", saw)
	}
}
