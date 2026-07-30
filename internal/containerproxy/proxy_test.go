package containerproxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/audit"
)

// fakeDaemon is a test Docker daemon recording the requests it receives.
type fakeDaemon struct {
	mux    *http.ServeMux
	server *httptest.Server
	calls  []recordedReq
	// createResponse is the JSON returned for POST /containers/create.
	createResponse string
	// inspectResponse is the JSON returned for GET /containers/{id}/json.
	inspectResponse string
	// networkCreateResponse is the JSON returned for POST /networks/create.
	networkCreateResponse string
	// sawAuthHeader records whether a /containers/create request reached
	// the daemon carrying an X-Registry-Auth header (the proxy must strip
	// it — review critical #2).
	sawAuthHeader bool
}

type recordedReq struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func newFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	d := &fakeDaemon{mux: http.NewServeMux()}
	d.mux.HandleFunc("/containers/create", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Registry-Auth") != "" {
			d.sawAuthHeader = true
		}
		b, _ := io.ReadAll(r.Body)
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		resp := d.createResponse
		if resp == "" {
			resp = `{"Id":"abc123","Warnings":[]}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, resp)
	})
	d.mux.HandleFunc("/networks/create", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		resp := d.networkCreateResponse
		if resp == "" {
			resp = `{"Id":"net-1","Warning":""}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, resp)
	})
	d.mux.HandleFunc("/networks/", func(w http.ResponseWriter, r *http.Request) {
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, ""})
		w.WriteHeader(http.StatusOK)
	})
	// Generic container endpoint: /containers/{id}/...
	d.mux.HandleFunc("/containers/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, string(b)})
		// Return the create response for /create, the inspect response for
		// /json, etc. Simplest: return inspectResponse for /json, OK otherwise.
		if strings.HasSuffix(r.URL.Path, "/json") {
			resp := d.inspectResponse
			if resp == "" {
				// Real Docker nests labels at Config.Labels (NOT top-level
				// Labels). The default fixture uses the real shape so the
				// ownership parse is exercised against what a real daemon
				// actually returns; a fixture with top-level Labels would
				// hide the Config.Labels parse bug (review critical #1).
				resp = `{"Id":"abc123","Config":{"Image":"pgvector/pgvector:pg16","Labels":{"omac.executor":"exec-1"}},"NetworkSettings":{"Ports":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"54321"}]}}}`
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, resp)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	d.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		d.calls = append(d.calls, recordedReq{r.Method, r.URL.Path, r.URL.RawQuery, ""})
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	// Wrap the mux so versioned /v1.44/... paths are stripped to /...
	// (the real Docker daemon accepts the versioned prefix; the fake
	// daemon's ServeMux handlers are version-agnostic).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = stripVersionPrefix(r.URL.Path)
		d.mux.ServeHTTP(w, r2)
	})
	d.server = httptest.NewServer(handler)
	t.Cleanup(d.server.Close)
	return d
}

// stripVersionPrefix removes a leading /v<digits>(.<digits>)?/ from path.
func stripVersionPrefix(path string) string {
	if !strings.HasPrefix(path, "/v") {
		return path
	}
	idx := strings.IndexByte(path[1:], '/')
	if idx < 0 {
		return path
	}
	seg := path[1 : 1+idx]
	if isVersionSeg(seg) {
		return path[1+idx:]
	}
	return path
}

