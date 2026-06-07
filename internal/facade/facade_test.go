package facade

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// requireUnixSocket skips the test when AF_UNIX listen/dial is not permitted
// in the current environment (e.g. some sandboxes), mirroring the inline
// probe the older integration tests do.
func requireUnixSocket(t *testing.T) {
	t.Helper()
	probeDir, err := os.MkdirTemp(".", "omac-probe-")
	if err != nil {
		t.Skip("mkdir temp:", err)
	}
	defer os.RemoveAll(probeDir)
	ps := filepath.Join(probeDir, "p.sock")
	pl, err := net.Listen("unix", ps)
	if err != nil {
		t.Skip("unix listen not permitted:", err)
	}
	c, err := net.Dial("unix", ps)
	if err != nil {
		pl.Close()
		t.Skip("unix dial not permitted:", err)
	}
	c.Close()
	pl.Close()
}

func TestFacadePlainHTTPAndSSE(t *testing.T) {
	// Probe whether unix-socket connect is permitted at all.
	probeDir, err := os.MkdirTemp(".", "omac-probe-")
	if err != nil {
		t.Skip("mkdir temp:", err)
	}
	defer os.RemoveAll(probeDir)
	probeSock := filepath.Join(probeDir, "p.sock")
	pl, err := net.Listen("unix", probeSock)
	if err != nil {
		t.Skip("unix listen not permitted in this environment:", err)
	}
	if c, err := net.Dial("unix", probeSock); err != nil {
		pl.Close()
		t.Skip("unix dial not permitted in this environment:", err)
	} else {
		c.Close()
	}
	pl.Close()

	// 1. Upstream sidecar: /status (200), /sse (streaming).
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "data: %d\n\n", i)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	// 2. Build the facade on a temp unix socket.
	// Prefer a socket under the working directory so sandboxed test environments
	// that forbid cross-dir socket access still work.
	dir, err := os.MkdirTemp(".", "omac-test-")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	// Unix socket paths have a length limit (~104 on darwin); keep it short.
	socket := filepath.Join(dir, "b.sock")
	f := New(socket, "",
		[]Route{{Mount: "demo", UpstreamPort: port}},
		1024*1024,
		30*time.Second,
		"", "test")
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	defer f.Close()

	client := unixClient(socket)

	// 3. Plain GET.
	resp, err := client.Get("http://x/demo/status")
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "ok" {
		t.Fatalf("status body = %q, want ok", body)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status code = %d", resp.StatusCode)
	}

	// 4. Unknown mount → 404.
	resp, err = client.Get("http://x/unknown/status")
	if err != nil {
		t.Fatalf("get unknown: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown mount status = %d, want 404", resp.StatusCode)
	}

	// 5. SSE: read first frame.
	resp, err = client.Get("http://x/demo/sse")
	if err != nil {
		t.Fatalf("get sse: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("sse content-type = %q", ct)
	}
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	if n == 0 || !strings.Contains(string(buf[:n]), "data:") {
		t.Fatalf("sse first read = %q", buf[:n])
	}

	// 6. Status endpoint.
	resp, err = client.Get("http://x/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"skills"`) {
		t.Fatalf("status body = %q", body)
	}
}

