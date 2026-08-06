package buildrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// handshakeSockRoot is a SHORT parent directory for the test socket
// file, so the path stays under macOS's 104-byte SUN_LEN limit.
// t.TempDir() under the omac sandbox yields a deep
// /var/folders/.../TestFooNNN/001 path whose + "/daemon.sock" tail
// exceeds SUN_LEN (bind: invalid argument); the worktree root itself
// is even deeper. /tmp/omac-hs-test is short enough (14 bytes +
// "hs-NNN/daemon.sock" ≈ 34 bytes total) and writable. The omac
// sandbox permits AF_UNIX LISTEN there but blocks DIAL (connect:
// operation not permitted) — see requireUnixSocket, which gates the
// dial tests. Listen-only tests (Close, socket file mode, parent-must-
// exist) run without the gate.
const handshakeSockRoot = "/tmp/omac-hs-test"

var handshakeDirOnce struct {
	once    bool
	dir     string
	initErr error
}

// newHandshakeDir returns a fresh per-test subdirectory under the short
// handshakeSockRoot, so concurrent tests get distinct socket files.
func newHandshakeDir(t *testing.T) (string, error) {
	t.Helper()
	if !handshakeDirOnce.once {
		handshakeDirOnce.once = true
		if err := os.MkdirAll(handshakeSockRoot, 0o700); err != nil {
			handshakeDirOnce.initErr = fmt.Errorf("create %s: %w", handshakeSockRoot, err)
		} else {
			handshakeDirOnce.dir = handshakeSockRoot
		}
	}
	if handshakeDirOnce.initErr != nil {
		return "", handshakeDirOnce.initErr
	}
	dir, err := os.MkdirTemp(handshakeDirOnce.dir, "hs-*")
	if err != nil {
		return "", fmt.Errorf("create handshake dir under %s: %w", handshakeDirOnce.dir, err)
	}
	return dir, nil
}

// newHandshakeChannel returns a listening DaemonHandshakeChannel with
// a SHORT socket path under /tmp/omac-hs-test (so it fits macOS's
// 104-byte SUN_LEN). The omac sandbox permits AF_UNIX listen there
// but blocks dial — tests that DIAL must call requireUnixSocket first.
// The t.Cleanup closes the channel and removes the socket file + its
// parent dir so a leaked listener does not outlive the test.
func newHandshakeChannel(t *testing.T) *DaemonHandshakeChannel {
	t.Helper()
	dir, err := newHandshakeDir(t)
	if err != nil {
		t.Fatalf("newHandshakeDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "daemon.sock")
	c := NewDaemonHandshakeChannel(sockPath)
	if err := c.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// dialAndSend is the fake "daemon" goroutine: it dials the host's
// daemon-handshake Unix socket, sends a single JSON line
// {"pid":<pid>,"marker":"<marker>"}, then reads the one-byte ack (or
// EOF). It reports the ack byte (or -1 for EOF/error) and surfaces
// dial/send errors via t.Errorf (the caller treats -1 as "no ack").
// Mirrors what the OMAC-authored init script does at daemon startup
// (RenderDaemonOwnerHandshakeInitScript).
func dialAndSend(t *testing.T, sockPath string, pid int, marker string) (ack int) {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Errorf("fake daemon dial %s: %v", sockPath, err)
		return -1
	}
	defer conn.Close()
	payload, _ := json.Marshal(struct {
		PID    int    `json:"pid"`
		Marker string `json:"marker"`
	}{PID: pid, Marker: marker})
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		t.Errorf("fake daemon send: %v", err)
		return -1
	}
	buf := make([]byte, 1)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return -1
	}
	return int(buf[0])
}

