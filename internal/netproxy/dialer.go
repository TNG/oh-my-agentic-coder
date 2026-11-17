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
// the server's Filter.Check/checkHostPinned, whose pinned, approved
// addresses are passed in as addrs. The direct implementation dials those
// pinned IPs (anti-DNS-rebinding); the upstream-proxy implementation
// CONNECTs to the pinned IP literal when addrs is non-empty so the
// upstream proxy connects to the exact address that was checked, and only
// falls back to the hostname when no addresses were pinned. On a NO_PROXY
// match it dials the pinned IPs directly.
type Dialer interface {
	DialTunnel(ctx context.Context, host string, port int, addrs []netip.Addr) (net.Conn, error)
}

// TunnelPlanner reports whether a host will be tunneled through an
// upstream proxy rather than dialed directly. When ChainsHost is true the
// server admits the host via checkHostPinned: it resolves locally when
// ResolveOnCheckHost is set (so the resolved IPs can be pinned and
// CONNECTed to as literals) and otherwise admits on the hostname alone.
// Only the upstream-proxy dialer implements it; direct dialers do not
// (they always need pinned IPs to dial).
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

// upstreamProxyDialer tunnels connections through an upstream corporate
// proxy via HTTP CONNECT. It is used when the sandbox itself sits behind
// a corporate egress proxy and cannot dial the destination directly.
type upstreamProxyDialer struct {
	proxyURL  *url.URL
	proxyAuth string   // "Basic <base64>" or ""
	noProxy   []string // NO_PROXY entries (see hostMatchesNoProxy) that bypass the upstream proxy
	direct    Dialer   // fallback for NO_PROXY matches (wraps the filter)
	logf      func(string, ...any)
}

// NewUpstreamProxyDialer creates a Dialer that tunnels through the
// corporate proxy at proxyURL. Hosts matching any entry in noProxy
// (a domain, ".domain", "*.domain" or "*"; see hostMatchesNoProxy) bypass
// the upstream proxy and dial the server-pinned IPs directly instead.
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

// hostMatchesNoProxy reports whether host matches any entry in noProxy.
// All domain forms are equivalent: "example.com", ".example.com", and
// "*.example.com" each match the domain itself and any subdomain, and
// "*" matches every host. An IP address entry matches that literal host.
// IP ranges, ports and "<local>" are not supported: matching happens on
// the unresolved hostname, so an address-based entry could not be
// evaluated here. NoProxyEntryIgnoreReason flags such entries.
func hostMatchesNoProxy(host string, entries []string) bool {
	h := NormalizeHost(host)
	for _, e := range entries {
		entry := strings.TrimSpace(e)
		// Check "*" before normalizing: NormalizeHost trims trailing
		// dots, which would turn the malformed entry "*." into a
		// match-all "*".
		if entry == "*" {
			return true
		}
		entry = normalizeNoProxyEntry(entry)
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

// normalizeNoProxyEntry strips the ".domain" / "*.domain" prefix and
// normalizes the rest like a host, so that all domain forms compare
// equal. The input must already be trimmed and must not be "*".
func normalizeNoProxyEntry(entry string) string {
	entry = strings.TrimPrefix(entry, "*.")
	entry = strings.TrimPrefix(entry, ".")
	return NormalizeHost(entry)
}

// NoProxyEntryIgnoreReason reports why a NO_PROXY entry can never match
// any host in hostMatchesNoProxy, or "" if the entry is usable (or
// empty: empty entries are skipped and handled by the callers).
// Hosts reach the matcher as a bare hostname or IP address, never with
// a port or brackets. Examples:
//
//	"10.0.0.0/8"       -> IP ranges are not supported
//	"example.com:8443" -> ports are not supported
//	".", "*."          -> no domain left after the prefix
//	"<local>", "[::1]" -> not a host name or IP address
//	"::1", "10.1.2.3"  -> "" (matches that literal address)
func NoProxyEntryIgnoreReason(entry string) string {
	entry = strings.TrimSpace(entry)
	if entry == "" || entry == "*" {
		return ""
	}
	if strings.Contains(entry, "/") {
		return "IP ranges are not supported, omac matches on the host name before resolving it"
	}
	if _, port, err := net.SplitHostPort(entry); err == nil && port != "" {
		return "entries with a port are not supported"
	}
	d := normalizeNoProxyEntry(entry)
	if d == "" {
		return "it names no domain"
	}
	if _, err := netip.ParseAddr(d); err == nil {
		return ""
	}
	for _, c := range d {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c >= 0x80) {
			return "it is not a host name or IP address"
		}
	}
	return ""
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

	// When admission pinned resolved addresses, CONNECT to the literal IP so
	// the upstream proxy connects to the exact address that was checked. A
	// hostname target would let the upstream proxy's own resolve diverge from
	// the admission-time answer. Without pinned addresses (e.g. an
	// internal-only host admitted by an allow rule without ResolveOnCheckHost)
	// the hostname is the only target available, so the upstream proxy resolves
	// it as before.
	targetHost := host
	if len(addrs) > 0 {
		targetHost = addrs[0].String()
	}
	target := net.JoinHostPort(targetHost, fmt.Sprintf("%d", port))
	// CONNECT uses URI authority syntax: escape the zone without changing
	// the admission-pinned IPv6 address or outgoing interface.
	target = strings.TrimPrefix((&url.URL{Host: target}).String(), "//")

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

	// CONNECT to the admission-pinned address when available (see targetHost
	// above); otherwise fall back to the hostname and let the upstream proxy
	// resolve it.
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