// TestFacadeEchoLikeRest stands in for the "real" echo-rest sidecar: it
// exercises exactly the request shapes the demo-client.sh script would
// issue (GET /status, GET /whoami, POST /echo, GET /, GET /unknown).
//
// The upstream is an in-process httptest server — the same transport-level
// round-trip omac performs against a real Python subprocess at runtime.
func TestFacadeEchoLikeRest(t *testing.T) {
	probeDir, err := os.MkdirTemp(".", "omac-probe-")
	if err != nil {
		t.Skip("mkdir temp:", err)
	}
	defer os.RemoveAll(probeDir)
	ps := filepath.Join(probeDir, "p.sock")
	pl, err := net.Listen("unix", ps)
	if err != nil {
		t.Skip("unix listen not permitted:", err)
	}
	if c, err := net.Dial("unix", ps); err != nil {
		pl.Close()
		t.Skip("unix dial not permitted:", err)
	} else {
		c.Close()
	}
	pl.Close()

	// Fake "echo-rest" upstream.
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true,"skill":"echo-rest"}`))
	})
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		// Pretend the secret was injected into the sidecar's env; the facade
		// should have stripped the /echo prefix before we see /whoami here.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"skill":"echo-rest","secret_present":true,"secret_fingerprint":"sha256:abcdef012345","fwd_prefix":%q}`,
			r.Header.Get("X-Forwarded-Prefix"))
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"echoed":%s}`, body)
	})
	// SSE: stream 5 frames with a 30ms gap. Used to prove the facade is
	// genuinely streaming (no per-response buffering, per-frame flush).
	mux.HandleFunc("/tick", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(200)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream ResponseWriter does not implement Flusher")
			return
		}
		for i := 1; i <= 5; i++ {
			fmt.Fprintf(w, "event: tick\nid: %d\ndata: {\"n\":%d}\n\n", i, i)
			fl.Flush()
			time.Sleep(30 * time.Millisecond)
		}
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
		fl.Flush()
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	var port int
	fmt.Sscanf(portStr, "%d", &port)

	// Bring up the facade.
	dir, err := os.MkdirTemp(".", "omac-itest-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "b.sock")

	f := New(socket, "",
		[]Route{{Mount: "echo", UpstreamPort: port, Skill: "echo-rest"}},
		1<<20, 30*time.Second, "", "test")
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	defer f.Close()

	client := unixClient(socket)

	// GET /echo/status
	resp, err := client.Get("http://x/echo/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"ok":true`) {
		t.Fatalf("status body = %s", b)
	}

	// GET /echo/whoami → X-Forwarded-Prefix injected by the facade.
	resp, err = client.Get("http://x/echo/whoami")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"fwd_prefix":"/echo"`) {
		t.Fatalf("X-Forwarded-Prefix not propagated; body = %s", b)
	}
	if !strings.Contains(string(b), `"secret_present":true`) {
		t.Fatalf("whoami body = %s", b)
	}

	// POST /echo/echo with JSON body round-trips.
	resp, err = client.Post("http://x/echo/echo", "application/json",
		strings.NewReader(`{"hello":"world","n":7}`))
	if err != nil {
		t.Fatalf("post echo: %v", err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"hello":"world"`) {
		t.Fatalf("echo round-trip failed: %s", b)
	}

	// Facade self-status.
	resp, err = client.Get("http://x/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"echo"`) {
		t.Fatalf("status body missing mount: %s", b)
	}

	// Unknown mount 404.
	resp, err = client.Get("http://x/nope/x")
	if err != nil {
		t.Fatalf("nope: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}

	// SSE through the facade: assert content-type, event count, and that the
	// frames do NOT all arrive at t=0 (i.e., the facade is streaming rather
	// than buffering the whole response).
	//
	// A dedicated client with no overall Timeout is required; otherwise the
	// 2-second default in unixClient() would cut the stream short.
	sseClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socket)
			},
			// With a finite ResponseHeaderTimeout, headers must arrive promptly;
			// body reads are then bounded only by our own context / Deadline below.
			ResponseHeaderTimeout: 2 * time.Second,
		},
	}
	sseCtx, sseCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer sseCancel()
	req, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, "http://x/echo/tick", nil)
	req.Header.Set("Accept", "text/event-stream")
	tStart := time.Now()
	resp, err = sseClient.Do(req)
	if err != nil {
		t.Fatalf("sse get: %v", err)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("sse content-type = %q", ct)
	}
	if ab := resp.Header.Get("X-Accel-Buffering"); ab != "no" {
		t.Fatalf("sse X-Accel-Buffering = %q, want no (facade must disable buffering)", ab)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	var (
		tickCount   int
		doneSeen    bool
		firstTickAt time.Time
		lastTickAt  time.Time
		currEvent   string
	)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			currEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			switch currEvent {
			case "tick":
				tickCount++
				now := time.Now()
				if firstTickAt.IsZero() {
					firstTickAt = now
				}
				lastTickAt = now
			case "done":
				doneSeen = true
			}
		case line == "":
			currEvent = ""
		}
		if doneSeen {
			break
		}
	}
	resp.Body.Close()
	if err := scanner.Err(); err != nil {
		t.Fatalf("sse scan: %v", err)
	}
	if tickCount != 5 {
		t.Fatalf("sse tick count = %d, want 5", tickCount)
	}
	if !doneSeen {
		t.Fatalf("sse done event missing")
	}
	// With 5 ticks and a 30ms gap, the *span* between first and last frame
	// should be at least ~100ms. If they all landed at t=0, the facade is
	// buffering — which would be a bug.
	span := lastTickAt.Sub(firstTickAt)
	total := lastTickAt.Sub(tStart)
	if span < 60*time.Millisecond {
		t.Fatalf("sse frames arrived too close together (span=%s total=%s); facade may be buffering",
			span, total)
	}
}