// startProxy starts a containerproxy pointed at the fake daemon.
func startProxy(t *testing.T, d *fakeDaemon) *Proxy {
	t.Helper()
	p, err := New(Config{
		Upstream:       d.server.URL,
		ApprovedImages: []string{"pgvector/pgvector:pg16"},
		ExecutorID:     "exec-1",
		Auditor:        audit.Nop(),
		Logf:           func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.shutdown)
	return p
}

// doReq issues a request to the proxy and returns the response status,
// body, and a parsed omac message (when present).
func doReq(t *testing.T, p *Proxy, method, path string, body []byte, hdr http.Header) (int, string, string) {
	t.Helper()
	conn, err := net.Dial("tcp", p.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req, err := http.NewRequest(method, "http://127.0.0.1"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if hdr != nil {
		for k, vs := range hdr {
			for _, v := range vs {
				req.Header.Set(k, v)
			}
		}
	}
	if err := req.Write(conn); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var omacMsg string
	var parsed map[string]any
	if json.Unmarshal(b, &parsed) == nil {
		if v, ok := parsed["omac"].(string); ok {
			omacMsg = v
		}
	}
	return resp.StatusCode, string(b), omacMsg
}

// --- allowlist tests -----------------------------------------------------

func TestAllowlist_PingVersionInfo(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	for _, path := range []string{"/_ping", "/v1.44/version", "/v1.44/info"} {
		status, _, _ := doReq(t, p, http.MethodGet, path, nil, nil)
		if status != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, status)
		}
	}
}

func TestAllowlist_UnknownEndpointDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, body, omac := doReq(t, p, http.MethodPost, "/v1.44/build", []byte(`{}`), nil)
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body=%q", status, body)
	}
	if omac == "" {
		t.Errorf("denial must include omac message field: %q", body)
	}
	if !strings.Contains(omac, "unknown Docker API endpoint") {
		t.Errorf("denial must say unknown endpoint: %q", omac)
	}
	// Prune endpoints denied.
	for _, path := range []string{"/v1.44/images/prune", "/v1.44/networks/prune", "/v1.44/volumes/prune", "/v1.44/containers/prune"} {
		s, _, o := doReq(t, p, http.MethodPost, path, nil, nil)
		if s != http.StatusForbidden || !strings.Contains(o, "unknown") {
			t.Errorf("%s: expected structured unknown-endpoint denial, got %d %q", path, s, o)
		}
	}
	// /exec, /build, /commit, /attach, /archive denied.
	for _, path := range []string{"/v1.44/exec/abc/start", "/v1.44/commit", "/v1.44/containers/abc/attach", "/v1.44/containers/abc/archive"} {
		s, _, _ := doReq(t, p, http.MethodPost, path, nil, nil)
		if s != http.StatusForbidden {
			t.Errorf("%s: expected 403, got %d", path, s)
		}
	}
	// swarm/node/service denied.
	for _, path := range []string{"/v1.44/swarm", "/v1.44/nodes", "/v1.44/services"} {
		s, _, _ := doReq(t, p, http.MethodGet, path, nil, nil)
		if s != http.StatusForbidden {
			t.Errorf("%s: expected 403, got %d", path, s)
		}
	}
}

// --- create-body validation tests ----------------------------------------

func validCreateBody() string {
	return `{"Image":"pgvector/pgvector:pg16","Labels":{},"Env":["POSTGRES_PASSWORD=hush"],"HostConfig":{"Privileged":false,"Binds":[],"Mounts":[],"NetworkMode":"","PidMode":"","IpcMode":"","UsernsMode":"","CgroupnsMode":"","Runtime":"","CapAdd":[],"Devices":[],"SecurityOpt":[],"Dns":[],"ExtraHosts":[],"CgroupParent":"","PortBindings":{"5432/tcp":[{"HostIp":"","HostPort":""}]}}}`
}

// waitForCall polls the fake daemon's recorded calls until one matches the
// predicate or the timeout elapses. The container proxy does post-response
// work (inspect, network attach) AFTER writing the response to the client,
// so a test that acts on the response must wait for the side effects.
func waitForCall(t *testing.T, d *fakeDaemon, pred func(recordedReq) bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range d.calls {
			if pred(c) {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestCreateBody_ApprovedImageForwardsWithRewrite(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(validCreateBody()), nil)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201", status)
	}
	// Find the recorded create body.
	var rec recordedReq
	for _, c := range d.calls {
		if c.Path == "/containers/create" {
			rec = c
		}
	}
	if rec.Method == "" {
		t.Fatal("create not forwarded to upstream")
	}
	// PortBindings HostIp rewritten to 127.0.0.1.
	if !strings.Contains(rec.Body, `"HostIp":"127.0.0.1"`) {
		t.Errorf("HostIp not rewritten to 127.0.0.1:\n%s", rec.Body)
	}
	// Ownership label injected.
	if !strings.Contains(rec.Body, `"omac.executor":"exec-1"`) {
		t.Errorf("ownership label not injected:\n%s", rec.Body)
	}
	// Env values are NOT present in audit; here we only check they pass
	// through to the daemon (they are ephemeral test creds).
}

func TestCreateBody_UnapprovedImageDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), "pgvector/pgvector:pg16", "postgres:17")
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "denied container image") || !strings.Contains(omac, "postgres:17") {
		t.Errorf("denial must name unapproved image: %q", omac)
	}
	if !strings.Contains(omac, "do not retry") {
		t.Errorf("denial must state do not retry: %q", omac)
	}
}

