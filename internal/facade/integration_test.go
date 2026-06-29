package facade_test

// End-to-end integration test:
//
//     [Go client] ---unix---> [facade (this process)] ---tcp---> [python sidecar subprocess]
//
// Exactly the wiring `omac start` uses, minus the sandbox step.
// Skips cleanly when the runtime blocks loopback TCP or Unix-socket connect.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tngtech/oh-my-agentic-coder/internal/facade"
)

const sidecarPython = `
import os, json, hashlib, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

# Bind an ephemeral loopback port chosen by the kernel, then print it
# for the parent (this test) to read. Letting Python claim its own port
# removes the reuse race that existed when Go pre-allocated a port,
# closed its listener, and handed the number to Python — on a busy CI
# runner that port could be taken in the gap, Python would exit with
# EADDRINUSE, and (because stderr was discarded) the test only saw
# "sidecar never became healthy" 30s later.
PORT = int(os.environ.get("SIDECAR_PORT", "0"))
SECRET = os.environ.get("ECHO_API_KEY", "")

def fp(s):
    if not s: return "<absent>"
    return "sha256:" + hashlib.sha256(s.encode()).hexdigest()[:12]

class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def _j(self, code, body):
        raw = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)
    def do_GET(self):
        if self.path == "/status":
            self._j(200, {"ok": True})
        elif self.path == "/whoami":
            self._j(200, {"secret_fingerprint": fp(SECRET), "secret_present": bool(SECRET)})
        elif self.path.startswith("/tick"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.send_header("Cache-Control", "no-cache")
            self.send_header("Connection", "close")
            self.end_headers()
            self.close_connection = True
            for i in range(1, 6):
                self.wfile.write(
                    ("event: tick\nid: %d\ndata: " % i).encode()
                    + json.dumps({"n": i, "secret_fingerprint": fp(SECRET)}).encode()
                    + b"\n\n"
                )
                self.wfile.flush()
                time.sleep(0.03)
            self.wfile.write(b"event: done\ndata: {}\n\n")
            self.wfile.flush()
        else:
            self._j(404, {"error": "not found"})
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0") or "0")
        raw = self.rfile.read(n) if n else b""
        body = json.loads(raw.decode() or "{}")
        self._j(200, {"echoed": body, "secret_fingerprint": fp(SECRET)})

srv = ThreadingHTTPServer(("127.0.0.1", PORT), H)
# Report the kernel-assigned port to the parent. Flush so the parent
# can read it before we enter serve_forever().
sys.stdout.write("PORT=%d\n" % srv.server_address[1])
sys.stdout.flush()
srv.serve_forever()
`