// requireUnixSocket probes whether AF_UNIX listen + dial is permitted
// in the current environment, mirroring facade_test.go's
// requireUnixSocket (facade_test.go:34). The omac sandbox blocks
// AF_UNIX connect even when listen succeeds (connect: operation not
// permitted), and macOS's 104-byte SUN_LEN limit means a socket path
// under this worktree's deep /Users/.../implement-shape-a/... root
// exceeds the bind limit. Both conditions make the real-socket
// DaemonHandshakeChannel tests un-runnable here; CI (no sandbox,
// shallow checkout) runs them. Tests that only exercise Listen/Close
// (no dial) do NOT call this — Listen under /tmp succeeds in the
// sandbox; only dial is blocked — so the channel's listen + cleanup
// path is still covered.
func requireUnixSocket(t *testing.T) {
	t.Helper()
	// Probe under the same short root the dial tests use, so the probe
	// tests exactly the dial capability (the omac sandbox blocks
	// AF_UNIX connect even when listen succeeds). A deep temp dir
	// would fail bind on SUN_LEN before reaching dial, conflating the
	// two constraints; the short root isolates the dial check.
	if err := os.MkdirAll(handshakeSockRoot, 0o700); err != nil {
		skipOrFailCI(t, "mkdir %s: %v", handshakeSockRoot, err)
		return
	}
	dir, err := os.MkdirTemp(handshakeSockRoot, "probe-*")
	if err != nil {
		skipOrFailCI(t, "mkdir temp: %v", err)
		return
	}
	defer os.RemoveAll(dir)
	ps := filepath.Join(dir, "p.sock")
	pl, err := net.Listen("unix", ps)
	if err != nil {
		skipOrFailCI(t, "unix listen not permitted: %v", err)
		return
	}
	c, err := net.Dial("unix", ps)
	if err != nil {
		pl.Close()
		skipOrFailCI(t, "unix dial not permitted: %v", err)
		return
	}
	c.Close()
	pl.Close()
}

// skipOrFailCI skips the test locally (e.g. inside the omac sandbox
// where AF_UNIX dial is blocked) and fails it when running in CI
// (where the sandbox is absent and AF_UNIX must work). CI is detected
// via the GITHUB_ACTIONS env var (the repo's e2e.yml sets it). This
// mirrors facade_test.go's convention so a regression on CI is not
// hidden by a local skip.
func skipOrFailCI(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatalf(format, args...)
		return
	}
	t.Skipf(format, args...)
}

func TestNewDaemonOwnerMarker_Unguessable(t *testing.T) {
	m1, err := NewDaemonOwnerMarker()
	if err != nil {
		t.Fatalf("NewDaemonOwnerMarker: %v", err)
	}
	if len(m1) != 64 {
		t.Errorf("marker len = %d, want 64 (32 bytes hex)", len(m1))
	}
	m2, err := NewDaemonOwnerMarker()
	if err != nil {
		t.Fatalf("second NewDaemonOwnerMarker: %v", err)
	}
	if m1 == m2 {
		t.Errorf("two minted markers must differ (got identical %q)", m1)
	}
	// The marker must be hex (no whitespace, no punctuation beyond
	// [0-9a-f]); a non-hex char would break the JVM system property
	// value or the JSON encoding.
	for _, r := range m1 {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Errorf("marker %q contains non-hex char %q", m1, r)
			break
		}
	}
}

func TestRenderGradleProperties_DaemonOwnerMarker(t *testing.T) {
	// Marker + MaxHeap: deterministic order (heap first, then marker).
	s := RenderGradleProperties(GradlePropertiesConfig{
		MaxHeap:            "1g",
		DaemonOwnerMarker: "abc123",
	})
	want := "org.gradle.jvmargs=-Xmx1g -Domac.daemon.owner=abc123\n"
	if !strings.Contains(s, want) {
		t.Errorf("jvmargs line missing or wrong:\nwant: %s\ngot:\n%s", want, s)
	}
	// Marker only (no MaxHeap): jvmargs line carries just the marker.
	s2 := RenderGradleProperties(GradlePropertiesConfig{
		DaemonOwnerMarker: "abc123",
	})
	want2 := "org.gradle.jvmargs=-Domac.daemon.owner=abc123\n"
	if !strings.Contains(s2, want2) {
		t.Errorf("marker-only jvmargs line missing or wrong:\nwant: %s\ngot:\n%s", want2, s2)
	}
}