func TestSplitMount(t *testing.T) {
	cases := []struct{ in, mount, rest string }{
		{"/slack/foo/bar", "slack", "foo/bar"},
		{"/slack/", "slack", ""},
		{"/slack", "slack", ""},
		{"/", "", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		m, r := splitMount(c.in)
		if m != c.mount || r != c.rest {
			t.Errorf("splitMount(%q) = (%q, %q), want (%q, %q)", c.in, m, r, c.mount, c.rest)
		}
	}
}

// unixClient returns an *http.Client whose Transport dials the given unix socket.
func unixClient(socket string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socket)
			},
		},
		Timeout: 2 * time.Second,
	}
}

// Keep os imported in this test file for future fixture needs.
var _ = os.Getenv

func TestWriteStatusHidesPerDirTokens(t *testing.T) {
	// Mix of routes: a flat start-mode mount, a global skill, and two
	// per-directory token-namespaced skills. The status index must expose
	// only the flat + global ones, never the secret dir tokens.
	f := New("", "", []Route{
		{Mount: "echo", Namespace: "", State: RouteReady, UpstreamPort: 1},
		{Mount: "skill-marketplace", Namespace: GlobalNamespace, State: RouteReady, UpstreamPort: 2},
		{Mount: "tng-email", Namespace: "d31b174d117e4bb9739f8a96f3b0b66d", State: RouteReady, UpstreamPort: 3},
		{Mount: "apple-calendar", Namespace: "34e8502d4fbd9ae9fd054d3f89e34b61", State: RouteReady, UpstreamPort: 4},
	}, 1<<20, 0, "", "test")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	f.handle(rec, req)

	body := rec.Body.String()
	// Allowed: flat + global.
	if !strings.Contains(body, `"echo"`) {
		t.Errorf("status should list flat mount echo; body=%s", body)
	}
	if !strings.Contains(body, "__global__/skill-marketplace") {
		t.Errorf("status should list global skill; body=%s", body)
	}
	// Forbidden: the secret dir tokens and their skills must NOT leak.
	for _, leak := range []string{
		"d31b174d117e4bb9739f8a96f3b0b66d",
		"34e8502d4fbd9ae9fd054d3f89e34b61",
		"tng-email",
		"apple-calendar",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("status leaked per-directory routing info %q; body=%s", leak, body)
		}
	}
}

