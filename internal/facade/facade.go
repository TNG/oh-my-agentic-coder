// Package facade implements the Unix-socket HTTP reverse proxy.
//
// Routing: requests whose path is /<mount>/<rest> are forwarded to
//
//	http://127.0.0.1:<port>/<rest>
//
// Streaming is handled by wrapping http.ResponseWriter with an
// immediate-flush writer: chunked responses and text/event-stream
// pass through without buffering.
//
// Upgrades (WebSocket) are handled via hijacking: after proxying the
// handshake, raw TCP bytes are spliced bidirectionally.
package facade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RouteState describes whether a route forwards to a live sidecar or
// serves a stub response. See the serve-mode design (docs/MULTI_DIR_DESKTOP.md
// §5.2): a registered skill whose required secrets/config are missing is
// mounted anyway, but as a stub that returns a structured 409 until the
// values are supplied; a skill that is broken (bad omac.yaml / bundle drift)
// is mounted as a 502 stub with diagnostics.
type RouteState string

const (
	// RouteReady forwards to UpstreamPort.
	RouteReady RouteState = "ready"
	// RoutePendingCredentials returns 409 with X-Omac-Reason: pending-credentials.
	RoutePendingCredentials RouteState = "pending-credentials"
	// RouteBroken returns 502 with X-Omac-Reason: skill-broken.
	RouteBroken RouteState = "broken"
)

// Route maps a mount prefix to an upstream localhost port.
//
// In single-workdir (omac start) mode the mount is a single segment
// (e.g. "slack") and the request path is /<mount>/<rest>. In serve mode
// (omac serve) the mount is namespaced by a directory token or the
// reserved literal "__global__", and the request path carries that as a
// first segment: /<dirtoken>/<mount>/<rest> or /__global__/<mount>/<rest>.
// The Namespace field, when non-empty, is that first segment; the routing
// key registered in the facade is then "<Namespace>/<Mount>".
type Route struct {
	Mount        string // e.g. "slack"
	Namespace    string // "" (flat) or a dir token or "__global__" (serve mode)
	UpstreamPort int
	MaxBodyBytes int64         // 0 = inherit facade default
	IdleTimeout  time.Duration // 0 = inherit facade default
	Skill        string        // registry name

	// SkillDir is the skill's on-disk directory (the dir holding its
	// omac.yaml and, when present, SKILL.md). It is the source for the
	// auto-discovery response served at the skill's top-level URL
	// (GET /<ns>/<mount>/ with no further path): the bridge reads
	// <SkillDir>/SKILL.md and returns it verbatim, so callers who probe
	// the root learn what the skill exposes without the skill needing to
	// implement a discovery endpoint itself. Empty disables this.
	SkillDir string

	// State selects forward vs. stub behavior. The zero value ("") is
	// treated as RouteReady so existing callers that don't set it keep
	// working unchanged.
	State RouteState
	// Detail carries human-readable diagnostics for stub routes (e.g. the
	// missing secret/config names and the fix commands). Ignored when
	// State is RouteReady.
	Detail string
}

// GlobalNamespace is the reserved, non-mintable first-segment token under
// which user-global (shared) skills are routed in serve mode. A directory
// token can never equal this value.
const GlobalNamespace = "__global__"

// key returns the routing-table key for a route: "<namespace>/<mount>"
// when namespaced, else just "<mount>".
func (r Route) key() string { return routeKey(r.Namespace, r.Mount) }

func routeKey(namespace, mount string) string {
	if namespace == "" {
		return mount
	}
	return namespace + "/" + mount
}

// Facade is an HTTP reverse proxy that simultaneously serves on a Unix
// domain socket AND a 127.0.0.1 TCP port. Both listeners share the same
// handler, so clients can pick whichever transport their environment
// permits.
//
// Why both:
//
//   - Unix socket: lower overhead, file-permission-gated; works in
//     unsandboxed runs and in nono on Linux (where AF_UNIX is purely
//     filesystem-governed).
//   - TCP loopback: required on macOS when nono runs in proxy mode
//     (auto-activated by `custom_credentials`, `--network-profile`,
//     etc.). Proxy mode installs `(deny network*)` in Seatbelt, which
//     classifies AF_UNIX `connect(2)` as `network-outbound` and blocks
//     it. There is no documented way to override that for a single
//     Unix-socket path. `--open-port` whitelists a TCP port instead.
type Facade struct {
	SocketPath    string // Unix socket path; "" disables Unix listener.
	TCPAddr       string // bind addr for TCP listener (e.g. "127.0.0.1:0"); "" disables TCP.
	Routes        []Route
	MaxBodyBytes  int64
	IdleTimeout   time.Duration
	AccessLogPath string
	Version       string

	mu          sync.RWMutex
	routes      map[string]*Route
	server      *http.Server
	unixLn      net.Listener
	tcpLn       net.Listener
	boundTCPort int // resolved port if TCPAddr ends in :0
	accLog      *log.Logger
	accFile     *os.File
}

