package netproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// Dialer establishes a tunnel connection to host:port. It is a pure
// transport: admission control and DNS resolution are performed once by
// the server's Filter.Check, whose pinned, approved addresses are passed
// in as addrs. The direct implementation dials those pinned IPs
// (anti-DNS-rebinding); the upstream-proxy implementation ignores them
// and passes the hostname to the corporate proxy for its own DNS, except
// on a NO_PROXY match where it dials the pinned IPs directly.
type Dialer interface {
	DialTunnel(ctx context.Context, host string, port int, addrs []netip.Addr) (net.Conn, error)
}

// TunnelPlanner reports whether a host will be tunneled through an
// upstream proxy (which does its own DNS) rather than dialed directly.
// When ChainsHost is true the server admits the host WITHOUT local DNS
// resolution: the upstream proxy resolves it and the hostname is the
// admission boundary. Only the upstream-proxy dialer implements it;
// direct dialers do not (they always need pinned IPs to dial).
type TunnelPlanner interface {
	ChainsHost(host string) bool
}

// ProxyAuthenticator returns the Proxy-Authorization header value
// for an upstream proxy, or "" if none. Only upstream-proxy dialers
// implement it; direct dialers do not. The handler uses it to set
// the upstream's credentials on forwarded plain-HTTP requests (after
// stripping the child's omac session token) and to decide between
// absolute-URI (upstream proxy) and origin-form (direct) forwarding.
type ProxyAuthenticator interface {
	ProxyAuthHeader() string
}

// directDialer dials the pinned addresses the server already resolved
// and approved via Filter.Check. It performs no admission control or DNS
// resolution of its own — that is the server's single responsibility.
type directDialer struct{}

// NewDirectDialer creates a Dialer that dials the server-pinned IPs
// directly (anti-DNS-rebinding preserved by reusing the server's addrs).
func NewDirectDialer() Dialer {
	return &directDialer{}
}

func (d *directDialer) DialTunnel(ctx context.Context, host string, port int, addrs []netip.Addr) (net.Conn, error) {
	return dialPinned(ctx, addrs, port)
}

// UpstreamError carries attribution for a failed upstream-proxy
// tunnel attempt. It never contains credentials.
type UpstreamError struct {
	ProxyHost  string // host:port of the upstream proxy (no userinfo)
	StatusLine string // e.g. "HTTP/1.1 407 Proxy Authentication Required" (empty on dial failure)
	Err        error  // underlying error (nil for non-200 responses)
}

func (e *UpstreamError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("upstream proxy %s: %v", e.ProxyHost, e.Err)
	}
	return fmt.Sprintf("upstream proxy %s rejected tunnel: %s", e.ProxyHost, e.StatusLine)
}

// upstreamProxyDialer tunnels connections through an upstream corporate
// proxy via HTTP CONNECT. It is used when the sandbox itself sits behind
// a corporate egress proxy and cannot dial the destination directly.
type upstreamProxyDialer struct {
	proxyURL  *url.URL
	proxyAuth string   // "Basic <base64>" or ""
	noProxy   []string // host suffixes that bypass the upstream proxy
	direct    Dialer   // fallback for NO_PROXY matches (wraps the filter)
	logf      func(string, ...any)
}

// NewUpstreamProxyDialer creates a Dialer that tunnels through the
// corporate proxy at proxyURL. Hosts matching any entry in noProxy
// (suffix match on the hostname) bypass the upstream proxy and dial the
// server-pinned IPs directly instead.
func NewUpstreamProxyDialer(proxyURL *url.URL, noProxy []string, logf func(string, ...any)) Dialer {
	var proxyAuth string
	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		creds := username + ":" + password
		proxyAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
	}
	return &upstreamProxyDialer{
		proxyURL:  proxyURL,
		proxyAuth: proxyAuth,
		noProxy:   noProxy,
		direct:    NewDirectDialer(),
		logf:      logf,
	}
}

// hostMatchesNoProxy reports whether host matches any entry in noProxy
// using simple hostname suffix matching: host == entry or host ends in
// "."+entry. CIDR ranges are not supported in this simple version.
func hostMatchesNoProxy(host string, entries []string) bool {
	h := strings.ToLower(host)
	for _, e := range entries {
		entry := strings.ToLower(strings.TrimSpace(e))
		if entry == "" {
			continue
		}
		if h == entry {
			return true
		}
		if strings.HasSuffix(h, "."+entry) {
			return true
		}
	}
	return false
}

// ProxyAuthHeader returns the upstream proxy's Proxy-Authorization
// header value ("Basic <base64>" or "") so the forward handler can set
// it on plain-HTTP requests tunneled through the upstream proxy.
func (d *upstreamProxyDialer) ProxyAuthHeader() string { return d.proxyAuth }

// ChainsHost reports whether host will be tunneled through the upstream
// proxy (true) or bypassed via NO_PROXY and dialed directly (false).
func (d *upstreamProxyDialer) ChainsHost(host string) bool {
	return !hostMatchesNoProxy(host, d.noProxy)
}

func (d *upstreamProxyDialer) DialTunnel(ctx context.Context, host string, port int, addrs []netip.Addr) (net.Conn, error) {
	if hostMatchesNoProxy(host, d.noProxy) {
		d.logf("omac netproxy: NO_PROXY match for %s — dialing direct", host)
		return d.direct.DialTunnel(ctx, host, port, addrs)
	}

	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "tcp", d.proxyURL.Host)
	if err != nil {
		return nil, &UpstreamError{
			ProxyHost:  d.proxyURL.Host,
			StatusLine: "",
			Err:        err,
		}
	}

	// Pass the hostname (not pre-resolved IPs): corporate proxies perform
	// their own DNS resolution.
	var req strings.Builder
	req.WriteString("CONNECT " + target + " HTTP/1.1\r\n")
	req.WriteString("Host: " + target + "\r\n")
	if d.proxyAuth != "" {
		req.WriteString("Proxy-Authorization: " + d.proxyAuth + "\r\n")
	}
	req.WriteString("\r\n")
	if _, err := conn.Write([]byte(req.String())); err != nil {
		conn.Close()
		return nil, &UpstreamError{
			ProxyHost:  d.proxyURL.Host,
			StatusLine: "",
			Err:        err,
		}
	}

	// http.ReadResponse tolerates extra headers corporate proxies send
	// (Proxy-Authenticate, Via, X-Squid-Error, ...).
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		conn.Close()
		return nil, &UpstreamError{
			ProxyHost:  d.proxyURL.Host,
			StatusLine: "",
			Err:        err,
		}
	}

	if resp.StatusCode != http.StatusOK {
		statusLine := resp.Status
		resp.Body.Close()
		conn.Close()
		return nil, &UpstreamError{
			ProxyHost:  d.proxyURL.Host,
			StatusLine: statusLine,
			Err:        nil,
		}
	}

	// On 200 the conn is a raw tunnel. Preserve any bytes the
	// bufio.Reader already pulled past the response head so the client
	// sees them before reading directly from the socket.
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, br: br}, nil
	}
	resp.Body.Close()
	return conn, nil
}

// bufferedConn prepends bytes the bufio.Reader already pulled past the
// proxy's response head before forwarding raw socket bytes.
type bufferedConn struct {
	net.Conn
	br *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.br != nil {
		n, err := c.br.Read(p)
		if err == nil && n > 0 {
			return n, nil
		}
		c.br = nil
	}
	return c.Conn.Read(p)
}
