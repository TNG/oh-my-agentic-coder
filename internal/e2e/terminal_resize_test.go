//go:build e2e && linux

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestHarnessTerminalResize starts the real OpenCode TUI with `omac start`
// in a pseudo-terminal, resizes the terminal and checks that OpenCode
// repaints at the new size. Model-free: the TUI's home screen renders
// without a provider.
//
// Linux-only because only bwrap detaches the harness from the terminal
// (--new-session), so it gets resize notifications only through omac's
// relay. Without the relay OpenCode keeps its old size and the newly
// exposed area stays empty (the regression this guards).
//
// How "repainted at the new size" is detected: OpenCode draws with
// absolute cursor moves (ESC [ row ; col H). The terminal starts at 24x80
// and grows to 50x160, so any cursor move written after the resize to a
// row past 24 or a column past 80 can only come from a repaint that used
// the new size. Before the fix OpenCode wrote nothing at all after the
// resize.
func TestHarnessTerminalResize(t *testing.T) {
	if name := os.Getenv("E2E_HARNESS"); name != "" && name != "opencode" {
		t.Skipf("E2E_HARNESS=%s: this test drives the OpenCode TUI only", name)
	}
	h, ok := harnessByName("opencode")
	if !ok {
		t.Fatal("opencode harness config missing")
	}
	if forceNoSandbox(h) {
		t.Skip("running without the sandbox: there is no detached session whose resize relay could be tested")
	}

	home := t.TempDir()
	workdir := t.TempDir()
	for _, dir := range []string{".cache", ".cache/opencode", ".local/share/opencode", ".local/state/opencode/locks"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	omacBin := buildOmac(t)
	installHarness(t, h, home)
	version := harnessVersion(t, h, home)
	writeSandboxProfile(t, home, h, nil)

	env := append(withHome(os.Environ(), home), "PWD="+workdir)
	env = withEnv(env, "TERM", "xterm-256color")
	if shortTmp := shortTmpDirForNested(t); shortTmp != "" {
		env = withEnv(env, "TMPDIR", shortTmp)
	}
	cmd := exec.Command(omacBin, "start", h.Name)
	cmd.Dir = workdir
	cmd.Env = env

	term := startTUI(t, cmd, 24, 80)
	result := "FAIL"
	defer func() { emitCompat(compatLine(h.Name, version, runtime.GOOS, "resize", result)) }()

	// Wait for the first full frame: the footer (bottom rows) is painted
	// last. Then let it settle so a late startup frame is not mistaken for
	// the repaint below.
	if !term.waitFor(90*time.Second, 0, func(out []byte) bool { return maxCursor(out).row >= 20 }) {
		t.Fatalf("OpenCode never painted its home screen at 24x80; output tail:\n%q", tailBytes(term.output(), 2000))
	}
	time.Sleep(2 * time.Second)

	mark := len(term.output())
	term.resize(t, 50, 160)
	grew := func(out []byte) bool {
		c := maxCursor(out)
		return c.row > 24 || c.col > 80
	}
	if !term.waitFor(15*time.Second, mark, grew) {
		after := term.output()[mark:]
		t.Fatalf("OpenCode did not repaint at 50x160 after the resize (it wrote %d bytes, furthest cursor move %+v); output tail:\n%q",
			len(after), maxCursor(after), tailBytes(after, 2000))
	}
	result = "PASS"
}

type cursorPos struct{ row, col int }

var cursorMoveRE = regexp.MustCompile(`\x1b\[(\d+);(\d+)H`)

// maxCursor returns the largest row and the largest column (independently)
// that out moves the cursor to.
func maxCursor(out []byte) cursorPos {
	var m cursorPos
	for _, g := range cursorMoveRE.FindAllSubmatch(out, -1) {
		r, _ := strconv.Atoi(string(g[1]))
		c, _ := strconv.Atoi(string(g[2]))
		m.row = max(m.row, r)
		m.col = max(m.col, c)
	}
	return m
}

func tailBytes(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

// ptyTerminal runs a command as the foreground job of a pseudo-terminal, as a
// terminal emulator runs a shell, and records everything it writes.
type ptyTerminal struct {
	master *os.File
	exited chan struct{}
	mu     sync.Mutex
	out    bytes.Buffer
}

func startTUI(t *testing.T, cmd *exec.Cmd, rows, cols uint16) *ptyTerminal {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open /dev/ptmx: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	term := &ptyTerminal{master: master, exited: make(chan struct{})}
	term.resize(t, rows, cols)

	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	slave.Close()
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := master.Read(buf)
			term.mu.Lock()
			term.out.Write(buf[:n])
			term.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		_, _ = cmd.Process.Wait()
		close(term.exited)
	}()
	t.Cleanup(func() {
		// SIGTERM lets omac tear the sandbox down; whatever is left after
		// that (omac start, omac sandbox run and bwrap each run in their
		// own process group) is killed leaves-first.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-term.exited:
		case <-time.After(10 * time.Second):
		}
		tree := descendants(cmd.Process.Pid)
		for i := len(tree) - 1; i >= 0; i-- {
			_ = syscall.Kill(tree[i], syscall.SIGKILL)
		}
		<-term.exited
		master.Close()
	})
	return term
}

func (p *ptyTerminal) output() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return bytes.Clone(p.out.Bytes())
}

// waitFor polls until done holds for the output written from offset from
// on, the process exits, or timeout passes.
func (p *ptyTerminal) waitFor(timeout time.Duration, from int, done func([]byte) bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if done(p.output()[from:]) {
			return true
		}
		select {
		case <-p.exited:
			return done(p.output()[from:])
		case <-time.After(100 * time.Millisecond):
		}
	}
	return false
}

func (p *ptyTerminal) resize(t *testing.T, rows, cols uint16) {
	t.Helper()
	if err := unix.IoctlSetWinsize(int(p.master.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols}); err != nil {
		t.Fatal(err)
	}
}

// descendants returns pid and all its descendants, parents before children.
func descendants(pid int) []int {
	out := []int{pid}
	for i := 0; i < len(out); i++ {
		p := strconv.Itoa(out[i])
		data, err := os.ReadFile(filepath.Join("/proc", p, "task", p, "children"))
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(data)) {
			if c, err := strconv.Atoi(f); err == nil {
				out = append(out, c)
			}
		}
	}
	return out
}