// New constructs a Facade. socketPath may be empty to disable the Unix
// listener; tcpAddr may be empty to disable the TCP listener. Passing
// "127.0.0.1:0" asks the OS for an ephemeral port (read it back via
// TCPPort() after Start).
func New(socketPath, tcpAddr string, routes []Route, maxBody int64, idle time.Duration, accessLog, version string) *Facade {
	m := make(map[string]*Route, len(routes))
	for i := range routes {
		r := routes[i]
		m[r.key()] = &r
	}
	return &Facade{
		SocketPath:    socketPath,
		TCPAddr:       tcpAddr,
		Routes:        routes,
		MaxBodyBytes:  maxBody,
		IdleTimeout:   idle,
		AccessLogPath: accessLog,
		Version:       version,
		routes:        m,
	}
}

// TCPPort returns the bound TCP port (after Start). Zero means TCP is
// disabled or not yet bound.
func (f *Facade) TCPPort() int { return f.boundTCPort }

// AddRoute installs (or replaces) a route at runtime. Safe to call after
// Start and from multiple goroutines. Used by serve mode to mount a
// directory's skills lazily and to swap a stub route for a live one once
// credentials arrive.
func (f *Facade) AddRoute(r Route) {
	rr := r
	f.mu.Lock()
	if f.routes == nil {
		f.routes = make(map[string]*Route)
	}
	f.routes[rr.key()] = &rr
	f.mu.Unlock()
}

// RemoveRoute drops the route with the given namespace/mount. A no-op if
// absent. Used by serve mode when a directory is deactivated.
func (f *Facade) RemoveRoute(namespace, mount string) {
	f.mu.Lock()
	delete(f.routes, routeKey(namespace, mount))
	f.mu.Unlock()
}

// HasRoute reports whether a route exists for namespace/mount.
func (f *Facade) HasRoute(namespace, mount string) bool {
	f.mu.RLock()
	_, ok := f.routes[routeKey(namespace, mount)]
	f.mu.RUnlock()
	return ok
}

// UpstreamPort returns the upstream port of the route for namespace/mount,
// or 0 if there is no such route (or it is a stub with no upstream). Used
// by serve mode to mirror a live route as a flat single-dir alias (§5.5).
func (f *Facade) UpstreamPort(namespace, mount string) int {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if r, ok := f.routes[routeKey(namespace, mount)]; ok {
		return r.UpstreamPort
	}
	return 0
}

// Start opens the listeners and begins serving. Returns once both are
// bound. Call Close to stop.
func (f *Facade) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", f.handle)
	f.server = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       f.IdleTimeout,
	}

	if f.AccessLogPath != "" {
		af, err := os.OpenFile(f.AccessLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("facade: open access log: %w", err)
		}
		f.accFile = af
		f.accLog = log.New(af, "", 0)
	}

	if f.SocketPath != "" {
		_ = os.Remove(f.SocketPath) // clean stale
		if err := os.MkdirAll(dirOf(f.SocketPath), 0o700); err != nil {
			f.cleanupListeners()
			return fmt.Errorf("facade: mkdir socket dir: %w", err)
		}
		ln, err := net.Listen("unix", f.SocketPath)
		if err != nil {
			f.cleanupListeners()
			return fmt.Errorf("facade: listen unix %s: %w", f.SocketPath, err)
		}
		if err := os.Chmod(f.SocketPath, 0o600); err != nil {
			ln.Close()
			f.cleanupListeners()
			return fmt.Errorf("facade: chmod socket: %w", err)
		}
		f.unixLn = ln
		go func() {
			if err := f.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				_, _ = fmt.Fprintln(os.Stderr, "facade: serve unix:", err)
			}
		}()
	}

	if f.TCPAddr != "" {
		ln, err := net.Listen("tcp", f.TCPAddr)
		if err != nil {
			f.cleanupListeners()
			return fmt.Errorf("facade: listen tcp %s: %w", f.TCPAddr, err)
		}
		if ta, ok := ln.Addr().(*net.TCPAddr); ok {
			f.boundTCPort = ta.Port
		}
		f.tcpLn = ln
		go func() {
			if err := f.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				_, _ = fmt.Fprintln(os.Stderr, "facade: serve tcp:", err)
			}
		}()
	}

	if f.unixLn == nil && f.tcpLn == nil {
		return fmt.Errorf("facade: no listener configured (set SocketPath and/or TCPAddr)")
	}
	return nil
}

