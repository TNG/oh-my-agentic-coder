// Package credproxy implements the scoped host-side credential-lift proxy
// for private Maven registry access (GitHub issue #92, JVM build executor
// ticket 06).
//
// Credential lift = the long-lived registry credential stays OUTSIDE the
// JVM build executor. The executor (Gradle) sees only a non-secret local
// loopback HTTP URL per approved private registry; the credential proxy —
// running host-side, unsandboxed — authenticates upstream Maven repos with
// the developer's OMAC-managed keychain credential while Gradle receives
// no credential at all.
//
// Two proxies run side by side for the build path (see
// internal/cli/build_proxy.go):
//
//   - the EXISTING filtered proxy (internal/netproxy) handles public
//     dependency resolution (repo.maven.apache.org, plugins.gradle.org,
//     ...) over direct CONNECT tunnels — no TLS interception.
//   - THIS credential-lift proxy handles ONLY the declared private
//     registry upstreams. It is a forward HTTP proxy: it receives
//     plain-HTTP requests for a private-repo path, injects an
//     `Authorization: Basic <user:pass>` header using the keychain
//     credential, and forwards to the upstream over a fresh TLS/HTTP
//     connection. The credential NEVER appears in executor env, args,
//     gradle.properties, the cache leaf, logs, or audit.
//
// The proxy is READ-ONLY for the dependency workflow: only GET and HEAD
// are forwarded (dependency resolution / metadata / artifact download).
// PUT/POST/DELETE (publish, deploy) and any request to an unregistered
// upstream are denied with a structured denial naming the registry alias
// — never the credential.
//
// v1 posture: started on macOS (Shape A, env-only network) only. On
// Linux the build executor is kernel-blocked, so neither the filtered
// proxy nor the credential proxy is started (build_proxy.go returns
// empty). Credential values never enter executor files/env/args/logs.
package credproxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/stableport"
)

// portFileName is the .omac-control control-state file recording the
// assigned credential-lift proxy port.
const portFileName = "credproxy-port"

// RegistryKeychainService returns the OMAC keychain service name under
// which a private registry's credential is stored. The credential is
// keyed by the registry ALIAS (the non-secret manifest entry), so the
// manifest carries only alias + upstream; the credential is the
// developer's keychain entry.
//
// Convention (documented in docs/build-command.md):
//
//	service = "omac/build/registry/<alias>"
//	account = "credential"
//	value  = "<user>:<password>"   (HTTP Basic auth credentials)
//
// The value is split on the first ':' to form the Basic-auth user/password
// pair sent upstream. A missing keychain entry yields a
// *RegistryCredentialError naming the alias (criterion 7); the credential
// itself never appears in the error.
func RegistryKeychainService(alias string) string {
	return "omac/build/registry/" + alias
}

// CredentialAccount is the keychain account name for a registry credential.
const CredentialAccount = "credential"

// CredentialLookup is the host-side keychain read seam. Production wires
// keychain.Get; tests inject a fake. It returns the credential for a
// registry alias as a secrets.Secret (redacted String/GoString, refuses
// JSON marshal). A missing/unavailable keychain yields
// ErrCredentialMissing so the caller can map it to a structured denial.
type CredentialLookup func(alias string) (secrets.Secret, error)

// ErrCredentialMissing is returned by a CredentialLookup when no
// credential exists for the alias (the keychain has no entry, or the OS
// keychain backend is unavailable). The credential proxy startup fails
// with a *RegistryCredentialError carrying this; the value itself is
// never surfaced.
var ErrCredentialMissing = errors.New("credproxy: registry credential missing from keychain")

// RegistryCredentialError is a structured diagnostic for a build that
// declared an approved private registry alias but has no OMAC keychain
// credential for it. It names the alias and the keychain setup required
// WITHOUT the credential value. The CLI maps it to ExitPolicyDenied (3)
// — the build cannot resolve private dependencies without the lift, and
// the credential cannot be recovered from inside the executor.
//
// Mirrors buildmanifest.MissingCapabilityError's shape (spec.md:234-242):
// names the resource, the keychain setup, the restart requirement, and
// "current session policy is frozen; do not retry".
type RegistryCredentialError struct {
	// Alias is the approved registry alias whose keychain credential
	// is missing.
	Alias string
	// Kind classifies why the credential is unavailable so the
	// diagnostic points at the right fix without brittle substring
	// matching of Reason. Never the credential value.
	Kind CredentialErrKind
	// Reason is a human-readable detail (never the credential value).
	Reason string
}

