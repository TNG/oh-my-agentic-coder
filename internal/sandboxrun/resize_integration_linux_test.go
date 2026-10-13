//go:build linux

package sandboxrun

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// The tests in this file run omac in a real PTY under a minimal job-control
// shell, like omac started from a shell in a terminal window. bwrap runs
// with --new-session, so the sandboxed program has no controlling terminal
// and depends on omac for resize notifications (SIGWINCH). Regression
// tests: OpenCode stayed at its old size after the window was resized.
//
// The resize tests go from narrow to realistic:
//   - TestIntegrationResizeReachesSandboxedTUI: the test binary is the
//     supervisor and runs bare bwrap (no stage 2).
//   - TestIntegrationResizeThroughSandboxRun: the real omac binary, `omac
//     sandbox run`, including stage 2 (Landlock, seccomp).
//   - TestIntegrationResizeThroughStart: `omac start`, which hands the
//     terminal to `omac sandbox run`, which runs bwrap. This is the process
//     chain of the bug report.

// Environment of the helper processes (TestPTYSupervisorHelper).
const (
	ptyHelperModeEnv   = "OMAC_TEST_PTY_MODE"   // "shell" or "supervisor"
	ptyHelperArgvEnv   = "OMAC_TEST_PTY_ARGV"   // shell: JSON argv of the job
	ptyHelperJobEnv    = "OMAC_TEST_PTY_JOB"    // shell: the job's mode, if any
	ptyHelperScriptEnv = "OMAC_TEST_PTY_SCRIPT" // supervisor: script to run
	ptyHelperBwrapEnv  = "OMAC_TEST_PTY_BWRAP"  // supervisor: "1" runs it in bwrap
	ptyHelperTostopEnv = "OMAC_TEST_PTY_TOSTOP" // supervisor: "1" sets stty tostop
)

// resizeProbeScript stands in for a TUI: it prints the size it reads on
// every SIGWINCH. READY names PID 1, which is "bwrap" only inside the
// sandbox, so a launch that silently skips the sandbox cannot pass. The
// trap runs between loop iterations; the loop is bounded so a failed test
// cannot leave it behind for long.
const resizeProbeScript = `trap 'echo WINCH $(stty size)' WINCH; echo READY $(cat /proc/1/comm); i=0; while [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done`

func TestIntegrationResizeReachesSandboxedTUI(t *testing.T) {
	requireBwrap(t)
	checkResizeRelay(t, startTestSupervisor(t, true, false, resizeProbeScript))
}

func TestIntegrationResizeThroughSandboxRun(t *testing.T) {
	requireBwrap(t)
	omac := buildOmac(t)
	checkResizeRelay(t, startInPTY(t, resizeTestEnv(t),
		omac, "sandbox", "run", "--block-net", "--", "/bin/sh", "-c", resizeProbeScript))
}

func TestIntegrationResizeThroughStart(t *testing.T) {
	requireBwrap(t)
	omac := buildOmac(t)
	// --inner swaps the harness binary for the probe; everything else is
	// the opencode launch path.
	checkResizeRelay(t, startInPTY(t, resizeTestEnv(t),
		omac, "start", "opencode", "--ephemeral-cache", "--inner", "/bin/sh", "--", "-c", resizeProbeScript))
}

// TestIntegrationSuspendKeyDoesNotStopOmac: confined code shares the
// terminal and can make an ordinary key send SIGTSTP. If that stopped omac,
// the shell would take the terminal back while the sandbox kept reading it,
// so the sandbox could read what the user types into the shell.
func TestIntegrationSuspendKeyDoesNotStopOmac(t *testing.T) {
	requireBwrap(t)
	omac := buildOmac(t)
	p := startInPTY(t, resizeTestEnv(t),
		omac, "sandbox", "run", "--block-net", "--", "/bin/sh", "-c", "stty isig susp q; "+resizeProbeScript)
	p.waitFor(t, "READY bwrap", 60*time.Second, "the program never started inside bwrap")

	p.write(t, "q")
	time.Sleep(500 * time.Millisecond)
	if strings.Contains(p.output(), "SUPERVISOR STOPPED") {
		t.Fatalf("the remapped suspend key stopped omac; output:\n%s", p.output())
	}
	for _, pid := range processTree(p.shellPID) {
		if procState(pid) == "T" {
			t.Fatalf("pid %d is stopped after the remapped suspend key", pid)
		}
	}
	// The relay still works after the suspend attempt.
	checkResizeRelay(t, p)
}

// TestIntegrationChildErrorsWithTostop: the child (bwrap's outer process)
// stays in the terminal's foreground group, so with `stty tostop` it can
// still print an error and exit instead of being stopped by SIGTTOU.
func TestIntegrationChildErrorsWithTostop(t *testing.T) {
	p := startTestSupervisor(t, false, true, `echo bwrap-like-error >&2; exit 3`)
	p.waitFor(t, "bwrap-like-error", 10*time.Second, "the child's error never appeared")
	if code := p.waitExit(t); code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
}