// cleanupListeners closes whatever has been opened so far. Safe to call
// from any partial state during Start.
func (f *Facade) cleanupListeners() {
	if f.unixLn != nil {
		_ = f.unixLn.Close()
		f.unixLn = nil
	}
	if f.tcpLn != nil {
		_ = f.tcpLn.Close()
		f.tcpLn = nil
	}
	if f.SocketPath != "" {
		_ = os.Remove(f.SocketPath)
	}
	if f.accFile != nil {
		_ = f.accFile.Close()
		f.accFile = nil
	}
}

// Close tears down the server and removes the socket.
func (f *Facade) Close() error {
	var firstErr error
	if f.server != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := f.server.Shutdown(shutCtx); err != nil {
			firstErr = err
		}
	}
	if f.unixLn != nil {
		_ = f.unixLn.Close()
	}
	if f.tcpLn != nil {
		_ = f.tcpLn.Close()
	}
	if f.accFile != nil {
		_ = f.accFile.Close()
	}
	if f.SocketPath != "" {
		_ = os.Remove(f.SocketPath)
	}
	return firstErr
}

// handle is the root HTTP handler.
func (f *Facade) handle(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	if r.URL.Path == "/" || r.URL.Path == "" {
		f.writeStatus(w, r)
		return
	}
	route, rest, ok := f.resolve(r.URL.Path)
	if !ok {
		if route == nil {
			// No segment at all.
			http.Error(w, "omac: invalid path", http.StatusNotFound)
			return
		}
		w.Header().Set("X-Omac-Reason", "unknown-mount")
		http.Error(w, "omac: unknown skill mount", http.StatusNotFound)
		return
	}

	// Stub routes never reach an upstream; they answer directly.
	switch route.State {
	case RoutePendingCredentials:
		w.Header().Set("X-Omac-Reason", "pending-credentials")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":  "pending-credentials",
			"skill":  route.Skill,
			"detail": route.Detail,
		})
		f.logAccess(r, route, rest, http.StatusConflict, 0, time.Since(started))
		return
	case RouteBroken:
		w.Header().Set("X-Omac-Reason", "skill-broken")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":  "skill-broken",
			"skill":  route.Skill,
			"detail": route.Detail,
		})
		f.logAccess(r, route, rest, http.StatusBadGateway, 0, time.Since(started))
		return
	}

	// Auto-discovery at the skill's top-level URL. When the request
	// addresses the mount root (no path beyond /<ns>/<mount>[/]) and the
	// skill ships a SKILL.md, serve that file instead of forwarding a bare
	// GET / to the sidecar. This gives every skill a useful landing page
	// (the same doc the agent reads) for free, with no per-sidecar code,
	// and turns the previously-confusing upstream 404 into real content.
	// It is a fallback, not an override: it only triggers for GET with an
	// empty remainder, so any real subpath — including a sidecar that
	// genuinely wants to serve its own root via a non-empty path — is
	// untouched. Skills without a SKILL.md keep proxying / as before.
	if rest == "" && r.Method == http.MethodGet && !isUpgrade(r) {
		if f.serveSkillDoc(w, r, route, started) {
			return
		}
	}

	// WebSocket / generic Upgrade path.
	if isUpgrade(r) {
		f.proxyUpgrade(w, r, route, rest, started)
		return
	}
	f.proxyHTTP(w, r, route, rest, started)
}

// skillDocName is the conventional human-readable manifest a skill ships
// at its root. The bridge serves it verbatim at the skill's top-level URL.
const skillDocName = "SKILL.md"

