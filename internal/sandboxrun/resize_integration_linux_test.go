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

// Environment of the supervisor helper process (TestPTYSupervisorHelper).
const (
	ptyHelperScriptEnv = "OMAC_TEST_PTY_SCRIPT"     // shell script to run
	ptyHelperBwrapEnv  = "OMAC_TEST_PTY_BWRAP"      // "1": run it inside bwrap
	ptyHelperTostopEnv = "OMAC_TEST_PTY_TOSTOP"     // "1": stty tostop first
	ptyHelperModeEnv   = "OMAC_TEST_PTY_SUPERVISOR" // "shell" or "supervisor"
)

// readyLoop prints READY, then idles for at most 30s so a failed test cannot
// leave it behind for long. Traps run between loop iterations.
const readyLoop = `echo READY; i=0; while [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done`

// TestIntegrationResizeReachesSandboxedTUI resizes a real PTY the way a
// terminal emulator does (TIOCSWINSZ on the master side) and checks that a
// program inside bwrap gets SIGWINCH and reads the new size. bwrap runs with
// --new-session, so the program has no controlling terminal and depends on
// omac relaying the signal. Regression test: OpenCode stayed at its old size
// after the window was resized.
func TestIntegrationResizeReachesSandboxedTUI(t *testing.T) {
	requireBwrap(t)
	s := startPTYSupervisor(t, true, false, `trap 'echo WINCH $(stty size)' WINCH; `+readyLoop)
	s.waitFor(t, "READY")

	s.setWinsize(t, 62, 242)
	s.waitFor(t, "WINCH 62 242")
	s.setWinsize(t, 30, 100)
	s.waitFor(t, "WINCH 30 100")

	// omac keeps the terminal foreground, so Ctrl-C reaches omac as well as
	// bwrap. It must still end the sandbox.
	s.write(t, "\x03")
	s.waitExit(t)
}

// TestIntegrationSuspendKeyDoesNotStopOmac: confined code shares the
// terminal and can make an ordinary key send SIGTSTP. If that stopped omac,
// a job-control shell would take the terminal back while the sandbox kept
// reading it, so the sandbox could read what the user types into the shell.
func TestIntegrationSuspendKeyDoesNotStopOmac(t *testing.T) {
	requireBwrap(t)
	s := startPTYSupervisor(t, true, false, `stty isig susp q; trap 'echo WINCH $(stty size)' WINCH; `+readyLoop)
	s.waitFor(t, "READY")

	s.write(t, "q")
	time.Sleep(500 * time.Millisecond)
	if strings.Contains(s.output(), "SUPERVISOR STOPPED") {
		t.Fatalf("the remapped suspend key stopped omac; output:\n%s", s.output())
	}
	supervisor := childPIDsOf(s.cmd.Process.Pid)
	for _, pid := range append(supervisor, childPIDsOf(supervisor[0])...) {
		if state := procState(pid); state == "T" {
			t.Fatalf("pid %d is stopped after the remapped suspend key", pid)
		}
	}
	// The relay still works after the suspend attempt.
	s.setWinsize(t, 62, 242)
	s.waitFor(t, "WINCH 62 242")
}

// TestIntegrationChildErrorsWithTostop: the child (bwrap's outer process)
// stays in the terminal's foreground group, so with `stty tostop` it can
// still print an error and exit instead of being stopped by SIGTTOU.
func TestIntegrationChildErrorsWithTostop(t *testing.T) {
	s := startPTYSupervisor(t, false, true, `echo bwrap-like-error >&2; exit 3`)
	s.waitFor(t, "bwrap-like-error")
	if code := s.waitExit(t); code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}

// TestPTYSupervisorHelper is the helper process of the PTY tests: a minimal
// job-control shell ("shell" mode) that runs the omac supervisor
// ("supervisor" mode). It does nothing in a normal run.
func TestPTYSupervisorHelper(t *testing.T) {
	switch os.Getenv(ptyHelperModeEnv) {
	case "shell":
		ptyShell(t)
	case "supervisor":
		ptySupervisorMain(t)
	default:
		t.Skip("helper process only")
	}
}

