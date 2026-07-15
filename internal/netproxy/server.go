package netproxy

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

// connectTimeout bounds the upstream dial (and the early DNS resolve).
// It is a var, not a const, only so tests can shorten it. The
// interactive prompt is bounded separately by prompt_timeout_secs and
// must never share this deadline — see checkAndDial.
var connectTimeout = 30 * time.Second

// sandboxDenyHeader marks deny responses as originating from the omac
// sandbox (not the destination server), so both humans and agents can
// tell a policy denial from a real upstream 403.
const sandboxDenyHeader = "X-Omac-Sandbox: denied\r\n"

// denyBody renders the body for a filtered denial. It explicitly
// attributes the denial to the sandbox network policy and points at
// the knobs that change it. When reason indicates the user clicked
// "Explain more", the body directs the agent to declare or refine an
// intent via POST /sandbox/intent and retry.
func denyBody(host, reason string) string {
	if strings.Contains(reason, "needs_intent") {
		return fmt.Sprintf(`omac sandbox: access to %q was DENIED — the user asked for more explanation.

Declare or refine your intent via:
  POST $OMAC_BASE/sandbox/intent  {"target":%q,"reason":"..."}
then retry the request.
`, host, host)
	}
	return fmt.Sprintf(`omac sandbox: access to %q was DENIED BY THE SANDBOX network policy (%s).

This response comes from the omac sandbox proxy, not from %s.
The destination was never contacted.

To allow this host, either:
  - answer "Allow" in the network prompt (if enabled),
  - add it to network.allow_domain in your sandbox profile
    (~/.config/omac/sandbox-profiles/<profile>.json),
  - or remove a matching deny entry from network.deny_domain or the
    <profile>.pages.json learned-policy file.
`, host, reason, host)
}

// Server is the filtering proxy. It binds 127.0.0.1:0 and serves
// CONNECT tunnels (HTTPS) and absolute-URI forwarding (plain HTTP).
type Server struct {
	filter *Filter
	token  string
	ln     net.Listener
	logf   func(format string, args ...any)

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

// NewServer creates a proxy with a fresh 256-bit session token.
func NewServer(filter *Filter, logf func(string, ...any)) (*Server, error) {
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return nil, fmt.Errorf("generate proxy token: %w", err)
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Server{
		filter: filter,
		token:  hex.EncodeToString(tok),
		logf:   logf,
		conns:  map[net.Conn]struct{}{},
	}, nil
}

// Start binds the loopback listener and serves in a goroutine.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("bind proxy listener: %w", err)
	}
	s.ln = ln
	go s.acceptLoop()
	return nil
}

// Port returns the bound port (after Start).
func (s *Server) Port() int {
	if s.ln == nil {
		return 0
	}
	return s.ln.Addr().(*net.TCPAddr).Port
}

// Token returns the session token.
func (s *Server) Token() string { return s.token }

// ProxyURL is the value for HTTP_PROXY/HTTPS_PROXY: the token rides in
// the userinfo so standard clients send Proxy-Authorization.
func (s *Server) ProxyURL() string {
	return fmt.Sprintf("http://omac:%s@127.0.0.1:%d", s.token, s.Port())
}

// EnvVars returns the proxy environment for the sandboxed child.
func (s *Server) EnvVars() map[string]string {
	u := s.ProxyURL()
	return map[string]string{
		"HTTP_PROXY":  u,
		"HTTPS_PROXY": u,
		"http_proxy":  u,
		"https_proxy": u,
		"NO_PROXY":    "localhost,127.0.0.1,::1",
		"no_proxy":    "localhost,127.0.0.1,::1",
	}
}

// Close stops the listener and tears down active tunnels.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	if s.ln != nil {
		_ = s.ln.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed
		}
		s.track(conn, true)
		go func() {
			defer s.track(conn, false)
			defer conn.Close()
			s.handle(conn)
		}()
	}
}

func (s *Server) track(c net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		if s.closed {
			_ = c.Close()
			return
		}
		s.conns[c] = struct{}{}
		return
	}
	delete(s.conns, c)
}

