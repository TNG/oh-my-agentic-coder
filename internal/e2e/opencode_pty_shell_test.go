//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	// opencodePTYPackage is installed next to, and independent of, the
	// opencode-ai pin in harnessVersions: OpenCode only reaches its shell
	// helper over a Unix socket from 2.3.0 on, and 2.x ships as @opencode/cli.
	opencodePTYPackage    = "@opencode/cli@2.3.2"
	opencodePTYMinVersion = "2.3.0"

	// The first shell call also starts OpenCode's shell helper, which can be
	// slow on a CI runner. A blocked helper stalls every call for 20 s, so
	// the later calls still catch it.
	firstShellCallLimit  = 30 * time.Second
	shellCallLimit       = 5 * time.Second
	shellCallCount       = 3
	ptyDaemonWarning     = "process daemon unavailable"
	openCodeLogProof     = "cli starting"
	opencodeServeTimeout = 90 * time.Second
)

// TestOpenCodeShellCallDoesNotStall starts the real `omac serve opencode` in
// the built-in sandbox and runs `echo hi` through OpenCode's shell API. When
// the sandbox blocks OpenCode's helper socket, a call either stalls for 20 s or
// falls back after 0.2 s with a warning in OpenCode's log, so both the
// duration and the log are checked. Model-free and secret-free.
func TestOpenCodeShellCallDoesNotStall(t *testing.T) {
	if name := os.Getenv("E2E_HARNESS"); name != "" && name != "opencode" {
		t.Skipf("E2E_HARNESS=%s: this test drives OpenCode only", name)
	}
	if nestedInOmacSandbox() {
		t.Skip("nested runs use --no-sandbox, which would not exercise the sandbox's socket rules")
	}
	if unavailable, reason := sandboxUnavailableForLaunch(); unavailable {
		t.Skipf("omac sandbox unavailable in this environment: %s", reason)
	}

	h := opencodeConfig()
	h.InstallCmd = []string{"bun", "install", "-g", opencodePTYPackage}

	home := t.TempDir()
	workdir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{".cache", ".cache/opencode", ".local/share/opencode", ".local/state/opencode/locks"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	omacBin := buildOmac(t)
	installHarness(t, h, home)
	version := harnessVersion(t, h, home)
	if !versionAtLeast(version, opencodePTYMinVersion) {
		t.Skipf("OpenCode %s does not use the shell helper socket (needs >= %s)", version, opencodePTYMinVersion)
	}
	writeSandboxProfile(t, home, h, nil)

	port := freeLoopbackPort(t)
	password := randomHex(t, 16)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, omacBin, "serve", "opencode", "--control-addr", "127.0.0.1:0", "--", "--port", strconv.Itoa(port))
	cmd.Dir = workdir
	cmd.Env = append(withHome(os.Environ(), home), "OMAC_SERVE_PASSWORD="+password)
	cmd.Stdin = strings.NewReader("")
	stdout := newCaptureFile(t, "serve-stdout.log")
	stderr := newCaptureFile(t, "serve-stderr.log")
	cmd.Stdout = stdout.file
	cmd.Stderr = stderr.file
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd) }
	if err := cmd.Start(); err != nil {
		t.Fatalf("start omac serve: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-waitCh:
		case <-time.After(serveTeardownGrace):
			t.Errorf("omac serve (pid %d) did not exit within %v of being killed", cmd.Process.Pid, serveTeardownGrace)
		}
	})

	serverLogs := func() string {
		return stdout.String() + stderr.String() + openCodeLogFiles(openCodeLogDir(home))
	}
	// Longer than firstShellCallLimit, so a slow first call fails on the limit, not on the client timeout.
	client := &http.Client{Timeout: 2 * firstShellCallLimit}
	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	if err := waitForOpenCodeHTTP(client, baseURL, password, waitCh, opencodeServeTimeout); err != nil {
		t.Fatalf("%v\nOMAC AND OPENCODE OUTPUT:\n%s", err, tailLines(serverLogs(), 60))
	}

	for call := 1; call <= shellCallCount; call++ {
		elapsed, status, body, err := postShell(client, baseURL, password, workdir)
		t.Logf("shell call %d: %v, HTTP %d", call, elapsed.Round(time.Millisecond), status)
		if err != nil {
			t.Fatalf("shell call %d: %v\nOMAC AND OPENCODE OUTPUT:\n%s", call, err, tailLines(serverLogs(), 60))
		}
		if status/100 != 2 {
			t.Fatalf("shell call %d: HTTP %d: %s", call, status, body)
		}
		limit, limitName := shellCallLimit, "shellCallLimit"
		if call == 1 {
			limit, limitName = firstShellCallLimit, "firstShellCallLimit"
		}
		if elapsed > limit {
			t.Errorf("shell call %d took %v, over %s (%v): OpenCode's shell helper is probably unreachable from the sandbox", call, elapsed, limitName, limit)
		}
	}

	logDir := openCodeLogDir(home)
	openCodeLog := openCodeLogFiles(logDir)
	if !strings.Contains(openCodeLog, openCodeLogProof) {
		t.Fatalf("could not observe OpenCode's log at %s: it is empty or lacks %q\n%s", logDir, openCodeLogProof, listDir(logDir))
	}
	if logs := stdout.String() + stderr.String() + openCodeLog; strings.Contains(logs, ptyDaemonWarning) {
		t.Errorf("OpenCode logged %q: its shell helper socket is blocked by the sandbox\n%s", ptyDaemonWarning, tailLines(logs, 40))
	}
}