func TestCreateBody_PrivilegedDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"Privileged":false`, `"Privileged":true`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "privileged mode") {
		t.Errorf("denial must state privileged mode forbidden: %q", omac)
	}
}

func TestCreateBody_BindMountDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"Binds":[]`, `"Binds":["/var/run/docker.sock:/var/run/docker.sock:rw"]`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "bind mount") {
		t.Errorf("denial must state bind mount forbidden: %q", omac)
	}
}

func TestCreateBody_RyukImageDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), "pgvector/pgvector:pg16", "testcontainers/ryuk:0.12.0")
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "Ryuk") {
		t.Errorf("denial must mention Ryuk: %q", omac)
	}
}

func TestCreateBody_ReservedLabelDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"Labels":{}`, `"Labels":{"omac.executor":"forged"}`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "reserved") {
		t.Errorf("denial must state reserved label: %q", omac)
	}
}

func TestCreateBody_NetworkNamespaceDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"NetworkMode":""`, `"NetworkMode":"host"`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "namespace") {
		t.Errorf("denial must state namespace forbidden: %q", omac)
	}
}

func TestCreateBody_CapAddDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := strings.ReplaceAll(validCreateBody(), `"CapAdd":[]`, `"CapAdd":["NET_ADMIN"]`)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "capabilities") || !strings.Contains(omac, "forbidden") {
		t.Errorf("denial must state capabilities forbidden: %q", omac)
	}
}

// --- images/create tests -------------------------------------------------

func TestImagesCreate_ApprovedFromImageForwards(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/images/create?fromImage=pgvector/pgvector&tag=pg16", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

func TestImagesCreate_UnapprovedFromImageDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/images/create?fromImage=evil/image&tag=latest", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "evil/image") {
		t.Errorf("denial must name the unapproved image: %q", omac)
	}
}

func TestImagesCreate_RegistryAuthDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	hdr := http.Header{}
	hdr.Set("X-Registry-Auth", "dXNlcjpwYXNz")
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/images/create?fromImage=pgvector/pgvector&tag=pg16", nil, hdr)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "X-Registry-Auth") {
		t.Errorf("denial must mention X-Registry-Auth: %q", omac)
	}
}

// TestCreate_RegistryAuthStripped asserts the credential header
// X-Registry-Auth is NOT forwarded to the daemon on /containers/create
// (review critical #2: the create path forwarded it verbatim via
// copyForwardHeaders, bypassing the filter that images/create enforces).
// The header is stripped by copyForwardHeaders; the create must still
// succeed (the header's absence does not block an approved-image create).
func TestCreate_RegistryAuthStripped(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	hdr := http.Header{}
	hdr.Set("X-Registry-Auth", "dXNlcjpwYXNz")
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(validCreateBody()), hdr)
	if status != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (approved image; auth header stripped)", status)
	}
	// Wait for the create to be recorded (the handler sets sawAuthHeader).
	waitForCall(t, d, func(c recordedReq) bool {
		return c.Method == http.MethodPost && c.Path == "/containers/create"
	}, "containers/create")
	if d.sawAuthHeader {
		t.Errorf("X-Registry-Auth was forwarded to the daemon on create; it must be stripped by copyForwardHeaders")
	}
}