// TestSkillDocDiscovery covers the bridge serving <SkillDir>/SKILL.md at a
// skill's top-level URL (the empty-remainder case), and the fallback rules
// around it.
func TestSkillDocDiscovery(t *testing.T) {
	skillDir := t.TempDir()
	docBody := "# echo-rest\n\nGET /status, POST /echo, GET /whoami\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(docBody), 0o600); err != nil {
		t.Fatal(err)
	}

	// UpstreamPort points nowhere; a request that reaches the proxy would
	// fail with a 5xx, which lets us prove the doc path short-circuits
	// before any upstream dial.
	f := New("", "", []Route{
		{Mount: "echo", Namespace: GlobalNamespace, State: RouteReady, UpstreamPort: 1, SkillDir: skillDir},
	}, 1<<20, 0, "", "test")

	// The mount root, both with and without a trailing slash, serves the doc.
	for _, path := range []string{"/__global__/echo", "/__global__/echo/"} {
		rec := httptest.NewRecorder()
		f.handle(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: code = %d, want 200", path, rec.Code)
		}
		if rec.Body.String() != docBody {
			t.Errorf("%s: body = %q, want SKILL.md content", path, rec.Body.String())
		}
		if got := rec.Header().Get("X-Omac-Discovery"); got != "skill-md" {
			t.Errorf("%s: X-Omac-Discovery = %q, want skill-md", path, got)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/markdown") {
			t.Errorf("%s: Content-Type = %q, want text/markdown", path, ct)
		}
	}

	// A non-GET method at the root is NOT intercepted; it falls through to
	// the proxy (which dials the dead upstream and fails — proving no
	// interception happened).
	rec := httptest.NewRecorder()
	f.handle(rec, httptest.NewRequest("POST", "/__global__/echo", nil))
	if rec.Header().Get("X-Omac-Discovery") != "" {
		t.Errorf("POST at root should not be intercepted as discovery")
	}
}

// TestSkillDocDiscoveryFallthrough proves the discovery is a fallback: a real
// subpath is proxied untouched, and a route without a SKILL.md does not get
// the discovery treatment. It runs the real proxy over a unix socket (the
// recorder path can't exercise ReverseProxy), so it skips where AF_UNIX is
// unavailable, like the other integration tests in this file.
func TestSkillDocDiscoveryFallthrough(t *testing.T) {
	requireUnixSocket(t)

	// Fake upstream that records the path it received.
	var mu sync.Mutex
	var gotPath string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("upstream:" + r.URL.Path))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	_, portStr, _ := net.SplitHostPort(strings.TrimPrefix(upstream.URL, "http://"))
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	lastPath := func() string { mu.Lock(); defer mu.Unlock(); return gotPath }

	skillDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("doc"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	socket := filepath.Join(dir, "b.sock")
	f := New(socket, "", []Route{
		// withdoc: has SKILL.md. nodoc: no SKILL.md (empty SkillDir).
		{Mount: "withdoc", Namespace: GlobalNamespace, State: RouteReady, UpstreamPort: port, SkillDir: skillDir},
		{Mount: "nodoc", Namespace: GlobalNamespace, State: RouteReady, UpstreamPort: port},
	}, 1<<20, 2*time.Second, "", "test")
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	defer f.Close()
	client := unixClient(socket)

	// Real subpath on the doc'd skill still proxies (prefix stripped).
	resp, err := client.Get("http://x/__global__/withdoc/status")
	if err != nil {
		t.Fatalf("subpath GET: %v", err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Omac-Discovery") != "" {
		t.Errorf("subpath should not be discovery-intercepted")
	}
	if lastPath() != "/status" {
		t.Errorf("upstream got path %q, want /status", lastPath())
	}

	// Root of the skill WITHOUT a SKILL.md falls through to the proxy.
	resp, err = client.Get("http://x/__global__/nodoc")
	if err != nil {
		t.Fatalf("nodoc GET: %v", err)
	}
	resp.Body.Close()
	if resp.Header.Get("X-Omac-Discovery") != "" {
		t.Errorf("nodoc root should not be discovery-intercepted")
	}
	if lastPath() != "/" {
		t.Errorf("upstream got path %q, want / (proxied root)", lastPath())
	}
}