// CredentialErrKind classifies a RegistryCredentialError so the
// diagnostic's fix hint is exact rather than substring-derived.
type CredentialErrKind int

const (
	// CredentialMissing means no keychain entry exists for the alias
	// (the developer has not run `omac secrets set` yet).
	CredentialMissing CredentialErrKind = iota
	// CredentialBackendUnavailable means the OS keychain backend is not
	// running/accessible (headless Linux without Secret Service, or a
	// locked macOS keychain). The fix is OS-side, not an `omac secrets set`.
	CredentialBackendUnavailable
	// CredentialReadFailed is a generic keychain read error not covered
	// by the two specific kinds above.
	CredentialReadFailed
)

func (e *RegistryCredentialError) Error() string { return e.Render() }

// Render produces the spec-exact diagnostic text. It names the alias and
// the keychain service/account convention the developer must populate,
// the restart requirement, and that retrying in the frozen session cannot
// succeed. The wording fragments are asserted in tests.
func (e *RegistryCredentialError) Render() string {
	return fmt.Sprintf(
		"OMAC build denied private registry %q.\n"+
			"Add the registry credential to the OMAC keychain:\n"+
			"  service = %s\n"+
			"  account = %s\n"+
			"  value  = <user>:<password>\n"+
			"%s, then restart OMAC to activate the credential lift.\n"+
			"The current session policy is frozen; do not retry.",
		e.Alias, RegistryKeychainService(e.Alias), CredentialAccount,
		restartHint(e.Kind),
	)
}

// restartHint renders the kind-specific fix. A missing entry points at
// `omac secrets set`; an unavailable backend points at the OS fix; a
// generic read failure points at the keychain entry + the underlying error.
func restartHint(kind CredentialErrKind) string {
	switch kind {
	case CredentialBackendUnavailable:
		return "Start the OS keychain backend (Secret Service on Linux / unlock the macOS keychain)"
	case CredentialReadFailed:
		return "Check the keychain entry and retry `omac secrets set <alias>`"
	default:
		return "Run `omac secrets set <alias>` (or set the keychain entry directly)"
	}
}

// Registry maps an approved private registry alias to its non-secret
// upstream identity (the Maven repo URL, no embedded userinfo — the
// manifest rejects `@` at parse time) and the resolved credential for
// that alias. The credential is read once at proxy startup (host-side,
// unsandboxed) and held in-process as a secrets.Secret; it is NEVER
// written to env, args, gradle.properties, the cache leaf, logs, or
// audit. The Upstream is the manifest's `upstream:` field.
type Registry struct {
	Alias      string
	Upstream   string         // non-secret, no userinfo
	Credential secrets.Secret // host-side only; zero value = none
}

// Config configures a credential-lift proxy Server. Registries is the
// approved private registry set (validated by NewServerWithConfig exactly
// as NewServer does). WorktreePath and ControlLeaf are OPTIONAL: when
// WorktreePath is empty the legacy random-port behavior is used (tests,
// callers without a worktree). When set, the proxy binds the
// deterministic stableport port for the worktree and records it under
// ControlLeaf/.omac-control/credproxy-port (when ControlLeaf is set) so
// the port survives listener teardown between runs (see choosePort).
type Config struct {
	Registries   []Registry
	WorktreePath string // canonical worktree path; empty = legacy random port
	ControlLeaf  string // GRADLE_USER_HOME cache leaf for the port file; optional
	Logf         func(string, ...any)
}

// Server is the credential-lift proxy. By default (Config.WorktreePath
// set) it binds the deterministic stableport loopback port for the
// worktree (range [30000,40000), persisted under ControlLeaf so it
// survives listener teardown between runs — see choosePort), with a
// fallback to a random ephemeral port when the stable window is occupied.
// The legacy path (empty WorktreePath) binds a kernel-assigned ephemeral
// port on 127.0.0.1. It serves plain-HTTP forward requests for the
// approved private registries, injecting `Authorization: Basic` upstream
// from the keychain credential held in-process. Gradle points at it
// through an OMAC-authored init.d script that maps each alias to
// http://127.0.0.1:<port>/<alias>/.
type Server struct {
	registries   map[string]Registry // keyed by alias
	worktreePath string
	controlLeaf  string
	ln           net.Listener
	logf         func(format string, args ...any)

	mu     sync.Mutex
	closed bool
	conns  map[net.Conn]struct{}
}