// serveSkillDoc writes <route.SkillDir>/SKILL.md to w and reports true when
// it handled the request. It returns false (handling nothing) when the
// route has no SkillDir, the file is absent/unreadable, or it escapes the
// skill dir — in which case the caller falls through to normal proxying.
func (f *Facade) serveSkillDoc(w http.ResponseWriter, r *http.Request, route *Route, started time.Time) bool {
	if route.SkillDir == "" {
		return false
	}
	docPath := filepath.Join(route.SkillDir, skillDocName)
	// Defense in depth: SkillDir comes from omac's own registry, but make
	// sure the resolved path stays inside the skill dir regardless.
	if !strings.HasPrefix(filepath.Clean(docPath), filepath.Clean(route.SkillDir)+string(os.PathSeparator)) {
		return false
	}
	data, err := os.ReadFile(docPath)
	if err != nil {
		return false
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("X-Omac-Discovery", "skill-md")
	w.WriteHeader(http.StatusOK)
	n, _ := w.Write(data)
	f.logAccess(r, route, "", http.StatusOK, int64(n), time.Since(started))
	return true
}

// resolve maps a request path to a route and the remaining path.
//
// It supports both flat mounts (/<mount>/<rest>, single-workdir mode) and
// namespaced mounts (/<namespace>/<mount>/<rest>, serve mode). It prefers
// the more specific two-segment key when one exists, so a namespaced route
// is never shadowed by a same-named flat route. Returns ok=false with a
// non-nil route==nil only when the path has no usable first segment;
// ok=false with route==nil also signals "no segment", while a real
// unknown-mount returns (nil, "", false) too — callers distinguish via the
// first segment having been present (we keep it simple: any miss => 404).
func (f *Facade) resolve(path string) (*Route, string, bool) {
	first, after := splitSegment(path)
	if first == "" {
		return nil, "", false
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	// Try namespaced: first=namespace, next=mount.
	if after != "" {
		mount, rest := splitSegment("/" + after)
		if mount != "" {
			if route, ok := f.routes[routeKey(first, mount)]; ok {
				return route, rest, true
			}
		}
	}
	// Fall back to flat: first=mount, after=rest.
	if route, ok := f.routes[first]; ok {
		return route, after, true
	}
	// Present first segment but no match.
	return &Route{}, "", false
}

func (f *Facade) writeStatus(w http.ResponseWriter, _ *http.Request) {
	type status struct {
		Version string   `json:"version"`
		Skills  []string `json:"skills"`
	}
	out := status{Version: f.Version}
	f.mu.RLock()
	for _, r := range f.routes {
		// SECURITY: do NOT enumerate per-directory routes here. In serve
		// mode every active directory's skills share this one facade port,
		// and a per-dir route's namespace is a random, secret bearer token
		// (see docs/MULTI_DIR_DESKTOP.md §4.1/§8.1). Listing those tokens on
		// the unauthenticated GET / index would let any session harvest them
		// and reach another directory's skills — a cross-workdir isolation
		// breach. Only expose routes that are not gated by a secret dir
		// token: flat start-mode mounts (empty namespace) and the
		// intentionally-shared global skills (__global__, the one designed
		// cross-dir surface, §4.5). A caller discovers its OWN directory's
		// skills via the per-session bridge manifest, never via this index.
		if r.Namespace == "" || r.Namespace == GlobalNamespace {
			out.Skills = append(out.Skills, r.key())
		}
	}
	f.mu.RUnlock()
	sort.Strings(out.Skills)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// proxyHTTP forwards plain HTTP (including SSE).
func (f *Facade) proxyHTTP(w http.ResponseWriter, r *http.Request, route *Route, rest string, started time.Time) {
	upstream := &url.URL{Scheme: "http", Host: upstreamHost(route.UpstreamPort)}
	rp := httputil.NewSingleHostReverseProxy(upstream)

	// Customize the Director so we can rewrite the path and header set.
	rp.Director = func(req *http.Request) {
		req.URL.Scheme = "http"
		req.URL.Host = upstream.Host
		req.URL.Path = "/" + rest
		req.Host = upstream.Host
		req.Header.Set("X-Forwarded-Prefix", "/"+route.key())
		// Hop-by-hop headers are stripped by httputil automatically.
	}

	// Enforce max body bytes for inbound request body (best-effort; SSE has no body).
	limit := route.MaxBodyBytes
	if limit == 0 {
		limit = f.MaxBodyBytes
	}
	if limit > 0 && r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	// Modify response to ensure SSE isn't buffered by any intermediate.
	rp.ModifyResponse = func(resp *http.Response) error {
		if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
			// net/http on the server side auto-flushes for chunked, but setting
			// X-Accel-Buffering tells any downstream-minded client we're streaming.
			resp.Header.Set("X-Accel-Buffering", "no")
		}
		return nil
	}

	rp.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		reason := "upstream-error"
		code := http.StatusBadGateway
		if isTimeout(err) {
			reason = "timeout"
			code = http.StatusGatewayTimeout
		} else if isConnRefused(err) {
			reason = "sidecar-down"
			code = http.StatusServiceUnavailable
		}
		w.Header().Set("X-Omac-Reason", reason)
		http.Error(w, "omac: "+reason, code)
	}

	// Short connect timeout; liberal overall IO.
	rp.Transport = &http.Transport{
		DialContext: (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		// Disable response buffering so SSE frames flush immediately.
		DisableCompression:    true,
		ResponseHeaderTimeout: 30 * time.Second,
	}

	// Wrap ResponseWriter so we capture the upstream status for logging.
	wr := &statusCaptureWriter{ResponseWriter: w, status: http.StatusOK}
	rp.ServeHTTP(wr, r)
	f.logAccess(r, route, rest, wr.status, wr.bytes, time.Since(started))
}

// proxyUpgrade handles HTTP Upgrade requests (WebSocket) by splicing the
// underlying TCP connections after forwarding the handshake.
func (f *Facade) proxyUpgrade(w http.ResponseWriter, r *http.Request, route *Route, rest string, started time.Time) {
	upstreamAddr := upstreamHost(route.UpstreamPort)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	upConn, err := (&net.Dialer{}).DialContext(ctx, "tcp", upstreamAddr)
	if err != nil {
		w.Header().Set("X-Omac-Reason", "sidecar-down")
		http.Error(w, "omac: upstream dial: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer upConn.Close()

	// Write the rewritten request to the upstream connection.
	clone := r.Clone(r.Context())
	clone.URL = &url.URL{Scheme: "http", Host: upstreamAddr, Path: "/" + rest, RawQuery: r.URL.RawQuery}
	clone.Host = upstreamAddr
	clone.RequestURI = clone.URL.RequestURI()
	clone.Header = r.Header.Clone()
	clone.Header.Set("X-Forwarded-Prefix", "/"+route.key())
	if err := clone.Write(upConn); err != nil {
		w.Header().Set("X-Omac-Reason", "upstream-error")
		http.Error(w, "omac: write upstream: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Hijack the client connection.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "omac: hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, buf, err := hj.Hijack()
	if err != nil {
		http.Error(w, "omac: hijack: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer clientConn.Close()

	// Flush anything already buffered from the client to the upstream.
	if buf != nil && buf.Reader.Buffered() > 0 {
		if _, err := io.CopyN(upConn, buf.Reader, int64(buf.Reader.Buffered())); err != nil {
			return
		}
	}

	// Splice both directions.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upConn, clientConn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(clientConn, upConn); done <- struct{}{} }()
	<-done
	f.logAccess(r, route, rest, 101, 0, time.Since(started))
}

func (f *Facade) logAccess(r *http.Request, route *Route, rest string, status int, bytes int64, dur time.Duration) {
	if f.accLog == nil {
		return
	}
	entry := map[string]any{
		"ts":              time.Now().UTC().Format(time.RFC3339Nano),
		"method":          r.Method,
		"mount":           route.Mount,
		"path":            "/" + rest,
		"upstream_status": status,
		"bytes_out":       bytes,
		"duration_ms":     dur.Milliseconds(),
	}
	b, _ := json.Marshal(entry)
	f.accLog.Println(string(b))
}

// splitSegment returns (firstSegment, rest) for a request path.
// "/slack/foo/bar" → ("slack", "foo/bar").
// "/slack/"        → ("slack", "").
// "/slack"         → ("slack", "").
// "/"              → ("", "").
func splitSegment(p string) (string, string) {
	if len(p) < 2 || p[0] != '/' {
		return "", ""
	}
	rest := p[1:]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return rest, ""
	}
	return rest[:slash], rest[slash+1:]
}

// splitMount is the historical name for splitSegment, retained for the
// single-segment path case and its existing test.
func splitMount(p string) (string, string) { return splitSegment(p) }

func upstreamHost(port int) string { return "127.0.0.1:" + strconv.Itoa(port) }

func isUpgrade(r *http.Request) bool {
	if !headerHasToken(r.Header.Get("Connection"), "upgrade") {
		return false
	}
	return r.Header.Get("Upgrade") != ""
}

func headerHasToken(hdr, token string) bool {
	for _, f := range strings.Split(hdr, ",") {
		if strings.EqualFold(strings.TrimSpace(f), token) {
			return true
		}
	}
	return false
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "connection refused") || strings.Contains(s, "connect: connection refused")
}

func dirOf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return "."
}

// statusCaptureWriter records status + byte counts for access logging.
type statusCaptureWriter struct {
	http.ResponseWriter
	status    int
	bytes     int64
	headerSet bool
}

func (w *statusCaptureWriter) WriteHeader(code int) {
	w.status = code
	w.headerSet = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusCaptureWriter) Write(b []byte) (int, error) {
	if !w.headerSet {
		w.headerSet = true
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush implements http.Flusher when the underlying writer does.
func (w *statusCaptureWriter) Flush() {
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

// (No Hijack method on the wrapper; the facade calls Hijack on the raw
// ResponseWriter in proxyUpgrade before wrapping would have happened.)