// ptyShell runs the supervisor the way an interactive shell runs a job: in
// its own process group, which becomes the terminal's foreground group. The
// shell is what makes that group non-orphaned; without it the kernel drops
// keyboard SIGTSTP and a stopped supervisor would go unnoticed. If the
// supervisor stops, it prints SUPERVISOR STOPPED, as a shell would print
// "Stopped" before taking the terminal back.
func ptyShell(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestPTYSupervisorHelper$")
	cmd.Env = append(os.Environ(), ptyHelperModeEnv+"=supervisor")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	for {
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(cmd.Process.Pid, &ws, syscall.WUNTRACED, nil); err != nil {
			t.Fatal(err)
		}
		switch {
		case ws.Stopped():
			os.Stdout.WriteString("SUPERVISOR STOPPED\n")
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			os.Exit(99)
		case ws.Exited():
			os.Exit(ws.ExitStatus())
		case ws.Signaled():
			os.Exit(128 + int(ws.Signal()))
		}
	}
}

// ptySupervisorMain is omac: it runs the script through execSandboxChild.
func ptySupervisorMain(t *testing.T) {
	if os.Getenv(ptyHelperTostopEnv) == "1" {
		tio, err := unix.IoctlGetTermios(0, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		tio.Lflag |= unix.TOSTOP
		if err := unix.IoctlSetTermios(0, unix.TCSETS, tio); err != nil {
			t.Fatal(err)
		}
	}
	argv := []string{"/bin/sh", "-c", os.Getenv(ptyHelperScriptEnv)}
	workdir := ""
	if os.Getenv(ptyHelperBwrapEnv) == "1" {
		p := &sandboxprofile.Profile{
			Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
			Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
		}
		g, err := ResolveGrants(p, t.TempDir(), os.Stderr)
		if err != nil {
			t.Fatal(err)
		}
		if argv, err = BuildBwrapArgv(g, argv); err != nil {
			t.Fatal(err)
		}
		workdir = g.Workdir
	}
	code, err := execSandboxChild(argv, os.Environ(), workdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	os.Exit(code)
}

// ptySupervisor is the helper shell running omac under a fresh PTY, like a
// terminal window.
type ptySupervisor struct {
	cmd    *exec.Cmd
	master *os.File
	mu     sync.Mutex
	out    bytes.Buffer
	exited chan struct{}
	state  *os.ProcessState
}

func startPTYSupervisor(t *testing.T, bwrap, tostop bool, script string) *ptySupervisor {
	t.Helper()
	master, slave := openPTY(t)
	s := &ptySupervisor{master: master, exited: make(chan struct{})}
	s.setWinsize(t, 42, 172)

	s.cmd = exec.Command(os.Args[0], "-test.run=^TestPTYSupervisorHelper$")
	s.cmd.Env = append(os.Environ(),
		ptyHelperModeEnv+"=shell",
		ptyHelperScriptEnv+"="+script,
		ptyHelperBwrapEnv+"="+boolEnv(bwrap),
		ptyHelperTostopEnv+"="+boolEnv(tostop))
	s.cmd.Stdin, s.cmd.Stdout, s.cmd.Stderr = slave, slave, slave
	// Own session with the PTY as controlling terminal, like the shell in a
	// terminal window.
	s.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := s.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	t.Cleanup(func() {
		_ = s.cmd.Process.Kill()
		<-s.exited
		master.Close()
	})

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			s.mu.Lock()
			s.out.Write(buf[:n])
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		s.state, _ = s.cmd.Process.Wait()
		close(s.exited)
	}()
	return s
}

func (s *ptySupervisor) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.out.String()
}

func (s *ptySupervisor) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(s.output(), want) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no %q within 10s; output:\n%s", want, s.output())
}

// waitExit waits for the helper shell to exit and returns its exit code,
// which is the supervisor's.
func (s *ptySupervisor) waitExit(t *testing.T) int {
	t.Helper()
	select {
	case <-s.exited:
		return s.state.ExitCode()
	case <-time.After(10 * time.Second):
		t.Fatalf("supervisor did not exit within 10s; output:\n%s", s.output())
		return 0
	}
}

func (s *ptySupervisor) write(t *testing.T, in string) {
	t.Helper()
	if _, err := s.master.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
}

func (s *ptySupervisor) setWinsize(t *testing.T, rows, cols uint16) {
	t.Helper()
	if err := unix.IoctlSetWinsize(int(s.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		t.Fatal(err)
	}
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

// procState returns the state letter of /proc/<pid>/stat ("T" = stopped),
// or "" when the process is gone.
func procState(pid int) string {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(data)
	fields := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func childPIDsOf(pid int) []int {
	data, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/task/" + strconv.Itoa(pid) + "/children")
	var out []int
	for _, f := range strings.Fields(string(data)) {
		if c, err := strconv.Atoi(f); err == nil {
			out = append(out, c)
		}
	}
	return out
}

func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