// TestCreateBody_UnknownHostConfigFieldDenied asserts the create-body
// validation is ALLOWLIST-based (spec.md:222: unknown security-relevant
// fields denied), not denylist-based. An unknown HostConfig field must
// be denied fail-closed so a future Docker API field cannot pass through
// unexamined (review major #3).
func TestCreateBody_UnknownHostConfigFieldDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	// A field NOT in the v1 allowedHostConfigKeys set.
	body := `{"Image":"pgvector/pgvector:pg16","HostConfig":{"SomeNewDangerousField":"evil"}}`
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (unknown HostConfig field must be denied fail-closed)", status)
	}
	if !strings.Contains(omac, "SomeNewDangerousField") {
		t.Errorf("denial must name the unknown field: %q", omac)
	}
}

// TestCreateBody_AutoRemoveDenied asserts AutoRemove=true is denied: a
// container that auto-removes on exit evades the proxy's ownership
// tracking and the cleanup/audit path (review major #3).
func TestCreateBody_AutoRemoveDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	body := `{"Image":"pgvector/pgvector:pg16","HostConfig":{"AutoRemove":true}}`
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(body), nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (AutoRemove must be denied)", status)
	}
	if !strings.Contains(omac, "AutoRemove") {
		t.Errorf("denial must mention AutoRemove: %q", omac)
	}
}

// --- image inspect -------------------------------------------------------

func TestImageInspect_ApprovedRefForwards(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, _, _ := doReq(t, p, http.MethodGet, "/v1.44/images/pgvector/pgvector:pg16/json", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

func TestImageInspect_UnapprovedRefDenied(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	status, _, omac := doReq(t, p, http.MethodGet, "/v1.44/images/evil:latest/json", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "evil:latest") {
		t.Errorf("denial must name the image: %q", omac)
	}
}

// --- ownership enforcement -----------------------------------------------

func TestOwnership_NotOwnedDenied(t *testing.T) {
	d := newFakeDaemon(t)
	// Inspect returns a DIFFERENT executor label.
	d.inspectResponse = `{"Id":"xyz","Config":{"Image":"pgvector/pgvector:pg16"},"Labels":{"omac.executor":"other-executor"}}`
	p := startProxy(t, d)
	status, _, omac := doReq(t, p, http.MethodPost, "/v1.44/containers/xyz/start", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status)
	}
	if !strings.Contains(omac, "not owned by this executor") {
		t.Errorf("denial must state not owned: %q", omac)
	}
}

func TestOwnership_OwnedForwards(t *testing.T) {
	d := newFakeDaemon(t)
	// Real Docker nests labels at Config.Labels (NOT top-level Labels).
	// This is the shape a real daemon returns from GET /containers/{id}/json;
	// the proxy MUST parse Config.Labels, not top-level Labels (review
	// critical #1: the previous fixture used top-level Labels and hid the
	// parse bug).
	d.inspectResponse = `{"Id":"abc123","Config":{"Image":"pgvector/pgvector:pg16","Labels":{"omac.executor":"exec-1"}}}`
	p := startProxy(t, d)
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/abc123/start", nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
}

// TestOwnership_ConfigLabelsNotTopLevel asserts the ownership parse reads
// Config.Labels (the real Docker inspect shape) and that a container
// whose labels are ONLY at top-level Labels (a non-real shape) is denied
// fail-closed — guarding against a regression of the critical parse bug.
func TestOwnership_ConfigLabelsNotTopLevel(t *testing.T) {
	d := newFakeDaemon(t)
	// Labels at top-level ONLY (the shape the buggy parser read) — a real
	// daemon does NOT return this. The proxy must NOT treat this as owned.
	d.inspectResponse = `{"Id":"abc123","Labels":{"omac.executor":"exec-1"}}`
	p := startProxy(t, d)
	status, _, body := doReq(t, p, http.MethodGet, "/v1.44/containers/abc123/json", nil, nil)
	if status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (top-level Labels must not satisfy ownership; real daemon nests at Config.Labels): body=%s", status, body)
	}
}

// --- containers/json filter rewrite -------------------------------------