// NewServer validates the registries and builds a Server (does NOT start
// it — call Start). A zero-length registries slice yields a Server that
// denies everything (no private registries approved); callers usually
// skip starting it in that case. Duplicate aliases are rejected. It is a
// thin wrapper over NewServerWithConfig with the legacy random-port
// behavior (no worktree path).
func NewServer(registries []Registry, logf func(string, ...any)) (*Server, error) {
	return NewServerWithConfig(Config{Registries: registries, Logf: logf})
}

// NewServerWithConfig validates the config and builds a Server (does NOT
// start it — call Start). Registry validation is identical to NewServer
// (NewServer delegates here).
func NewServerWithConfig(cfg Config) (*Server, error) {
	logf := cfg.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	seen := map[string]bool{}
	for _, r := range cfg.Registries {
		if r.Alias == "" {
			return nil, fmt.Errorf("credproxy: registry with empty alias")
		}
		if r.Upstream == "" {
			return nil, fmt.Errorf("credproxy: registry %q: empty upstream", r.Alias)
		}
		if strings.Contains(r.Upstream, "@") {
			return nil, fmt.Errorf("credproxy: registry %q: upstream must not contain embedded credentials", r.Alias)
		}
		up, err := url.Parse(r.Upstream)
		if err != nil || up.Host == "" || (up.Scheme != "http" && up.Scheme != "https") {
			return nil, fmt.Errorf("credproxy: registry %q: upstream %q must be an absolute http(s) URL", r.Alias, r.Upstream)
		}
		if seen[r.Alias] {
			return nil, fmt.Errorf("credproxy: duplicate registry alias %q", r.Alias)
		}
		seen[r.Alias] = true
	}
	rm := map[string]Registry{}
	for _, r := range cfg.Registries {
		rm[r.Alias] = r
	}
	return &Server{
		registries:   rm,
		worktreePath: cfg.WorktreePath,
		controlLeaf:  cfg.ControlLeaf,
		logf:         logf,
		conns:        map[net.Conn]struct{}{},
	}, nil
}

// Start binds the loopback listener and serves in a goroutine. When a
// worktree path is wired the listener binds the deterministic stable port
// (with scan/ephemeral fallback — see choosePort) and the assigned port
// is persisted to the control file (best-effort) so the next run can
// prefer it.
func (s *Server) Start() error {
	port, fallback := s.choosePort()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		// The chosen port (stable or random) was not bindable; retry once
		// with a kernel-assigned ephemeral port so a transient bind race
		// or a stale control file pointing at an in-use port never wedges
		// the build. Correctness over determinism.
		if port != 0 {
			s.logf("credproxy: bind on chosen port %d failed (%v); falling back to a random ephemeral port", port, err)
			ln, err = net.Listen("tcp", "127.0.0.1:0")
		}
		if err != nil {
			return fmt.Errorf("credproxy: bind listener: %w", err)
		}
		fallback = true
	}
	s.ln = ln
	if fallback {
		s.logf("credproxy: using fallback ephemeral port %d (stable window unavailable; init-script repository URL may drift on next run)", s.Port())
	}
	// Persist the assigned port so the next run can prefer it. Any port
	// inside [StablePortMin, StablePortMax) — preferred OR a scanned
	// neighbor — is persisted so the next run prefers exactly what this run
	// bound, breaking the permanent warn-loop where a scanned neighbor is
	// treated as "fallback" and never persisted (issue #191). A true
	// out-of-window random/ephemeral port is NOT persisted because it would
	// poison the control file — the next run would prefer a dead-ephemeral
	// or out-of-range value and destabilize again. Best-effort: a write
	// failure degrades cross-run stability but does not fail the build (the
	// port is valid for this run).
	if s.controlLeaf != "" && !fallback {
		if werr := stableport.WritePreferred(s.controlLeaf, portFileName, s.Port()); werr != nil {
			s.logf("credproxy: could not persist port file: %v", werr)
		}
	}
	go s.acceptLoop()
	return nil
}

// logUnbindablePreferred logs why a preferred stable port could not be
// bound, with the actual listen error (issue #191: EADDRINUSE vs EPERM vs
// sandbox-blocked) so the user can diagnose instead of guessing.
// Select consults the preferred port FIRST, so the first onReason
// callback is always the preferred port's failure; only it is logged here
// to keep the line actionable.
func (s *Server) logUnbindablePreferred(reasons map[int]error, preferred int) {
	if err, ok := reasons[preferred]; ok {
		s.logf("credproxy: preferred stable port %d unavailable: %v", preferred, err)
	}
}