func TestRenderGradleProperties_NoMarkerUnchanged(t *testing.T) {
	// Zero DaemonOwnerMarker must produce the SAME output as before
	// the field existed (the existing control_test.go expectations
	// rely on this). MaxHeap-only line, no -Domac.daemon.owner.
	s := RenderGradleProperties(GradlePropertiesConfig{MaxHeap: "512m"})
	if strings.Contains(s, "omac.daemon.owner") {
		t.Errorf("zero marker must not emit the -Domac.daemon.owner property:\n%s", s)
	}
	if !strings.Contains(s, "org.gradle.jvmargs=-Xmx512m\n") {
		t.Errorf("heap-only line missing:\n%s", s)
	}
	// Neither MaxHeap nor marker: no jvmargs line at all.
	s2 := RenderGradleProperties(GradlePropertiesConfig{})
	if strings.Contains(s2, "org.gradle.jvmargs") {
		t.Errorf("empty cfg must not emit a jvmargs line:\n%s", s2)
	}
}

func TestRenderDaemonOwnerHandshakeInitScript_NoOpWhenNoMarker(t *testing.T) {
	s := RenderDaemonOwnerHandshakeInitScript()
	// The no-op guard: read the marker, return if absent.
	for _, want := range []string{
		"System.getProperty('omac.daemon.owner')",
		"if (omacMarker == null || omacMarker.isEmpty())",
		"return",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("handshake init script missing no-op guard %q:\n%s", want, s)
		}
	}
}

func TestRenderDaemonOwnerHandshakeInitScript_NoOpWhenNoSockFile(t *testing.T) {
	s := RenderDaemonOwnerHandshakeInitScript()
	// The no-op guard for the socket file: read
	// .omac-control/daemon-handshake-sock, return if absent/empty.
	for _, want := range []string{
		".omac-control/daemon-handshake-sock",
		"if (sockPath == null || sockPath.isEmpty())",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("handshake init script missing sock-file no-op guard %q:\n%s", want, s)
		}
	}
}

func TestRenderDaemonOwnerHandshakeInitScript_PortablePID(t *testing.T) {
	s := RenderDaemonOwnerHandshakeInitScript()
	// PID extraction must be portable back to Java 8 (ManagementFactory,
	// NOT Java 9+ ProcessHandle.current().pid()). The comment
	// legitimately MENTIONS ProcessHandle to explain why it is not
	// used; only the executable call `ProcessHandle.current().pid()`
	// is banned.
	if !strings.Contains(s, "ManagementFactory.getRuntimeMXBean().getName()") {
		t.Errorf("handshake init script must extract PID via ManagementFactory (Java 8+ portable):\n%s", s)
	}
	if strings.Contains(s, "ProcessHandle.current().pid()") {
		t.Errorf("handshake init script must NOT call Java 9+ ProcessHandle.current().pid() (daemon toolchain may be Java 8):\n%s", s)
	}
}

func TestRenderDaemonOwnerHandshakeInitScript_FailClosed(t *testing.T) {
	s := RenderDaemonOwnerHandshakeInitScript()
	// Fail-closed: a non-ack or host-close-without-ack throws a
	// GradleException so the wrapper cannot proceed unverified.
	for _, want := range []string{
		"throw new GradleException",
		"host did not acknowledge",
		"daemon handshake failed",
		// Single-byte ack (not a line): the script checks '1' as int.
		"((int) '1')",
		// Bounded timeout (30s) so a hung host cannot deadlock Gradle.
		"30000",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("handshake init script missing %q:\n%s", want, s)
		}
	}
	// Determinism: re-rendering yields identical output.
	if s2 := RenderDaemonOwnerHandshakeInitScript(); s2 != s {
		t.Errorf("handshake init script is not deterministic across renders")
	}
}

func TestRenderDaemonOwnerHandshakeInitScript_UnixDomainSocket(t *testing.T) {
	s := RenderDaemonOwnerHandshakeInitScript()
	// The script opens the Unix-domain socket via
	// java.net.UnixDomainSocketAddress (Java 16+), not a TCP Socket.
	if !strings.Contains(s, "java.net.UnixDomainSocketAddress.of(sockPath)") {
		t.Errorf("handshake init script must open the Unix-domain socket via UnixDomainSocketAddress:\n%s", s)
	}
	if strings.Contains(s, "new Socket()") {
		t.Errorf("handshake init script must NOT use a plain TCP Socket:\n%s", s)
	}
	// The JSON payload is a single line terminated by \n.
	if !strings.Contains(s, `JsonOutput.toJson([pid: pid, marker: omacMarker]) + "\n"`) {
		t.Errorf("handshake init script must emit a single-line JSON payload:\n%s", s)
	}
}

