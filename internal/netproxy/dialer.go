package netproxy

import (
	"bufio"
	"context"
	"crypto/tls"
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
// (anti-DNS-rebinding); the upstream-proxy implementation passes the
// hostname to the corporate proxy for its own DNS, except on a NO_PROXY
// match where it dials the pinned IPs directly. When a resolver is wired
// (SetResolver), the upstream-proxy dialer also re-resolves and
// re-validates the hostname against the hard-deny ranges immediately
// before issuing CONNECT, closing the TOCTOU gap between admission and
// the upstream's own DNS.
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
type directDialer struct {
	allowLoopback bool // test seam: skip pre-dial loopback check
}

// NewDirectDialer creates a Dialer that dials the server-pinned IPs
// directly (anti-DNS-rebinding preserved by reusing the server's addrs).
func NewDirectDialer() Dialer {
	return &directDialer{}
}

// NewDirectDialerAllowLoopback is a test-only constructor that skips the
// pre-dial loopback check, for tests that route traffic through a local
// origin server on 127.0.0.1.
func NewDirectDialerAllowLoopback() Dialer {
	return &directDialer{allowLoopback: true}
}

func (d *directDialer) DialTunnel(ctx context.Context, host string, port int, addrs []netip.Addr) (net.Conn, error) {
	return dialPinned(ctx, addrs, port, d.allowLoopback)
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

// ForbiddenAddressError is returned by DialTunnel when a pre-CONNECT
// re-resolution of the hostname lands in a hard-denied address range. The
// server maps it to a 403 denial (not a 502 upstream error) so the agent
// sees the same "hard-deny" body it would for any other forbidden
// destination.
type ForbiddenAddressError struct {
	Host   string
	Addr   netip.Addr
	Reason string // "hard-deny ..." reason from hardDeniedAddr
}

func (e *ForbiddenAddressError) Error() string {
	return fmt.Sprintf("pre-CONNECT re-validation denied %s: resolves to %s (%s)", e.Host, e.Addr, e.Reason)
}

// upstreamProxyDialer tunnels connections through an upstream corporate
// proxy via HTTP CONNECT. It is used when the sandbox itself sits behind
// a corporate egress proxy and cannot dial the destination directly.
type upstreamProxyDialer struct {
	proxyURL   *url.URL
	proxyAuth  string   // "Basic <base64>" or ""
	noProxy    []string // host suffixes that bypass the upstream proxy
	direct     Dialer   // fallback for NO_PROXY matches (wraps the filter)
	logf       func(string, ...any)
	resolve    func(context.Context, string) ([]netip.Addr, error) // nil = no pre-CONNECT re-validation
	revalidate bool                                                // gate re-validation on ResolveOnCheckHost
}

// NewUpstreamProxyDialer creates a Dialer that tunnels through the
// corporate proxy at proxyURL. Hosts matching any entry in noProxy
// (suffix match on the hostname) bypass the upstream proxy and dial the
// server-pinned IPs directly instead.
func NewUpstreamProxyDialer(proxyURL *url.URL, noProxy []string, logf func(string, ...any)) Dialer {
	return newUpstreamProxyDialerInternal(proxyURL, noProxy, logf, false)
}

func newUpstreamProxyDialerAllowLoopback(proxyURL *url.URL, noProxy []string, logf func(string, ...any)) Dialer {
	return newUpstreamProxyDialerInternal(proxyURL, noProxy, logf, true)
}

func newUpstreamProxyDialerInternal(proxyURL *url.URL, noProxy []string, logf func(string, ...any), allowLoopback bool) Dialer {
	var proxyAuth string
	if proxyURL.User != nil {
		username := proxyURL.User.Username()
		password, _ := proxyURL.User.Password()
		creds := username + ":" + password
		proxyAuth = "Basic " + base64.StdEncoding.EncodeToString([]byte(creds))
	}
	var direct Dialer
	if allowLoopback {
		direct = NewDirectDialerAllowLoopback()
	} else {
		direct = NewDirectDialer()
	}
	return &upstreamProxyDialer{
		proxyURL:  proxyURL,
		proxyAuth: proxyAuth,
		noProxy:   noProxy,
		direct:    direct,
		logf:      logf,
	}
}

// SetResolver wires the DNS resolver and re-validation flag for the
// chained (upstream-proxy) path. When revalidate is true and resolve is
// non-nil, DialTunnel re-resolves the hostname immediately before issuing
// CONNECT and aborts if any resolved address is in a hard-denied range,
// closing the TOCTOU gap between admission and the upstream's own DNS.
// When resolve is nil or revalidate is false, the chained path forwards
// the hostname as-is (the upstream proxy does its own DNS). On resolver
// failure the re-validation is skipped: a corporate-internal hostname
// that only resolves behind the proxy must still be admitted.
func (d *upstreamProxyDialer) SetResolver(resolve func(context.Context, string) ([]netip.Addr, error), revalidate bool) {
	d.resolve = resolve
	d.revalidate = revalidate
}

// hostMatchesNoProxy reports whether host matches any entry in noProxy
// using simple hostname suffix matching: host == entry or host ends in
// "."+entry. CIDR ranges are not supported in this simple version.
func hostMatchesNoProxy(host string, entries []string) bool {
	h := NormalizeHost(host)
	for _, e := range entries {
		entry := NormalizeHost(strings.TrimSpace(e))
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
	host = NormalizeHost(host) // send canonical form; upstream must not see trailing dots
	if hostMatchesNoProxy(host, d.noProxy) {
		d.logf("omac netproxy: NO_PROXY match for %s — dialing direct", host)
		return d.direct.DialTunnel(ctx, host, port, addrs)
	}

	// Pre-CONNECT re-validation: close the TOCTOU gap between the
	// admission-time CheckHost (which resolved the hostname) and the
	// upstream proxy's own DNS lookup. If DNS has flipped the answer into
	// a hard-denied range since admission, abort before sending CONNECT.
	// Skipped when no resolver is wired (diagnose --probe) or admission
	// did not resolve (ResolveOnCheckHost=false). A resolver failure is
	// not fatal: a corporate-internal hostname may only resolve behind
	// the upstream proxy, so we fall through and let the proxy resolve it.
	if d.revalidate && d.resolve != nil {
		if ip, err := netip.ParseAddr(host); err == nil {
			if reason, denied := hardDeniedAddr(ip); denied {
				return nil, &ForbiddenAddressError{Host: host, Addr: ip, Reason: reason}
			}
		} else if addrs, rerr := d.resolve(ctx, host); rerr == nil {
			for _, a := range addrs {
				if reason, denied := hardDeniedAddr(a); denied {
					return nil, &ForbiddenAddressError{Host: host, Addr: a, Reason: reason}
				}
			}
		}
	}

	target := net.JoinHostPort(host, fmt.Sprintf("%d", port))

	var nd net.Dialer
	rawConn, err := nd.DialContext(ctx, "tcp", d.proxyURL.Host)
	if err != nil {
		return nil, &UpstreamError{
			ProxyHost:  d.proxyURL.Host,
			StatusLine: "",
			Err:        err,
		}
	}
	var conn net.Conn
	if d.proxyURL.Scheme == "https" {
		tlsConn := tls.Client(rawConn, &tls.Config{
			ServerName: d.proxyURL.Hostname(),
			MinVersion: tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			rawConn.Close()
			return nil, &UpstreamError{
				ProxyHost:  d.proxyURL.Host,
				StatusLine: "",
				Err:        fmt.Errorf("TLS handshake: %w", err),
			}
		}
		conn = tlsConn
	} else {
		conn = rawConn
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
	resp.Body.Close()
	if br.Buffered() > 0 {
		return &bufferedConn{Conn: conn, br: br}, nil
	}
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