// choosePort resolves the loopback port Start should bind. It prefers, in
// order: (1) a previously-assigned port read from the control-state file
// (so the port stays stable even after the listener is torn down between
// runs); (2) a fresh stable port derived from the worktree path; (3) a
// fallback random ephemeral port when the whole stable window is occupied.
// Returns the chosen port and a fallback flag (true when the chosen port
// is NOT the deterministic stable one — the caller logs a warning so the
// user understands the warm-daemon bug may resurface in the rare collision
// case). When WorktreePath is empty the legacy random-port behavior is
// used (port 0, not flagged as fallback — that is the documented v1 path).
func (s *Server) choosePort() (port int, fallback bool) {
	preferred := 0
	reasons := map[int]error{}
	port, fallback = stableport.Choose(s.worktreePath, s.controlLeaf, portFileName,
		stableport.IsFree, stableport.RandomFree,
		func(port int, cause error) {
			if preferred == 0 {
				preferred = port
			}
			reasons[port] = cause
		})
	if preferred != 0 {
		s.logUnbindablePreferred(reasons, preferred)
	}
	return port, fallback
}

// Port returns the bound port (after Start), 0 before.
func (s *Server) Port() int {
	if s.ln == nil {
		return 0
	}
	return s.ln.Addr().(*net.TCPAddr).Port
}