func TestPrepareControlState_WritesDaemonOwnerHandshakeInitScript(t *testing.T) {
	leaf := t.TempDir()
	chmodInitDForCleanup(t, leaf)
	paths, err := PrepareControlState(leaf, GradlePropertiesConfig{})
	if err != nil {
		t.Fatalf("PrepareControlState: %v", err)
	}
	initScript := filepath.Join(leaf, "init.d", daemonOwnerHandshakeInitName)
	data, err := os.ReadFile(initScript)
	if err != nil {
		t.Fatalf("daemon-owner-handshake init script not written (it must be unconditional): %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "omac.daemon.owner") {
		t.Errorf("handshake init script missing marker read:\n%s", body)
	}
	// The init script file is granted read-only: it appears in the
	// returned control files list AND its parent init.d dir is in
	// control dirs (read-only).
	found := false
	for _, p := range paths.Files {
		if strings.HasSuffix(p, daemonOwnerHandshakeInitName) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("handshake init script not in control files (read-only grant missing): %v", paths.Files)
	}
}

func TestPrepareControlState_WritesDaemonHandshakeSockFile(t *testing.T) {
	leaf := t.TempDir()
	chmodInitDForCleanup(t, leaf)
	wantSock := "/tmp/omac-build/req-42/daemon.sock"
	paths, err := PrepareControlState(leaf, GradlePropertiesConfig{
		DaemonHandshakeSock: wantSock,
	})
	if err != nil {
		t.Fatalf("PrepareControlState: %v", err)
	}
	sockFile := filepath.Join(leaf, controlStateName, daemonHandshakeSockName)
	got, err := os.ReadFile(sockFile)
	if err != nil {
		t.Fatalf("daemon-handshake-sock control file not written: %v", err)
	}
	if strings.TrimSpace(string(got)) != wantSock {
		t.Errorf("daemon-handshake-sock content = %q, want %q", got, wantSock)
	}
	// The file must be in the control files list (read-only grant for
	// the init script to read it).
	found := false
	for _, p := range paths.Files {
		if strings.HasSuffix(p, daemonHandshakeSockName) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("daemon-handshake-sock not in control files (read-only grant missing): %v", paths.Files)
	}
}

func TestPrepareControlState_OmitsDaemonHandshakeSockWhenEmpty(t *testing.T) {
	leaf := t.TempDir()
	chmodInitDForCleanup(t, leaf)
	if _, err := PrepareControlState(leaf, GradlePropertiesConfig{}); err != nil {
		t.Fatalf("PrepareControlState: %v", err)
	}
	sockFile := filepath.Join(leaf, controlStateName, daemonHandshakeSockName)
	if _, err := os.Stat(sockFile); !os.IsNotExist(err) {
		t.Errorf("daemon-handshake-sock file should not exist when DaemonHandshakeSock is empty: %v", err)
	}
}

// --- DaemonHandshakeChannel tests ---

func TestDaemonHandshakeChannel_HappyPath(t *testing.T) {
	requireUnixSocket(t)
	c := newHandshakeChannel(t)
	const marker = "deadbeef"
	const wantPID = 4242
	// Fake "daemon" goroutine: dial, send, read ack.
	ackCh := make(chan int, 1)
	go func() {
		ackCh <- dialAndSend(t, c.SockPath(), wantPID, marker)
	}()
	pid, err := c.AwaitHandshake(2*time.Second, marker, func(p int) (bool, error) {
		if p != wantPID {
			t.Errorf("verify seam got pid %d, want %d", p, wantPID)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("AwaitHandshake: %v", err)
	}
	if pid != wantPID {
		t.Errorf("returned pid = %d, want %d", pid, wantPID)
	}
	// The daemon received the one-byte ack ('1').
	select {
	case ack := <-ackCh:
		if ack != '1' {
			t.Errorf("daemon ack byte = %d, want %d ('1')", ack, '1')
		}
	case <-time.After(time.Second):
		t.Fatal("fake daemon did not receive ack")
	}
}

func TestDaemonHandshakeChannel_MarkerMismatch(t *testing.T) {
	requireUnixSocket(t)
	c := newHandshakeChannel(t)
	const expectedMarker = "right"
	const wrongMarker = "wrong"
	const wantPID = 99
	ackCh := make(chan int, 1)
	go func() {
		ackCh <- dialAndSend(t, c.SockPath(), wantPID, wrongMarker)
	}()
	_, err := c.AwaitHandshake(2*time.Second, expectedMarker, func(int) (bool, error) {
		t.Error("verify seam must NOT be called on marker mismatch")
		return false, nil
	})
	if !errors.Is(err, ErrHandshakeMarkerMismatch) {
		t.Errorf("error = %v, want ErrHandshakeMarkerMismatch", err)
	}
	// The daemon received NO ack (EOF / -1) — the build fails closed.
	select {
	case ack := <-ackCh:
		if ack != -1 {
			t.Errorf("daemon must NOT be acked on mismatch; got ack byte %d", ack)
		}
	case <-time.After(time.Second):
		t.Fatal("fake daemon did not observe the close")
	}
}

func TestDaemonHandshakeChannel_VerifyFalse(t *testing.T) {
	requireUnixSocket(t)
	c := newHandshakeChannel(t)
	const marker = "abc"
	ackCh := make(chan int, 1)
	go func() {
		ackCh <- dialAndSend(t, c.SockPath(), 7, marker)
	}()
	_, err := c.AwaitHandshake(2*time.Second, marker, func(int) (bool, error) {
		return false, nil // live but mismatched (PID-reused / wrong exe)
	})
	if !errors.Is(err, ErrHandshakeVerifyFailed) {
		t.Errorf("error = %v, want ErrHandshakeVerifyFailed", err)
	}
	select {
	case ack := <-ackCh:
		if ack != -1 {
			t.Errorf("daemon must NOT be acked when verify=false; got %d", ack)
		}
	case <-time.After(time.Second):
		t.Fatal("fake daemon did not observe the close")
	}
}

func TestDaemonHandshakeChannel_VerifyError(t *testing.T) {
	requireUnixSocket(t)
	c := newHandshakeChannel(t)
	const marker = "abc"
	ackCh := make(chan int, 1)
	go func() {
		ackCh <- dialAndSend(t, c.SockPath(), 7, marker)
	}()
	verifyErr := errors.New("procidentity: no such process")
	_, err := c.AwaitHandshake(2*time.Second, marker, func(int) (bool, error) {
		return false, verifyErr
	})
	if !errors.Is(err, ErrHandshakeVerifyFailed) {
		t.Errorf("error = %v, want ErrHandshakeVerifyFailed wrapping the verify err", err)
	}
	if !errors.Is(err, verifyErr) {
		t.Errorf("error must wrap the verify seam error; got %v", err)
	}
	select {
	case ack := <-ackCh:
		if ack != -1 {
			t.Errorf("daemon must NOT be acked on verify error; got %d", ack)
		}
	case <-time.After(time.Second):
		t.Fatal("fake daemon did not observe the close")
	}
}

func TestDaemonHandshakeChannel_TimeoutNoConnection(t *testing.T) {
	c := newHandshakeChannel(t)
	// No daemon connects. AwaitHandshake must time out quickly.
	start := time.Now()
	_, err := c.AwaitHandshake(100*time.Millisecond, "any", func(int) (bool, error) {
		t.Error("verify seam must NOT be called on timeout")
		return false, nil
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrHandshakeTimeout) {
		t.Errorf("error = %v, want ErrHandshakeTimeout", err)
	}
	// Must return promptly after the deadline, not block far longer.
	if elapsed > 500*time.Millisecond {
		t.Errorf("AwaitHandshake took %s, want close to the 100ms deadline", elapsed)
	}
}

func TestDaemonHandshakeChannel_NegativeDeadline(t *testing.T) {
	c := newHandshakeChannel(t)
	if _, err := c.AwaitHandshake(0, "m", func(int) (bool, error) { return true, nil }); !errors.Is(err, ErrHandshakeTimeout) {
		t.Errorf("zero deadline error = %v, want ErrHandshakeTimeout", err)
	}
	if _, err := c.AwaitHandshake(-time.Second, "m", func(int) (bool, error) { return true, nil }); !errors.Is(err, ErrHandshakeTimeout) {
		t.Errorf("negative deadline error = %v, want ErrHandshakeTimeout", err)
	}
}

func TestDaemonHandshakeChannel_EmptyExpectedMarker(t *testing.T) {
	c := newHandshakeChannel(t)
	if _, err := c.AwaitHandshake(time.Second, "", func(int) (bool, error) { return true, nil }); err == nil {
		t.Error("empty expected marker must return an error")
	}
}

func TestDaemonHandshakeChannel_NilVerify(t *testing.T) {
	c := newHandshakeChannel(t)
	if _, err := c.AwaitHandshake(time.Second, "m", nil); err == nil {
		t.Error("nil verify seam must return an error")
	}
}

func TestDaemonHandshakeChannel_CloseRemovesSocket(t *testing.T) {
	dir, err := newHandshakeDir(t)
	if err != nil {
		t.Fatalf("newHandshakeDir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "daemon.sock")
	c := NewDaemonHandshakeChannel(sockPath)
	if err := c.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket file not created: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("socket file must be removed after Close: %v", err)
	}
	// Close is idempotent.
	if err := c.Close(); err != nil {
		t.Errorf("second Close must be a no-op: %v", err)
	}
}

func TestDaemonHandshakeChannel_NilSafe(t *testing.T) {
	var c *DaemonHandshakeChannel
	if c.SockPath() != "" {
		t.Errorf("nil SockPath must return empty")
	}
	if err := c.Close(); err != nil {
		t.Errorf("nil Close must be a no-op: %v", err)
	}
	if err := c.Listen(); err == nil {
		t.Error("nil Listen must return an error")
	}
}

func TestDaemonHandshakeChannel_ListenEmptyPath(t *testing.T) {
	c := NewDaemonHandshakeChannel("")
	if err := c.Listen(); err == nil {
		t.Error("Listen with empty path must return an error")
	}
}

func TestDaemonHandshakeChannel_SocketFileMode(t *testing.T) {
	c := newHandshakeChannel(t)
	fi, err := os.Stat(c.SockPath())
	if err != nil {
		t.Fatalf("socket not created: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("socket mode = %o, want 0600 (owner-only)", got)
	}
}

func TestDaemonHandshakeChannel_ParentDirMustExist(t *testing.T) {
	// A non-existent parent dir must surface a clear listen error,
	// not a silent no-op. The engine (Phase 3) creates the parent
	// before Listen; a missing parent is a setup bug.
	missing := filepath.Join(t.TempDir(), "does", "not", "exist", "daemon.sock")
	c := NewDaemonHandshakeChannel(missing)
	if err := c.Listen(); err == nil {
		_ = c.Close()
		t.Fatal("Listen into a missing parent dir must fail")
	}
}

func TestDaemonHandshakeSockPath(t *testing.T) {
	got := DaemonHandshakeSockPath("/cache/build-control/requests/req-42")
	want := "/cache/build-control/requests/req-42/daemon.sock"
	if got != want {
		t.Errorf("DaemonHandshakeSockPath = %q, want %q", got, want)
	}
}

// TestAwaitHandshake_NotListening verifies AwaitHandshake returns a
// clear error when called before Listen (a caller bug that would
// otherwise panic on the (*net.UnixListener) type assertion).
func TestAwaitHandshake_NotListening(t *testing.T) {
	c := NewDaemonHandshakeChannel(filepath.Join(t.TempDir(), "daemon.sock"))
	if _, err := c.AwaitHandshake(time.Second, "m", func(int) (bool, error) { return true, nil }); err == nil {
		t.Error("AwaitHandshake before Listen must return an error")
	}
}

// TestAwaitHandshake_MalformedJSON verifies a non-JSON handshake line
// is rejected without an ack (the build fails closed; the verify seam
// is never reached).
func TestAwaitHandshake_MalformedJSON(t *testing.T) {
	requireUnixSocket(t)
	c := newHandshakeChannel(t)
	ackCh := make(chan int, 1)
	go func() {
		conn, err := net.Dial("unix", c.SockPath())
		if err != nil {
			ackCh <- -1
			return
		}
		defer conn.Close()
		// Send a malformed line (not JSON).
		conn.Write([]byte("not-json\n"))
		buf := make([]byte, 1)
		n, _ := conn.Read(buf)
		if n == 0 {
			ackCh <- -1
		} else {
			ackCh <- int(buf[0])
		}
	}()
	_, err := c.AwaitHandshake(2*time.Second, "m", func(int) (bool, error) {
		t.Error("verify seam must NOT be called on malformed JSON")
		return false, nil
	})
	if err == nil {
		t.Error("malformed JSON must return an error")
	}
	if ack := <-ackCh; ack != -1 {
		t.Errorf("daemon must NOT be acked on malformed JSON; got %d", ack)
	}
}

// TestAwaitHandshake_NonPositivePID verifies a handshake that carries
// pid <= 0 is rejected without an ack (the verify seam is never
// reached — a non-positive pid is a protocol violation).
func TestAwaitHandshake_NonPositivePID(t *testing.T) {
	requireUnixSocket(t)
	c := newHandshakeChannel(t)
	const marker = "m"
	ackCh := make(chan int, 1)
	go func() {
		ackCh <- dialAndSend(t, c.SockPath(), 0, marker)
	}()
	_, err := c.AwaitHandshake(2*time.Second, marker, func(int) (bool, error) {
		t.Error("verify seam must NOT be called for a non-positive pid")
		return false, nil
	})
	if err == nil {
		t.Error("non-positive pid must return an error")
	}
	if ack := <-ackCh; ack != -1 {
		t.Errorf("daemon must NOT be acked on non-positive pid; got %d", ack)
	}
}

// TestAwaitHandshake_ReadTimeout verifies a daemon that connects but
// never sends the handshake line triggers ErrHandshakeTimeout (a
// hung daemon cannot hold the host forever).
func TestAwaitHandshake_ReadTimeout(t *testing.T) {
	requireUnixSocket(t)
	c := newHandshakeChannel(t)
	// Daemon connects but never sends the handshake line.
	connErr := make(chan error, 1)
	go func() {
		conn, err := net.Dial("unix", c.SockPath())
		if err != nil {
			connErr <- err
			return
		}
		defer conn.Close()
		// Hold the connection open without writing.
		connErr <- nil
		// Block until the host closes the conn (the defer above then
		// closes it).
		select {}
	}()
	_, err := c.AwaitHandshake(200*time.Millisecond, "m", func(int) (bool, error) {
		t.Error("verify seam must NOT be called on read timeout")
		return false, nil
	})
	if !errors.Is(err, ErrHandshakeTimeout) {
		t.Errorf("error = %v, want ErrHandshakeTimeout", err)
	}
	if err := <-connErr; err != nil {
		t.Errorf("fake daemon dial failed: %v", err)
	}
}

// TestAwaitHandshake_HostClosesWithoutAck confirms the contract: when
// the host returns an error (marker mismatch / verify false), it does
// NOT ack; the daemon's read sees EOF (-1). This is the fail-closed
// path the init script relies on to throw a GradleException.
func TestAwaitHandshake_HostClosesWithoutAck(t *testing.T) {
	requireUnixSocket(t)
	c := newHandshakeChannel(t)
	const marker = "right"
	ackCh := make(chan int, 1)
	go func() {
		ackCh <- dialAndSend(t, c.SockPath(), 1, "wrong")
	}()
	_, _ = c.AwaitHandshake(2*time.Second, marker, func(int) (bool, error) { return true, nil })
	if ack := <-ackCh; ack != -1 {
		t.Errorf("daemon must see EOF (-1) when host fails without ack; got %d", ack)
	}
}

// TestAwaitHandshake_VerifySeamMapsToProcidentity is a documentation
// test: it shows the exact closure shape Phase 3 will pass to wire
// AwaitHandshake to procidentity.Verify. It does NOT call real
// procidentity (that is Phase 3's integration); it asserts the seam
// signature is callable with the documented closure shape so the
// handoff to Phase 3 is unambiguous.
func TestAwaitHandshake_VerifySeamMapsToProcidentity(t *testing.T) {
	requireUnixSocket(t)
	c := newHandshakeChannel(t)
	const marker = "m"
	const wantPID = 1234
	// This is the closure shape Phase 3 will use: it calls
	// procidentity.Verify(pid, expectedJDKExecutable, "") and returns
	// (verified, err). expectedStart is "" at handshake time (the
	// daemon was just promoted; the start identity is captured from
	// the returned Identity and recorded via
	// buildcontrol.PromoteDaemonRecord). Here we use a fake that
	// mimics the contract.
	const expectedJDK = "/usr/lib/jvm/bin/java"
	verifyClosure := func(pid int) (bool, error) {
		// Phase 3 replaces this body with:
		//   verified, id, err := procidentity.Verify(pid, expectedJDK, "")
		//   if err != nil { return false, err }
		//   if !verified { return false, nil }
		//   startID = id.StartIdentity  // captured for PromoteDaemonRecord
		//   return true, nil
		if pid != wantPID {
			return false, fmt.Errorf("pid %d != want %d", pid, wantPID)
		}
		_ = expectedJDK
		return true, nil
	}
	ackCh := make(chan int, 1)
	go func() { ackCh <- dialAndSend(t, c.SockPath(), wantPID, marker) }()
	pid, err := c.AwaitHandshake(2*time.Second, marker, verifyClosure)
	if err != nil {
		t.Fatalf("AwaitHandshake: %v", err)
	}
	if pid != wantPID {
		t.Errorf("pid = %d, want %d", pid, wantPID)
	}
	if ack := <-ackCh; ack != '1' {
		t.Errorf("daemon ack = %d, want '1'", ack)
	}
}

// TestAwaitHandshake_CancelInterruptsBlockedAccept asserts that Cancel
// interrupts a blocked AwaitHandshake (no daemon dials) so the engine
// does not hang for the full handshake deadline when the wrapper exits
// before a daemon registers (the 45s-hang fix). Cancel closes the
// listener; AwaitHandshake's Accept returns net.ErrClosed, which maps
// to ErrHandshakeCancelled. The test does NOT requireUnixSocket because
// it only listens (no dial) — the omac sandbox permits listen.
func TestAwaitHandshake_CancelInterruptsBlockedAccept(t *testing.T) {
	c := newHandshakeChannel(t)
	// AwaitHandshake blocks on Accept (no daemon dials). Run it in a
	// goroutine; Cancel must interrupt it well under the deadline.
	done := make(chan error, 1)
	go func() {
		_, err := c.AwaitHandshake(30*time.Second, "m", func(int) (bool, error) { return true, nil })
		done <- err
	}()
	// Give Accept a moment to block, then Cancel.
	select {
	case err := <-done:
		t.Fatalf("AwaitHandshake returned before Cancel: %v (expected to block on Accept)", err)
	case <-time.After(100 * time.Millisecond):
	}
	c.Cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrHandshakeCancelled) {
			t.Errorf("AwaitHandshake after Cancel = %v, want ErrHandshakeCancelled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AwaitHandshake did not return within 2s of Cancel — the 45s-hang bug is still present")
	}
}

// TestCancel_IdempotentAndSafeAfterClose asserts Cancel is idempotent
// and safe to call after Close (the engine defers Close AND calls
// Cancel on RunBuild return; the two must not race or double-close).
func TestCancel_IdempotentAndSafeAfterClose(t *testing.T) {
	c := newHandshakeChannel(t)
	c.Cancel() // before any await — no-op, must not panic
	c.Cancel() // idempotent
	c.Close()  // Close after Cancel — must not panic
	c.Cancel() // after Close — no-op, must not panic
	c.Close()  // idempotent Close
}