// checkResizeRelay resizes twice (a relay that works only once, or reports
// a stale size, fails the second step), then checks Ctrl-C still ends the
// sandbox: omac keeps the terminal foreground for a detached child, so
// Ctrl-C reaches omac as well as bwrap.
func checkResizeRelay(t *testing.T, p *ptyProcess) {
	t.Helper()
	p.waitFor(t, "READY bwrap", 60*time.Second, "the program never started inside bwrap")
	for _, size := range [][2]uint16{{62, 242}, {30, 100}} {
		p.resize(t, size[0], size[1])
		want := fmt.Sprintf("WINCH %d %d", size[0], size[1])
		p.waitFor(t, want, 10*time.Second, fmt.Sprintf("the sandboxed program did not get SIGWINCH after a resize to %dx%d", size[1], size[0]))
	}
	p.write(t, "\x03")
	p.waitExit(t)
}

// resizeTestEnv isolates omac's config, state and caches in a temp home.
func resizeTestEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "TERM=xterm-256color"}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "PATH=") || strings.HasPrefix(kv, "TMPDIR=") {
			env = append(env, kv)
		}
	}
	return env
}

// startTestSupervisor runs the test binary as omac's supervisor
// (execSandboxChild), with script run in bwrap or directly.
func startTestSupervisor(t *testing.T, bwrap, tostop bool, script string) *ptyProcess {
	t.Helper()
	env := append(os.Environ(),
		ptyHelperJobEnv+"=supervisor",
		ptyHelperScriptEnv+"="+script,
		ptyHelperBwrapEnv+"="+boolEnv(bwrap),
		ptyHelperTostopEnv+"="+boolEnv(tostop))
	return startInPTY(t, env, os.Args[0], "-test.run=^TestPTYSupervisorHelper$")
}

// TestPTYSupervisorHelper is the helper process of the PTY tests: a minimal
// job-control shell ("shell" mode) or the omac supervisor ("supervisor"
// mode). It does nothing in a normal run.
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

// ptyShell runs the job the way an interactive shell does: in its own
// process group, which becomes the terminal's foreground group. The shell
// is what makes that group non-orphaned; without it the kernel drops
// keyboard SIGTSTP and a stopped omac would go unnoticed. If the job stops,
// it prints SUPERVISOR STOPPED, as a shell prints "Stopped" before taking
// the terminal back.
func ptyShell(t *testing.T) {
	var argv []string
	if err := json.Unmarshal([]byte(os.Getenv(ptyHelperArgvEnv)), &argv); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	// The job must not inherit shell mode, or it would start another shell.
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, ptyHelperModeEnv+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	if job := os.Getenv(ptyHelperJobEnv); job != "" {
		cmd.Env = append(cmd.Env, ptyHelperModeEnv+"="+job)
	}
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
			_, _ = os.Stdout.WriteString("SUPERVISOR STOPPED\n")
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

// ptyProcess is the helper shell running a job in a fresh PTY: its own
// session with the PTY as controlling terminal, like a terminal window.
type ptyProcess struct {
	master   *os.File
	shellPID int
	exited   chan struct{}
	state    *os.ProcessState
	mu       sync.Mutex
	out      bytes.Buffer
}

// startInPTY runs argv with env as the job of a helper shell in a new PTY.
func startInPTY(t *testing.T, env []string, argv ...string) *ptyProcess {
	t.Helper()
	master, slave := openPTY(t)
	setWinsize(t, master, 42, 172)
	argvJSON, err := json.Marshal(argv)
	if err != nil {
		t.Fatal(err)
	}
	shell := exec.Command(os.Args[0], "-test.run=^TestPTYSupervisorHelper$")
	shell.Dir = t.TempDir()
	shell.Env = append(env, ptyHelperModeEnv+"=shell", ptyHelperArgvEnv+"="+string(argvJSON))
	shell.Stdin, shell.Stdout, shell.Stderr = slave, slave, slave
	shell.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := shell.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()

	p := &ptyProcess{master: master, shellPID: shell.Process.Pid, exited: make(chan struct{})}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			p.mu.Lock()
			p.out.Write(buf[:n])
			p.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		p.state, _ = shell.Process.Wait()
		close(p.exited)
	}()
	t.Cleanup(func() {
		// A supervisor killed by SIGKILL cannot pass it on and may leave its
		// children behind: kill the whole tree, leaves first.
		tree := processTree(shell.Process.Pid)
		for i := len(tree) - 1; i >= 0; i-- {
			_ = syscall.Kill(tree[i], syscall.SIGKILL)
		}
		<-p.exited
		master.Close()
	})
	return p
}

func (p *ptyProcess) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.out.String()
}

func (p *ptyProcess) waitFor(t *testing.T, want string, timeout time.Duration, failure string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(p.output(), want) {
			return
		}
		select {
		case <-p.exited:
			if strings.Contains(p.output(), want) {
				return
			}
			t.Fatalf("%s: the process exited; output:\n%s", failure, p.output())
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatalf("%s; output:\n%s", failure, p.output())
}

// waitExit waits for the shell to exit and returns its exit code, which is
// the job's.
func (p *ptyProcess) waitExit(t *testing.T) int {
	t.Helper()
	select {
	case <-p.exited:
		return p.state.ExitCode()
	case <-time.After(10 * time.Second):
		t.Fatalf("omac did not exit within 10s; output:\n%s", p.output())
		return 0
	}
}

func (p *ptyProcess) write(t *testing.T, in string) {
	t.Helper()
	if _, err := p.master.Write([]byte(in)); err != nil {
		t.Fatal(err)
	}
}

func (p *ptyProcess) resize(t *testing.T, rows, cols uint16) {
	t.Helper()
	setWinsize(t, p.master, rows, cols)
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

func boolEnv(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