// handle reads one request head and dispatches CONNECT vs forward.
func (s *Server) handle(conn net.Conn) {
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	if !s.authorized(req) {
		s.logf("omac sandbox: net DENY %s (missing/invalid proxy token)", req.Host)
		writeRawResponse(conn, http.StatusProxyAuthRequired, "Proxy-Authenticate: Basic realm=\"omac\"\r\n",
			"proxy authentication required\n")
		return
	}
	if req.Method == http.MethodConnect {
		s.handleConnect(conn, req)
		return
	}
	s.handleForward(conn, br, req)
}

// authorized validates the session token from Proxy-Authorization
// (Basic user:token) constant-time.
func (s *Server) authorized(req *http.Request) bool {
	h := req.Header.Get("Proxy-Authorization")
	if h == "" {
		return false
	}
	parts := strings.SplitN(h, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Basic") {
		return false
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(parts[1]))
	if err != nil {
		return false
	}
	creds := string(dec)
	colon := strings.IndexByte(creds, ':')
	if colon < 0 {
		return false
	}
	pass := creds[colon+1:]
	if len(pass) != len(s.token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(pass), []byte(s.token)) == 1
}

// handleConnect filters and splices a raw TCP tunnel. TLS bytes pass
// through untouched.
func (s *Server) handleConnect(conn net.Conn, req *http.Request) {
	host, port, err := splitConnectTarget(req.Host)
	if err != nil {
		writeRawResponse(conn, http.StatusBadRequest, "", "malformed CONNECT target\n")
		return
	}
	// Loopback CONNECTs are refused: in-sandbox loopback traffic goes
	// direct via open_port, not through the proxy.
	if isLoopbackHost(host) {
		writeRawResponse(conn, http.StatusForbidden, sandboxDenyHeader,
			fmt.Sprintf("omac sandbox: CONNECT to loopback %q refused by the sandbox (loopback traffic must use a granted open_port, not the proxy)\n", host))
		return
	}
	upstream, verdict, err := s.checkAndDial(host, port)
	if verdict.Decision != Allow {
		writeRawResponse(conn, http.StatusForbidden, sandboxDenyHeader, denyBody(host, verdict.Reason))
		return
	}
	if err != nil {
		writeRawResponse(conn, http.StatusBadGateway, "", fmt.Sprintf("upstream dial failed: %v\n", err))
		return
	}
	defer upstream.Close()
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	splice(conn, upstream)
}

// checkAndDial runs the filter — which may block on the interactive
// prompt for up to prompt_timeout_secs — and, on Allow, dials the
// pinned upstream on a *fresh* connectTimeout deadline. The two phases
// use independent deadlines on purpose: a slow human approval must not
// spend the dial budget, or a granted host would fail on an
// already-expired context. verdict is always meaningful; upstream/err
// are only set when the verdict is Allow.
func (s *Server) checkAndDial(host string, port int) (net.Conn, Verdict, error) {
	// The check phase runs in its own scope so checkCancel is deferred
	// (fires even if filter.Check panics) yet still releases the check
	// context before the dial phase begins.
	verdict, addrs := func() (Verdict, []netip.Addr) {
		checkCtx, checkCancel := context.WithTimeout(context.Background(), connectTimeout)
		defer checkCancel()
		return s.filter.Check(checkCtx, host, port)
	}()
	if verdict.Decision != Allow {
		return nil, verdict, nil
	}
	dialCtx, dialCancel := context.WithTimeout(context.Background(), connectTimeout)
	defer dialCancel()
	upstream, err := dialPinned(dialCtx, addrs, port)
	return upstream, verdict, err
}

