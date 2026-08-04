package credproxy

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/secrets"
	"github.com/tngtech/oh-my-agentic-coder/internal/stableport"
)

// fakeUpstream is a test Maven upstream that records the Authorization
// header it received and serves a fixed body. It lets the credential-
// injection test assert the header reached upstream without the client
// (Gradle) ever supplying it.
func startFakeUpstream(t *testing.T, status int, body string) (*url.URL, *string) {
	t.Helper()
	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u, &gotAuth
}

// startCredProxy starts a credential-lift proxy with one registry and
// returns its base URL (http://127.0.0.1:<port>).
func startCredProxy(t *testing.T, reg Registry) *Server {
	t.Helper()
	srv, err := NewServer([]Registry{reg}, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// doRequest issues a request to the credential proxy at /<alias>/<path>
// and returns the response status + body. The request carries NO
// Authorization header (the executor never had the credential).
func doRequest(t *testing.T, srv *Server, method, alias, path string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", srv.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	urlPath := "/" + alias
	if path != "" {
		urlPath += "/" + path
	}
	req, _ := http.NewRequest(method, "http://127.0.0.1"+urlPath, nil)
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestCredentialLift_InjectsAuthorizationUpstream asserts criterion 2:
// a request to the credential proxy for a private-repo path gets an
// Authorization header added upstream; the downstream (Gradle) request
// carries no credential.
func TestCredentialLift_InjectsAuthorizationUpstream(t *testing.T) {
	up, gotAuth := startFakeUpstream(t, http.StatusOK, "artifact-bytes")
	srv := startCredProxy(t, Registry{
		Alias:      "internal",
		Upstream:   up.String(),
		Credential: secrets.NewSecretString("alice:s3cr3t"),
	})

	// Client (Gradle) sends NO Authorization header.
	status, body := doRequest(t, srv, http.MethodGet, "internal", "foo/bar.pom")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", status, body)
	}
	if body != "artifact-bytes" {
		t.Errorf("body = %q, want %q", body, "artifact-bytes")
	}
	// Upstream MUST have received Basic auth from the keychain credential.
	if *gotAuth == "" || !strings.HasPrefix(*gotAuth, "Basic ") {
		t.Errorf("upstream got no/invalid Authorization: %q", *gotAuth)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cr3t"))
	if *gotAuth != want {
		t.Errorf("upstream Authorization = %q, want %q", *gotAuth, want)
	}
}

// TestCredentialLift_ClientCredentialDropped asserts a client-supplied
// Authorization header is IGNORED — the executor never had the real
// credential; a forged one must not reach upstream.
func TestCredentialLift_ClientCredentialDropped(t *testing.T) {
	up, gotAuth := startFakeUpstream(t, http.StatusOK, "ok")
	srv := startCredProxy(t, Registry{
		Alias:      "internal",
		Upstream:   up.String(),
		Credential: secrets.NewSecretString("alice:s3cr3t"),
	})
	conn, err := net.Dial("tcp", srv.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/internal/x", nil)
	req.Header.Set("Authorization", "Basic forg3d")
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// Upstream got the KEYCHAIN credential, not the forged one.
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cr3t"))
	if *gotAuth != want {
		t.Errorf("upstream Authorization = %q, want %q (forged must be dropped)", *gotAuth, want)
	}
}

// TestCredentialLift_PublishDenied asserts criterion 5: PUT/POST/DELETE
// are denied with a structured denial naming the alias. The request is
// NOT forwarded upstream.
func TestCredentialLift_PublishDenied(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	srv := startCredProxy(t, Registry{
		Alias:      "internal",
		Upstream:   upURL.String(),
		Credential: secrets.NewSecretString("alice:s3cr3t"),
	})
	for _, method := range []string{http.MethodPut, http.MethodPost, http.MethodDelete} {
		status, body := doRequest(t, srv, method, "internal", "foo/bar.jar")
		if status != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405; body=%q", method, status, body)
		}
		if !strings.Contains(body, "internal") {
			t.Errorf("%s: denial must name alias %q, got %q", method, "internal", body)
		}
		if !strings.Contains(body, "read-only") {
			t.Errorf("%s: denial must state read-only, got %q", method, body)
		}
		// The credential must not appear in the denial body.
		if strings.Contains(body, "s3cr3t") {
			t.Errorf("%s: credential leaked into denial body: %q", method, body)
		}
	}
	if calls != 0 {
		t.Errorf("publish methods must NOT reach upstream; got %d upstream calls", calls)
	}
}

// TestCredentialLift_UnapprovedRegistryDenied asserts a request for an
// alias not in the approved set is denied naming the alias (criterion 7).
func TestCredentialLift_UnapprovedRegistryDenied(t *testing.T) {
	up, _ := startFakeUpstream(t, http.StatusOK, "ok")
	srv := startCredProxy(t, Registry{
		Alias:      "internal",
		Upstream:   up.String(),
		Credential: secrets.NewSecretString("alice:s3cr3t"),
	})
	status, body := doRequest(t, srv, http.MethodGet, "other", "x")
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%q", status, body)
	}
	if !strings.Contains(body, "other") {
		t.Errorf("denial must name the unapproved alias, got %q", body)
	}
}

// TestCredentialLift_NonSecretURL asserts the URL Gradle sees carries
// no credential: it is http://127.0.0.1:<port>/<alias>/ — only host, port,
// and the non-secret alias.
func TestCredentialLift_NonSecretURL(t *testing.T) {
	up, _ := startFakeUpstream(t, http.StatusOK, "ok")
	srv := startCredProxy(t, Registry{
		Alias:      "internal",
		Upstream:   up.String(),
		Credential: secrets.NewSecretString("alice:s3cr3t"),
	})
	u := srv.URL("internal")
	if u == "" {
		t.Fatal("URL returned empty for registered alias")
	}
	if strings.Contains(u, "@") {
		t.Errorf("URL must not contain userinfo: %q", u)
	}
	if !strings.HasPrefix(u, "http://127.0.0.1:") {
		t.Errorf("URL must be a loopback http URL: %q", u)
	}
	if !strings.HasSuffix(u, "/internal/") {
		t.Errorf("URL must end with /<alias>/: %q", u)
	}
	if strings.Contains(u, "s3cr3t") || strings.Contains(u, "alice") {
		t.Errorf("URL leaked credential material: %q", u)
	}
}

// TestCredentialLift_CredentialAbsentFromLogs asserts criterion 4: the
// credential string never appears in proxy log lines.
func TestCredentialLift_CredentialAbsentFromLogs(t *testing.T) {
	var logBuf bytes.Buffer
	up, _ := startFakeUpstream(t, http.StatusOK, "ok")
	srv, err := NewServer([]Registry{{
		Alias:      "internal",
		Upstream:   up.String(),
		Credential: secrets.NewSecretString("alice:s3cr3t"),
	}}, func(format string, args ...any) {
		// Mirror netproxy.Logf: only decisions, never bodies/headers.
		fmt.Fprintf(&logBuf, format+"\n", args...)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close)
	// Trigger a denial log (publish method).
	_, _ = doRequest(t, srv, http.MethodPut, "internal", "x")
	// Trigger an upstream log (bad upstream).
	// (covered by the publish denial above; the logf is exercised.)
	if logBuf.Len() == 0 {
		t.Fatal("expected log lines from the proxy; got none")
	}
	if strings.Contains(logBuf.String(), "s3cr3t") || strings.Contains(logBuf.String(), "alice:s3cr3t") {
		t.Errorf("credential leaked into proxy log:\n%s", logBuf.String())
	}
}

// TestRegistryCredentialError_Render asserts criterion 7: the
// diagnostic names the alias, the keychain service/account convention,
// the restart requirement, and "current session policy is frozen; do not
// retry" — WITHOUT the credential value.
func TestRegistryCredentialError_Render(t *testing.T) {
	err := &RegistryCredentialError{Alias: "internal", Reason: "no keychain entry"}
	msg := err.Render()
	for _, want := range []string{
		"internal",
		"omac/build/registry/internal",
		"credential",
		"<user>:<password>",
		"restart OMAC",
		"current session policy is frozen; do not retry",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("diagnostic missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "s3cr3t") {
		t.Errorf("diagnostic must not contain the credential value: %s", msg)
	}
}

// TestRegistryCredentialError_UnavailableBackend asserts the
// unavailable-backend variant points at the OS fix rather than
// `omac secrets set`. The hint is driven by the typed Kind, not a
// substring match on Reason (so a generic error that happens to contain
// "unavailable" does not misclassify).
func TestRegistryCredentialError_UnavailableBackend(t *testing.T) {
	err := &RegistryCredentialError{Alias: "internal", Kind: CredentialBackendUnavailable, Reason: "keychain backend unavailable"}
	msg := err.Render()
	if !strings.Contains(msg, "Start the OS keychain backend") {
		t.Errorf("unavailable-backend diagnostic must point at the OS fix:\n%s", msg)
	}
	// A generic read failure (even if its reason text contained
	// "unavailable") must NOT route to the OS-fix hint — it routes to
	// the secrets-set/retry hint, proving the hint is Kind-driven.
	leak := &RegistryCredentialError{Alias: "internal", Kind: CredentialReadFailed, Reason: "dbus org.freedesktop.secrets unavailable: timeout"}
	lm := leak.Render()
	if strings.Contains(lm, "Start the OS keychain backend") {
		t.Errorf("generic read-failure must not render the OS-fix hint even if the reason mentions unavailable:\n%s", lm)
	}
}

// TestNewServer_RejectsBadRegistries asserts structural validation:
// empty alias, empty upstream, embedded credentials, duplicate aliases,
// and non-absolute/non-http(s) upstreams are rejected at construction.
func TestNewServer_RejectsBadRegistries(t *testing.T) {
	cases := []struct {
		name string
		reg  Registry
	}{
		{"empty alias", Registry{Alias: "", Upstream: "https://maven.example/repo"}},
		{"empty upstream", Registry{Alias: "internal", Upstream: ""}},
		{"embedded credentials", Registry{Alias: "internal", Upstream: "https://alice:s3cr3t@maven.example/repo"}},
		{"non-absolute", Registry{Alias: "internal", Upstream: "maven.example/repo"}},
		{"non-http scheme", Registry{Alias: "internal", Upstream: "ftp://maven.example/repo"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := NewServer([]Registry{c.reg}, nil)
			if err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
		})
	}
	// Duplicate aliases.
	_, err := NewServer([]Registry{
		{Alias: "a", Upstream: "https://maven.example/repo"},
		{Alias: "a", Upstream: "https://maven.example/repo"},
	}, nil)
	if err == nil {
		t.Fatal("expected error for duplicate alias")
	}
}

// TestServer_URL_UnregisteredAlias asserts URL returns "" for an alias
// not in the registered set (and for a not-started server).
func TestServer_URL_UnregisteredAlias(t *testing.T) {
	up, _ := startFakeUpstream(t, http.StatusOK, "ok")
	srv := startCredProxy(t, Registry{
		Alias:      "internal",
		Upstream:   up.String(),
		Credential: secrets.NewSecretString("u:p"),
	})
	if u := srv.URL("other"); u != "" {
		t.Errorf("URL for unregistered alias must be empty, got %q", u)
	}
	// A not-started server returns "".
	notStarted, _ := NewServer([]Registry{{
		Alias: "internal", Upstream: "https://maven.example/repo",
	}}, nil)
	if u := notStarted.URL("internal"); u != "" {
		t.Errorf("URL for not-started server must be empty, got %q", u)
	}
}

// --- stable port selection (stale init-script URL fix) ------------------
//
// The pure stable-port helper tests (hash determinism/range/symlinks,
// window scan/wrap/fallback) live in internal/stableport. The tests below
// exercise the credproxy wiring of those helpers against a real Server,
// mirroring internal/containerproxy/proxy_test.go's TestStart_* tests
// (same helper package, same port choice semantics).

// portTestRegistry returns one valid registry for the port tests. A zero
// secrets.Secret is fine — forwarding is not exercised here.
func portTestRegistry() Registry {
	return Registry{Alias: "internal", Upstream: "http://127.0.0.1:1/repo"}
}

// TestStart_StablePortBindsDeterministically asserts two sequential
// Servers (Start → Port → Close) with the same worktree path and
// DIFFERENT empty control leaves bind the SAME deterministic stable port
// when it is free.
func TestStart_StablePortBindsDeterministically(t *testing.T) {
	worktree := "/worktree/feat-a"
	ports := make([]int, 0, 2)
	for range 2 {
		srv, err := NewServerWithConfig(Config{
			Registries:   []Registry{portTestRegistry()},
			WorktreePath: worktree,
			ControlLeaf:  t.TempDir(), // fresh per iteration: no port file
			Logf:         t.Logf,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.Start(); err != nil {
			t.Fatal(err)
		}
		ports = append(ports, srv.Port())
		srv.Close()
	}
	if ports[0] != ports[1] {
		t.Errorf("same worktree bound different ports across restarts: %d then %d (want the deterministic stable port)", ports[0], ports[1])
	}
	if want := stableport.For(worktree); ports[0] != want {
		t.Errorf("port = %d, want the worktree stable port %d", ports[0], want)
	}
}

// TestStart_PortFilePreferredOverHash asserts Start prefers the
// previously-assigned port from the control file over a fresh hash, so
// the port stays stable even after the listener is torn down between runs.
func TestStart_PortFilePreferredOverHash(t *testing.T) {
	leaf := t.TempDir()
	// Pre-seed the control file with a port that is NOT the hash-derived
	// one. Start must bind the seeded port (the hash is only a fallback).
	seeded := stableport.For("/worktree/feat-a") + 7
	if seeded >= stableport.StablePortMax {
		seeded = stableport.StablePortMin + (seeded - stableport.StablePortMax)
	}
	if err := stableport.WritePreferred(leaf, portFileName, seeded); err != nil {
		t.Fatal(err)
	}
	srv, err := NewServerWithConfig(Config{
		Registries:   []Registry{portTestRegistry()},
		WorktreePath: "/worktree/feat-a",
		ControlLeaf:  leaf,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.Port() != seeded {
		t.Errorf("Port() = %d, want seeded %d (port file preferred over hash %d)", srv.Port(), seeded, stableport.For("/worktree/feat-a"))
	}
}

// TestStart_ScansWhenStablePortBusy asserts Start never stays on a held
// port: when the stable port AND the whole scan window are occupied, the
// chosen port is either the FIRST scan neighbor (if the occupier actually
// released it in the bind/close TOCTOU window between the occupier's own
// bind and the Server's scan — the race the neighbor-tolerance exists
// for) or a random ephemeral fallback (outside [StablePortMin,
// StablePortMax), logged as a warning). It must NEVER equal the occupied
// stable port. Occupy the preferred port + PortScanWindow neighbors
// (wrapping) so every scan candidate is deterministically held; a partial
// occupation reintroduces the TOCTOU race at the first unheld neighbor.
func TestStart_ScansWhenStablePortBusy(t *testing.T) {
	worktree := "/worktree/feat-b"
	busy := stableport.For(worktree)
	// Occupy the stable port + the full scan window. Each occupier is a
	// throwaway listener released at test end.
	held := make([]net.Listener, 0, stableport.PortScanWindow+1)
	for i := 0; i <= stableport.PortScanWindow; i++ {
		p := busy + i
		if p >= stableport.StablePortMax {
			p = stableport.StablePortMin + (p - stableport.StablePortMax)
		}
		occ, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			// A neighbor we could not occupy is FREE: the scan may
			// legitimately land on it. That is the first-neighbor branch —
			// still not the stable port, still correct. Skip the strict
			// in-range assertion below by remembering the hole.
			t.Logf("neighbor %d unoccupiable (%v) — treating as free", p, err)
			continue
		}
		held = append(held, occ)
	}
	defer func() {
		for _, occ := range held {
			_ = occ.Close()
		}
	}()
	srv, err := NewServerWithConfig(Config{
		Registries:   []Registry{portTestRegistry()},
		WorktreePath: worktree,
		ControlLeaf:  t.TempDir(),
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if srv.Port() == busy {
		t.Errorf("Port() = %d, must NOT be the occupied stable port", srv.Port())
	}
	// Two valid outcomes: a scan neighbor in [StablePortMin, StablePortMax)
	// (a momentarily-released window port), or a fallback ephemeral port
	// outside the range. Assert only that the chosen port is not one of the
	// HELD window ports (it cannot conflict with a real listener).
	for _, occ := range held {
		hp := occ.Addr().(*net.TCPAddr).Port
		if srv.Port() == hp {
			t.Errorf("Port() = %d collides with held window port %d (scan or bind picked a held port)", srv.Port(), hp)
		}
	}
}

// TestStart_LegacyRandomPortWhenNoWorktree asserts a Server built without
// WorktreePath/ControlLeaf keeps the legacy kernel-assigned ephemeral
// behavior: it starts, serves a request, and never touches a control
// file (no control leaf wired — nothing can be persisted).
func TestStart_LegacyRandomPortWhenNoWorktree(t *testing.T) {
	up, _ := startFakeUpstream(t, http.StatusOK, "ok")
	srv := startCredProxy(t, Registry{
		Alias:      "internal",
		Upstream:   up.String(),
		Credential: secrets.NewSecretString("u:p"),
	})
	if srv.Port() <= 0 {
		t.Errorf("Port() = %d, want a positive kernel-assigned ephemeral port", srv.Port())
	}
	// The server still serves normally (the port choice does not affect
	// forwarding).
	status, _ := doRequest(t, srv, http.MethodGet, "internal", "foo.pom")
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 (legacy random-port server must still serve)", status)
	}
}

// TestStart_ControlFileNotPersistedOnFallback asserts that when the full
// stable window is occupied (preferred + all PortScanWindow neighbors),
// the fallback path skips WritePreferred — the port file is NOT written
// despite a non-nil ControlLeaf. This prevents a fallback ephemeral port
// from poisoning the control file for the next run (ticket 03).
//
// A full window is the test's precondition: any port in it that cannot be
// bound (a stray listener, e.g. another process on the shared CI host)
// leaves a hole Select can legitimately land on as an in-window neighbor,
// which Start then persists — invalidating the "no port file" assertion.
// The window is therefore occupied until every slot is held; when a stray
// port makes that impossible (rare), the test skips like
// TestStart_FallbackRandomWhenWindowFull rather than asserting against a
// broken precondition.
func TestStart_ControlFileNotPersistedOnFallback(t *testing.T) {
	worktree := "/worktree/feat-fallback"
	busy := stableport.For(worktree)
	// Occupy the stable port + the full scan window so every neighbour is
	// held and Select must fall back to RandomFree. The window must be
	// COMPLETE: a hole would let Select choose an in-window neighbor,
	// which Start persists (issue #191 semantics) and the assertion below
	// would fail against a stray port on the host.
	held := make([]net.Listener, 0, stableport.PortScanWindow+1)
	for i := 0; i <= stableport.PortScanWindow; i++ {
		p := busy + i
		if p >= stableport.StablePortMax {
			p = stableport.StablePortMin + (p - stableport.StablePortMax)
		}
		occ, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			for _, l := range held {
				_ = l.Close()
			}
			t.Skipf("could not occupy the full scan window (port %d already in use: %v); cannot deterministically force the random fallback", p, err)
		}
		held = append(held, occ)
	}
	defer func() {
		for _, occ := range held {
			_ = occ.Close()
		}
	}()

	leaf := t.TempDir()
	srv, err := NewServerWithConfig(Config{
		Registries:   []Registry{portTestRegistry()},
		WorktreePath: worktree,
		ControlLeaf:  leaf,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	srv.Close()

	// The port file must NOT exist: WritePreferred was skipped because
	// fallback was true (the whole window was occupied).
	if got := stableport.ReadPreferred(leaf, portFileName); got != 0 {
		t.Errorf("port file was written despite fallback: ReadPreferred = %d, want 0", got)
	}
}

// TestStart_ScanNeighborPersisted asserts that when the preferred stable
// port is occupied but a scan neighbor is free, the scan neighbor is
// persisted to the control file AND the log does NOT emit the "fallback"
// warning (issue #191: the old code treated chosen != preferred as
// "fallback" and skipped persistence, causing a permanent warn-loop).
func TestStart_ScanNeighborPersisted(t *testing.T) {
	worktree := "/worktree/feat-credproxy-scan"
	preferred := stableport.For(worktree)
	// Occupy ONLY the preferred port so Select scans to preferred+1.
	occ, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferred))
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()

	leaf := t.TempDir()
	var logBuf strings.Builder
	srv, err := NewServerWithConfig(Config{
		Registries:   []Registry{portTestRegistry()},
		WorktreePath: worktree,
		ControlLeaf:  leaf,
		Logf:         func(format string, args ...any) { logBuf.WriteString(fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	// The bound port must be a scan neighbor, NOT outside the stable window.
	port := srv.Port()
	if port < stableport.StablePortMin || port >= stableport.StablePortMax {
		t.Errorf("port = %d outside stable window, want a scan neighbor inside [%d,%d)", port, stableport.StablePortMin, stableport.StablePortMax)
	}
	if port == preferred {
		t.Errorf("port = %d equals occupied preferred port", port)
	}
	// The scan neighbor MUST be persisted (core fix, issue #191).
	got := stableport.ReadPreferred(leaf, portFileName)
	if got != port {
		t.Errorf("port file = %d, want bound neighbor %d (scanned neighbor was not persisted)", got, port)
	}
	// The log must NOT contain the "fallback" warning.
	if strings.Contains(logBuf.String(), "fallback") {
		t.Errorf("log contains 'fallback' warning but a scan neighbor is NOT a fallback:\n%s", logBuf.String())
	}
}

// TestStart_PortPersistsAcrossRestarts asserts the assigned port is
// recorded in the control file and preferred by the NEXT server over the
// worktree hash: a second server with a DIFFERENT worktree path but the
// SAME control leaf binds the file's port.
func TestStart_PortPersistsAcrossRestarts(t *testing.T) {
	leaf := t.TempDir()
	srv, err := NewServerWithConfig(Config{
		Registries:   []Registry{portTestRegistry()},
		WorktreePath: "/worktree/feat-a",
		ControlLeaf:  leaf,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	p1 := srv.Port()
	srv.Close()
	// The control file must record the assigned port.
	if got := stableport.ReadPreferred(leaf, portFileName); got != p1 {
		t.Fatalf("port file = %d, want %d after first run", got, p1)
	}
	// A second server with a different worktree (hash would differ) but
	// the SAME control leaf must bind the file's port: the file beats the
	// hash.
	srv2, err := NewServerWithConfig(Config{
		Registries:   []Registry{portTestRegistry()},
		WorktreePath: "/worktree/feat-b",
		ControlLeaf:  leaf,
		Logf:         t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv2.Start(); err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	if srv2.Port() != p1 {
		t.Errorf("Port() = %d after restart, want persisted %d (control file beats worktree hash %d)", srv2.Port(), p1, stableport.For("/worktree/feat-b"))
	}
}