// TestEchoRestEndToEnd spawns a real Python sidecar, mounts it behind the
// facade, and proves that Unix-socket requests reach the sidecar, that
// the secret was injected via env, and that POST bodies round-trip.
func TestEchoRestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}

	// Warm the Python interpreter. The first exec of python3 on a fresh
	// macOS GitHub runner can take well over 30s: the kernel validates
	// the binary's code signature / notarization ticket on first launch,
	// and on a loaded ARM64 runner that gate is slow. Pay that cost once
	// here, up front, so the sidecar spawn below starts fast and the
	// port-read deadline is measured against a warm interpreter.
	if out, err := exec.Command("python3", "-c", "import sys; print(sys.version)").CombinedOutput(); err != nil {
		t.Skipf("python3 not runnable: %v (%s)", err, strings.TrimSpace(string(out)))
	} else {
		t.Logf("python3: %s", strings.TrimSpace(string(out)))
	}

	// Can we listen + dial on loopback at all?
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("tcp listen denied:", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Close()
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
	if err == nil {
		c.Close()
	} else if !strings.Contains(err.Error(), "refused") {
		t.Skip("tcp connect denied:", err)
	}

	// Can we use a unix socket here?
	workDir, err := os.MkdirTemp(".", "omac-itest-")
	if err != nil {
		t.Skip("mkdir:", err)
	}
	t.Cleanup(func() { os.RemoveAll(workDir) })
	probeSock := filepath.Join(workDir, "probe.sock")
	pl, err := net.Listen("unix", probeSock)
	if err != nil {
		t.Skip("unix listen denied:", err)
	}
	if c, err := net.Dial("unix", probeSock); err != nil {
		pl.Close()
		t.Skip("unix dial denied:", err)
	} else {
		c.Close()
	}
	pl.Close()

	// Write the sidecar script to the working dir and start Python.
	script := filepath.Join(workDir, "sidecar.py")
	if err := os.WriteFile(script, []byte(sidecarPython), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := exec.CommandContext(ctx, "python3", script)
	cmd.Env = append(os.Environ(),
		"ECHO_API_KEY=super-secret-demo-key",
	)
	// The sidecar prints "PORT=<n>\n" on its stdout once it has bound.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	// Capture stderr so a sidecar crash is diagnosable instead of the
	// old "never became healthy" dead-end. io.Discard here made every
	// macOS CI failure invisible.
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("spawn sidecar: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	// Read the port the kernel assigned to the sidecar. The sidecar
	// binds 127.0.0.1:0 and prints the real port; we must not
	// pre-allocate one (see script comment). A genuinely cold Python
	// first-exec on a loaded macOS runner can take a while (signature
	// validation), so the deadline is generous; the warm-up above makes
	// it virtually never the bottleneck.
	type portResult struct {
		port int
		err  error
	}
	prCh := make(chan portResult, 1)
	go func() {
		r := bufio.NewReader(stdout)
		line, err := r.ReadString('\n')
		if err != nil {
			prCh <- portResult{0, fmt.Errorf("read port line: %w (stderr: %s)", err, stderr.String())}
			return
		}
		line = strings.TrimSpace(line)
		var p int
		if _, err := fmt.Sscanf(line, "PORT=%d", &p); err != nil {
			prCh <- portResult{0, fmt.Errorf("parse %q: %w (stderr: %s)", line, err, stderr.String())}
			return
		}
		prCh <- portResult{p, nil}
	}()

	var sidecarPort int
	select {
	case pr := <-prCh:
		if pr.err != nil {
			t.Fatalf("sidecar did not report its port: %v", pr.err)
		}
		sidecarPort = pr.port
	case <-time.After(120 * time.Second):
		t.Fatalf("sidecar never bound a port within 120s (stderr: %s)", stderr.String())
	}

	// Wait for /status to answer 2xx. A cold Python interpreter on a
	// loaded CI runner (notably macOS) can take a while to start
	// serving; the previous tight bound flaked as "sidecar never
	// became healthy" even though the sidecar was merely slow.
	sidecarURL := fmt.Sprintf("http://127.0.0.1:%d/status", sidecarPort)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(sidecarURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				goto ready
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("sidecar never became healthy (stderr: %s)", stderr.String())

ready:
	// Start the facade on a Unix socket routing /echo/* → sidecarPort.
	socket := filepath.Join(workDir, "b.sock")
	f := facade.New(socket, "",
		[]facade.Route{{Mount: "echo", UpstreamPort: sidecarPort, Skill: "echo-rest"}},
		1<<20,
		30*time.Second,
		"", "itest")
	if err := f.Start(ctx); err != nil {
		t.Fatalf("facade start: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	// HTTP client over the Unix socket.
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socket)
			},
		},
		Timeout: 3 * time.Second,
	}

	// 1. Status route routes through.
	resp, err := client.Get("http://x/echo/status")
	if err != nil {
		t.Fatalf("get status: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var sj map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&sj); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	resp.Body.Close()
	if sj["ok"] != true {
		t.Fatalf("status body = %+v", sj)
	}

	// 2. /whoami proves the secret made it into the sidecar's env.
	resp, err = client.Get("http://x/echo/whoami")
	if err != nil {
		t.Fatalf("get whoami: %v", err)
	}
	var wj map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&wj)
	resp.Body.Close()
	if wj["secret_present"] != true {
		t.Fatalf("secret was NOT injected; body = %+v", wj)
	}
	if fp, _ := wj["secret_fingerprint"].(string); !strings.HasPrefix(fp, "sha256:") {
		t.Fatalf("bad fingerprint: %+v", wj)
	}

	// 3. POST /echo round-trips a JSON body.
	resp, err = client.Post("http://x/echo/echo", "application/json",
		strings.NewReader(`{"hello":"world","n":7}`))
	if err != nil {
		t.Fatalf("post echo: %v", err)
	}
	var ej map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&ej)
	resp.Body.Close()
	echoed, _ := ej["echoed"].(map[string]any)
	if echoed["hello"] != "world" || echoed["n"].(float64) != 7 {
		t.Fatalf("echo mismatch: %+v", ej)
	}

	// 4. Unknown mount → 404 from facade.
	resp, err = client.Get("http://x/nope/whatever")
	if err != nil {
		t.Fatalf("get nope: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown mount status = %d", resp.StatusCode)
	}

	// 5. Facade status endpoint.
	resp, err = client.Get("http://x/")
	if err != nil {
		t.Fatalf("get /: %v", err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"echo"`) {
		t.Fatalf("facade status missing mount: %s", b)
	}

	// 6. SSE through the facade: five "tick" frames with a 30ms gap, then
	// "done". We assert the stream is *streamed* (span between first and
	// last tick > 60ms), the Content-Type is right, and the fingerprint
	// of the injected secret is present in the payload.
	sseClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", socket)
			},
			ResponseHeaderTimeout: 2 * time.Second,
		},
	}
	sseCtx, sseCancel := context.WithTimeout(ctx, 5*time.Second)
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
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	var (
		tickCount   int
		doneSeen    bool
		firstTickAt time.Time
		lastTickAt  time.Time
		currEvent   string
		fpSeen      bool
	)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			currEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			payload := strings.TrimPrefix(line, "data: ")
			if currEvent == "tick" {
				tickCount++
				now := time.Now()
				if firstTickAt.IsZero() {
					firstTickAt = now
				}
				lastTickAt = now
				// Python's json.dumps emits a space between key and value
				// by default ("a": "b"), Go's encoding/json does not.
				// Accept either to keep the assertion robust against
				// upstream serialization choices.
				if strings.Contains(payload, "secret_fingerprint") &&
					strings.Contains(payload, "sha256:") {
					fpSeen = true
				}
			} else if currEvent == "done" {
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
	if !fpSeen {
		t.Fatalf("sse payload did not include the secret fingerprint (injection broken?)")
	}
	span := lastTickAt.Sub(firstTickAt)
	total := lastTickAt.Sub(tStart)
	if span < 60*time.Millisecond {
		t.Fatalf("sse frames arrived too close together (span=%s total=%s); facade may be buffering",
			span, total)
	}
}
