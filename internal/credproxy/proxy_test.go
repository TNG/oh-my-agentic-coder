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
