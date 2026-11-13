//go:build linux

package sandbox

import (
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// openPTY opens a pseudo-terminal pair: the controller side (what a terminal
// emulator reads) and the terminal side (what a program uses as its tty).
func openPTY(t *testing.T) (controller, tty *os.File) {
	t.Helper()
	controller, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	fd := int(controller.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlock pty: %v", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("pty number: %v", err)
	}
	tty, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	t.Cleanup(func() { _ = tty.Close(); _ = controller.Close() })
	return controller, tty
}

// A TUI that dies leaves the terminal in raw mode; the restore must bring
// echo and line editing back and send the mode resets to the terminal.
func TestSaveTerminalOnRealPTY(t *testing.T) {
	controller, tty := openPTY(t)
	fd := int(tty.Fd())

	ops := realTerminalOps
	// The test process does not own this pty as its controlling terminal.
	ops.isForeground = func(int) bool { return true }
	restore := saveTerminalWith(ops, fd, tty, fd)

	// What the dying TUI leaves behind.
	if _, err := term.MakeRaw(fd); err != nil {
		t.Fatalf("make raw: %v", err)
	}
	restore()

	tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		t.Fatalf("get termios: %v", err)
	}
	if tio.Lflag&unix.ECHO == 0 || tio.Lflag&unix.ICANON == 0 {
		t.Fatalf("terminal still raw after restore: lflag=%#x", tio.Lflag)
	}

	got := make([]byte, len(terminalResetSeq))
	_ = controller.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(controller, got); err != nil {
		t.Fatalf("read reset sequence from pty: %v", err)
	}
	if string(got) != terminalResetSeq {
		t.Fatalf("terminal received %q, want %q", got, terminalResetSeq)
	}
}