// URL returns the non-secret local loopback URL Gradle points at for a
// given registry alias: http://127.0.0.1:<port>/<alias>/. The URL carries
// NO credential — the credential rides upstream from the proxy. Returns
// "" if the alias is not registered or the server is not started.
func (s *Server) URL(alias string) string {
	if s.ln == nil {
		return ""
	}
	if _, ok := s.registries[alias]; !ok {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d/%s/", s.Port(), alias)
}

// Close stops the listener and tears down active connections.
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
			return
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

// handle serves one HTTP request. It reads one request head, dispatches
// to the forward handler, and tears down the connection (HTTP/1.1
// connection reuse is not required for dependency resolution — Gradle's
// HTTP client re-dials as needed).
func (s *Server) handle(conn net.Conn) {
	conn.SetDeadline(time.Now().Add(requestTimeout))
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil {
		return
	}
	s.forward(conn, req)
}

// requestTimeout bounds a single proxied request end-to-end. Generous
// enough for an artifact download on a slow link; short enough that a
// wedged upstream does not hold the connection forever.
const requestTimeout = 5 * time.Minute

// allowedMethods are the HTTP methods the credential-lift proxy
// forwards. Maven dependency resolution is GET (artifact/metadata
// download) and HEAD (presence check). Publish/deploy (PUT/POST/DELETE)
// is denied (criterion 5) — the proxy is read-only for the supported
// dependency workflow.
var allowedMethods = map[string]bool{
	http.MethodGet:  true,
	http.MethodHead: true,
}

// forward proxies one request to the upstream Maven repo, injecting the
// Authorization header from the keychain credential. The request path
// encodes the alias as the first segment: GET /<alias>/<repo-path>.
func (s *Server) forward(conn net.Conn, req *http.Request) {
	// Resolve the alias from the first path segment. Origin-form
	// requests (the init.d-rewritten repository) arrive as
	// GET /<alias>/foo/bar.pom; absolute-URI requests (if a client is
	// configured for forward proxying) are rejected here — Gradle is
	// pointed at the proxy as an ORIGIN server via the init.d script,
	// not as a forward proxy.
	alias, rest, ok := splitAliasPath(req.URL.Path)
	if !ok {
		s.deny(conn, http.StatusNotFound, "unknown registry alias in path")
		return
	}
	reg, ok := s.registries[alias]
	if !ok {
		s.deny(conn, http.StatusForbidden, fmt.Sprintf("registry %q is not approved", alias))
		return
	}
	// Read-only: deny publish/deploy methods with a structured denial
	// naming the alias (criterion 5). Never the credential.
	if !allowedMethods[req.Method] {
		s.deny(conn, http.StatusMethodNotAllowed,
			fmt.Sprintf("OMAC credential proxy is read-only for dependency resolution; %s to registry %q is denied", req.Method, alias))
		return
	}
	up, err := url.Parse(reg.Upstream)
	if err != nil {
		s.deny(conn, http.StatusBadGateway, fmt.Sprintf("registry %q: invalid upstream", alias))
		return
	}
	// Build the upstream URL: upstream base + the repo path after the alias.
	target := *up
	target.Path = joinPath(up.Path, rest)
	target.RawQuery = req.URL.RawQuery

	// Build the upstream request. We do NOT copy the client's
	// Authorization header (the executor never had the credential; a
	// client-supplied Authorization is ignored). We inject the
	// keychain credential as Basic auth.
	outReq, err := http.NewRequestWithContext(context.Background(), req.Method, target.String(), req.Body)
	if err != nil {
		s.deny(conn, http.StatusBadGateway, fmt.Sprintf("registry %q: build upstream request", alias))
		return
	}
	// Copy non-sensitive headers. Drop hop-by-hop and auth headers.
	// Range (artifact resumption) is not in the drop list, so it is
	// forwarded by copyForwardHeaders like any other non-sensitive header.
	copyForwardHeaders(outReq.Header, req.Header)
	// Inject the credential. The credential value lives ONLY here, in
	// the Authorization header sent upstream over TLS. It is never
	// logged, never echoed to the client, never written to env/args.
	outReq.Header.Set("Authorization", basicAuth(reg.Credential))

	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(outReq)
	if err != nil {
		s.logf("credproxy: upstream error for %s: %v", alias, err)
		s.deny(conn, http.StatusBadGateway, fmt.Sprintf("registry %q: upstream unreachable", alias))
		return
	}
	defer resp.Body.Close()
	writeResponse(conn, resp)
}

// copyForwardHeaders copies request headers that should reach upstream,
// dropping hop-by-hop and credential-bearing headers. The client's
// Authorization is dropped (the executor never had the real credential;
// a client-supplied one is ignored and replaced with the keychain
// credential upstream).
func copyForwardHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch strings.ToLower(k) {
		case "authorization", "proxy-authorization", "proxy-connection",
			"connection", "keep-alive", "te", "trailer",
			"transfer-encoding", "upgrade", "host":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// basicAuth renders the HTTP Basic `Authorization: Basic <base64(user:pass)>`
// value from a secrets.Secret holding "user:password". The credential
// value is read here and placed ONLY in the upstream header. If the
// secret is empty (no credential resolved), return "" so no auth header
// is sent (the upstream will 401; the diagnostic naming the alias — not
// the credential — is produced at startup, not here).
func basicAuth(cred secrets.Secret) string {
	if cred.IsEmpty() {
		return ""
	}
	return "Basic " + base64.StdEncoding.EncodeToString(cred.Expose())
}

// splitAliasPath splits a request path of the form /<alias>/<rest> into
// the alias and the remaining path. ok=false if the path is empty or has
// no second segment. The alias is the first non-empty path segment.
func splitAliasPath(path string) (alias, rest string, ok bool) {
	path = strings.TrimPrefix(path, "/")
	if path == "" {
		return "", "", false
	}
	idx := strings.IndexByte(path, '/')
	if idx < 0 {
		return path, "", true
	}
	return path[:idx], path[idx+1:], true
}

// joinPath joins an upstream base path with a repo-relative path,
// collapsing duplicate slashes so a manifest upstream of
// https://maven.internal.example/repo and a request /internal/foo/bar
// resolves to https://maven.internal.example/repo/foo/bar.
func joinPath(base, rest string) string {
	if base == "" {
		return "/" + rest
	}
	if rest == "" {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(rest, "/")
}

// deny writes a minimal HTTP/1.1 denial back to the client. The body
// carries the structured reason naming the alias (criterion 7) — never
// the credential. It is marked X-Omac-Sandbox so a human/agent can tell
// a policy denial from a real upstream error.
func (s *Server) deny(conn net.Conn, status int, reason string) {
	s.logf("credproxy: DENY %d %s", status, reason)
	body := reason + "\n"
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nX-Omac-Sandbox: denied\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

// writeResponse streams the upstream response back to the client,
// preserving status, headers (minus hop-by-hop), and the body. SSE-safe
// (no buffering beyond the kernel socket). The Authorization response
// header from upstream (if any) is dropped — it would echo the
// credential the client never had.
func writeResponse(conn net.Conn, resp *http.Response) {
	var hdr strings.Builder
	fmt.Fprintf(&hdr, "HTTP/1.1 %s\r\n", resp.Status)
	for k, vs := range resp.Header {
		switch strings.ToLower(k) {
		case "connection", "keep-alive", "te", "trailer",
			"transfer-encoding", "upgrade", "authorization":
			continue
		}
		for _, v := range vs {
			fmt.Fprintf(&hdr, "%s: %s\r\n", k, v)
		}
	}
	hdr.WriteString("Connection: close\r\n\r\n")
	if _, err := conn.Write([]byte(hdr.String())); err != nil {
		return
	}
	_, _ = io.Copy(conn, resp.Body)
}