func postShell(client *http.Client, baseURL, password, workdir string) (time.Duration, int, string, error) {
	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/shell", bytes.NewReader([]byte(`{"command":"echo hi"}`)))
	if err != nil {
		return 0, 0, "", err
	}
	req.SetBasicAuth("opencode", password)
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-opencode-directory", workdir)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return time.Since(start), 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return time.Since(start), resp.StatusCode, string(body), err
}

func waitForOpenCodeHTTP(client *http.Client, baseURL, password string, waitCh <-chan error, timeout time.Duration) error {
	deadline := time.After(timeout)
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		req, err := http.NewRequest(http.MethodGet, baseURL+"/", nil)
		if err != nil {
			return err
		}
		req.SetBasicAuth("opencode", password)
		if resp, err := client.Do(req); err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized {
				return fmt.Errorf("OpenCode rejects the test password")
			}
			if resp.StatusCode/100 == 2 || resp.StatusCode/100 == 3 {
				return nil
			}
		}
		select {
		case werr := <-waitCh:
			return fmt.Errorf("omac serve exited before OpenCode answered: %v", werr)
		case <-deadline:
			return fmt.Errorf("OpenCode did not answer on %s within %v", baseURL, timeout)
		case <-tick.C:
		}
	}
}

func openCodeLogDir(home string) string {
	return filepath.Join(home, ".local", "share", "opencode", "log")
}

func listDir(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Sprintf("listing %s: %v", dir, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "contents of %s:\n", dir)
	for _, e := range entries {
		fmt.Fprintf(&b, "  %s\n", e.Name())
	}
	return b.String()
}

func openCodeLogFiles(logDir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(logDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(path); err == nil {
			b.Write(data)
		}
		return nil
	})
	return b.String()
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buf)
}

// versionAtLeast compares dotted numeric versions; a non-numeric part counts as 0.
func versionAtLeast(version, minimum string) bool {
	have, want := versionParts(version), versionParts(minimum)
	for i := range want {
		if have[i] != want[i] {
			return have[i] > want[i]
		}
	}
	return true
}

func versionParts(v string) [3]int {
	var parts [3]int
	fields := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	for i := 0; i < len(fields) && i < 3; i++ {
		digits := fields[i]
		if end := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); end >= 0 {
			digits = digits[:end]
		}
		parts[i], _ = strconv.Atoi(digits)
	}
	return parts
}
