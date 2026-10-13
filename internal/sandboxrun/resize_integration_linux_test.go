//go:build linux

package sandboxrun

import (
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// resizeHelperEnv makes the test binary act as the omac supervisor for
// TestIntegrationResizeReachesSandboxedTUI (see TestResizeSupervisorHelper).
const resizeHelperEnv = "OMAC_TEST_RESIZE_SUPERVISOR"

// TestIntegrationResizeReachesSandboxedTUI resizes a real PTY the way a
// terminal emulator does (TIOCSWINSZ on the master side) and checks that a
// program inside bwrap gets SIGWINCH and reads the new size. bwrap runs with
// --new-session, so the program has no controlling terminal and depends on
// omac relaying the signal. Regression test: OpenCode stayed at its old size
// after the window was resized.
func TestIntegrationResizeReachesSandboxedTUI(t *testing.T) {
	requireBwrap(t)

	master, slave := openPTY(t)
	defer master.Close()
	setWinsize(t, master, 42, 172)

	cmd := exec.Command(os.Args[0], "-test.run=^TestResizeSupervisorHelper$")
	cmd.Env = append(os.Environ(), resizeHelperEnv+"=1")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// Like a shell in a terminal window: own session, PTY as controlling
	// terminal, so the supervisor is the foreground process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	defer func() { _ = cmd.Process.Kill() }()

	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	waitFor := func(want string) bool {
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			got := strings.Contains(out.String(), want)
			mu.Unlock()
			if got {
				return true
			}
			time.Sleep(50 * time.Millisecond)
		}
		return false
	}

	if !waitFor("READY") {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("sandboxed program never started; output:\n%s", out.String())
	}
	setWinsize(t, master, 62, 242)
	if !waitFor("WINCH 62 242") {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("sandboxed program did not get SIGWINCH after resize to 242x62; output:\n%s", out.String())
	}

	// omac keeps the terminal foreground now, so Ctrl-C reaches omac rather
	// than bwrap. It must still end the sandbox.
	if _, err := master.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		_, _ = cmd.Process.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("Ctrl-C did not end the sandbox")
	}
}

// TestResizeSupervisorHelper is the supervisor process of
// TestIntegrationResizeReachesSandboxedTUI. It does nothing in a normal run.
func TestResizeSupervisorHelper(t *testing.T) {
	if os.Getenv(resizeHelperEnv) != "1" {
		t.Skip("helper process only")
	}
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, t.TempDir(), os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	// The trap runs between loop iterations; the loop is bounded so a
	// failed test cannot leave it behind for long.
	script := `trap 'echo WINCH $(stty size)' WINCH; echo READY; i=0; while [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done`
	argv, err := BuildBwrapArgv(g, []string{"/bin/sh", "-c", script})
	if err != nil {
		t.Fatal(err)
	}
	code, err := execSandboxChild(argv, os.Environ(), g.Workdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(code)
}

func openPTY(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	fd := int(master.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return master, slave
}

func setWinsize(t *testing.T, f *os.File, rows, cols uint16) {
	t.Helper()
	if err := unix.IoctlSetWinsize(int(f.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		t.Fatal(err)
	}
}