// handleForward proxies a plain-HTTP absolute-URI request, streaming
// the response (SSE-safe: no buffering beyond the kernel socket).
func (s *Server) handleForward(conn net.Conn, br *bufio.Reader, req *http.Request) {
	if !req.URL.IsAbs() {
		writeRawResponse(conn, http.StatusBadRequest, "", "absolute-URI required on a proxy\n")
		return
	}
	if !strings.EqualFold(req.URL.Scheme, "http") {
		writeRawResponse(conn, http.StatusBadRequest, "", "only http:// forwarding is supported (use CONNECT for TLS)\n")
		return
	}
	host := req.URL.Hostname()
	port := 80
	if p := req.URL.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	}
	if isLoopbackHost(host) {
		writeRawResponse(conn, http.StatusForbidden, sandboxDenyHeader,
			fmt.Sprintf("omac sandbox: forward to loopback %q refused by the sandbox (loopback traffic must use a granted open_port, not the proxy)\n", host))
		return
	}
	upstream, verdict, err := s.checkAndDial(host, port)
	if verdict.Decision != Allow {
		writeRawResponse(conn, http.StatusForbidden, sandboxDenyHeader, denyBody(host, verdict.Reason))
		return
	}
	if err != nil {
		writeRawResponse(conn, http.StatusBadGateway, "", fmt.Sprintf("upstream dial failed: %v\n", err))
		return
	}
	defer upstream.Close()

	// Rewrite to origin-form and strip hop-by-hop proxy headers.
	req.Header.Del("Proxy-Authorization")
	req.Header.Del("Proxy-Connection")
	req.RequestURI = ""
	outReq := req.Clone(context.Background())
	outReq.URL.Scheme = ""
	outReq.URL.Host = ""
	if err := outReq.Write(upstream); err != nil {
		return
	}
	// Stream the raw response bytes back; the client parses them. This
	// preserves SSE/chunked semantics without re-buffering.
	// Any leftover bytes the client already pipelined are forwarded too.
	splice(conn, upstream)
	_ = br // request body (if any) was consumed by outReq.Write via req.Body
}

// maxParallelDials caps how many pinned addresses are dialed at once.
// RFC 8305 recommends racing only a small number of connection attempts
// concurrently rather than an unbounded fan-out.
const maxParallelDials = 8

// dialPinned connects to the already-resolved addresses, racing them
// concurrently (Happy Eyeballs, RFC 8305) rather than in strict order.
// Only the pre-resolved IPs are dialed, so DNS pinning (anti-rebinding)
// is preserved — but a dead address family (e.g. an AAAA that resolves
// yet has no route) can no longer stall the whole connection ahead of a
// working address. The first successful connection wins; the losing
// dials are cancelled and any that still connected are closed.
func dialPinned(ctx context.Context, addrs []netip.Addr, port int) (net.Conn, error) {
	if len(addrs) == 0 {
		return nil, fmt.Errorf("no addresses")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, len(addrs))
	// Bound concurrent dials (RFC 8305 recommends racing only a small
	// number). All addresses are still attempted; at most maxParallelDials
	// are in flight at once. Goroutines waiting for a slot bail as soon as
	// a winner cancels ctx.
	sem := make(chan struct{}, maxParallelDials)
	for _, a := range addrs {
		addr := net.JoinHostPort(a.String(), strconv.Itoa(port))
		go func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				ch <- result{nil, ctx.Err()}
				return
			}
			var d net.Dialer
			conn, err := d.DialContext(ctx, "tcp", addr)
			ch <- result{conn, err}
		}()
	}
	var winner net.Conn
	var lastErr error
	for range addrs {
		r := <-ch
		switch {
		case r.err != nil:
			lastErr = r.err
		case winner == nil:
			winner = r.conn
			cancel() // let the remaining in-flight dials bail out fast
		default:
			_ = r.conn.Close() // connected but lost the race
		}
	}
	if winner != nil {
		return winner, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no addresses")
	}
	return nil, lastErr
}

// splice copies bytes in both directions until either side closes.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); halfClose(a); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); halfClose(b); done <- struct{}{} }()
	<-done
	<-done
}

func halfClose(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
		return
	}
	_ = c.Close()
}

func splitConnectTarget(target string) (string, int, error) {
	host, portStr, err := net.SplitHostPort(target)
	if err != nil {
		return "", 0, err
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("invalid port %q", portStr)
	}
	return host, port, nil
}

func isLoopbackHost(host string) bool {
	h := strings.ToLower(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip, err := netip.ParseAddr(h); err == nil {
		return IsLoopback(ip)
	}
	return false
}

// writeRawResponse emits a minimal HTTP/1.1 response on a raw conn.
func writeRawResponse(conn net.Conn, status int, extraHeaders, body string) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n%sContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), extraHeaders, len(body), body)
}