func TestContainersList_FilterRewritten(t *testing.T) {
	d := newFakeDaemon(t)
	p := startProxy(t, d)
	// Client sends a forgeable label filter.
	status, _, _ := doReq(t, p, http.MethodGet, `/v1.44/containers/json?all=true&filters=%7B%22label%22%3A%5B%22org.testcontainers%3Dtrue%22%5D%7D`, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	// Find the recorded request and verify the filter was rewritten.
	var rec recordedReq
	for _, c := range d.calls {
		if c.Path == "/containers/json" {
			rec = c
		}
	}
	if rec.Method == "" {
		t.Fatal("containers/json not forwarded")
	}
	if !strings.Contains(rec.Query, "omac.executor%3Dexec-1") {
		t.Errorf("ownership label not injected into filters: %q", rec.Query)
	}
	// The forgeable client label must not survive (the proxy strips it;
	// the injected ownership label replaces the label set).
	if strings.Contains(rec.Query, "org.testcontainers") {
		t.Errorf("client label filter must be stripped, not trusted: %q", rec.Query)
	}
}

// --- cleanup -------------------------------------------------------------

func TestCleanup_RemovesOwnedContainersAndNetwork(t *testing.T) {
	d := newFakeDaemon(t)
	d.networkCreateResponse = `{"Id":"net-xyz"}`
	p := startProxy(t, d)
	// Create a container so it is tracked.
	status, _, _ := doReq(t, p, http.MethodPost, "/v1.44/containers/create", []byte(validCreateBody()), nil)
	if status != http.StatusCreated {
		t.Fatalf("create status = %d", status)
	}
	// Wait for the proxy's post-response work (network create + attach)
	// to complete before Cleanup runs; otherwise Cleanup races the
	// attachToNetwork goroutine.
	waitForCall(t, d, func(c recordedReq) bool {
		return c.Method == http.MethodPost && c.Path == "/networks/create"
	}, "networks/create")
	// Trigger cleanup.
	p.Cleanup()
	// Assert a DELETE for abc123 reached the daemon.
	foundDelete := false
	foundNetRemove := false
	for _, c := range d.calls {
		if c.Method == http.MethodDelete && strings.Contains(c.Path, "/containers/abc123") {
			foundDelete = true
		}
		if c.Method == http.MethodDelete && strings.Contains(c.Path, "/networks/") {
			foundNetRemove = true
		}
	}
	if !foundDelete {
		t.Error("cleanup did not DELETE the owned container")
	}
	if !foundNetRemove {
		t.Error("cleanup did not DELETE the executor network")
	}
}

// --- config validation ---------------------------------------------------

func TestNew_RequiresExecutorIDAndImages(t *testing.T) {
	if _, err := New(Config{ApprovedImages: []string{"x"}}); err == nil {
		t.Error("empty executor id must error")
	}
	if _, err := New(Config{ExecutorID: "x"}); err == nil {
		t.Error("empty approved images must error")
	}
}

// --- ContainerPolicyError.Render -----------------------------------------

func TestContainerPolicyError_Render(t *testing.T) {
	cases := []struct {
		kind PolicyErrKind
		want []string
	}{
		{KindUnapprovedImage, []string{"denied container image", "do not retry"}},
		{KindPrivilegedForbidden, []string{"privileged mode", "forbidden"}},
		{KindBindMountForbidden, []string{"bind mount", "forbidden"}},
		{KindHostNamespaceForbidden, []string{"namespace", "forbidden"}},
		{KindDeviceForbidden, []string{"forbidden"}},
		{KindUnknownEndpoint, []string{"unknown Docker API endpoint"}},
		{KindNotOwnedByExecutor, []string{"not owned by this executor"}},
		{KindRyukForbidden, []string{"Ryuk"}},
		{KindRegistryAuthForbidden, []string{"X-Registry-Auth"}},
		{KindReservedLabel, []string{"reserved"}},
	}
	for _, c := range cases {
		e := &ContainerPolicyError{Kind: c.kind, Image: "test:1", ContainerID: "cid", Reason: "r"}
		msg := e.Render()
		for _, w := range c.want {
			if !strings.Contains(msg, w) {
				t.Errorf("kind %d render missing %q: %s", c.kind, w, msg)
			}
		}
	}
}
