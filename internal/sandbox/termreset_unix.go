//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package sandbox

import (
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

// terminalResetSeq turns off the terminal modes a TUI typically enables and
// only disables again on a clean exit. Every sequence is a plain mode switch
// that does nothing if the mode is already off, so writing it after a
// program that never touched them (e.g. `ls`) changes nothing visible.
//
//   - ?1000/?1002/?1003: mouse click, drag and motion reports
//   - ?1005/?1006/?1015: mouse report encodings (UTF-8, SGR, urxvt)
//   - ?1004: focus in/out reports (^[[I / ^[[O)
//   - ?2004: bracketed paste
//   - ?25h: show the cursor
//
// The alternate screen (?1049) is left alone on purpose: leaving it also
// restores a saved cursor position, which moves the cursor if the program
// never entered it.
const terminalResetSeq = "\x1b[?1000l\x1b[?1002l\x1b[?1003l" +
	"\x1b[?1005l\x1b[?1006l\x1b[?1015l" +
	"\x1b[?1004l\x1b[?2004l\x1b[?25h"

// terminalOps are the terminal calls saveTerminal makes, swappable in tests.
type terminalOps struct {
	isTerminal   func(fd int) bool
	getState     func(fd int) (*term.State, error)
	restore      func(fd int, s *term.State) error
	isForeground func(fd int) bool
}

var realTerminalOps = terminalOps{
	isTerminal: term.IsTerminal,
	getState:   term.GetState,
	restore:    term.Restore,
	isForeground: func(fd int) bool {
		fg, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
		return err == nil && fg == unix.Getpgrp()
	},
}

// saveTerminal records the terminal settings before a child starts and
// returns a function that puts them back after the child has exited.
//
// A TUI that is killed, or that a plugin ends with process.exit, never runs
// its own cleanup. The terminal then stays in raw mode and keeps sending
// mouse and focus reports, which the shell prints as text such as
// "^[[<35;80;12M". The returned function restores the saved termios
// settings (echo, line editing) and writes terminalResetSeq to out.
//
// It does nothing unless in and out are both terminals, and it skips the
// restore if omac is not in the terminal's foreground process group at that
// point (e.g. omac was moved to the background), so it never changes the
// terminal under another job.
func saveTerminal(in, out *os.File) func() {
	return saveTerminalWith(realTerminalOps, int(in.Fd()), out, int(out.Fd()))
}

func saveTerminalWith(ops terminalOps, inFd int, out io.Writer, outFd int) func() {
	noop := func() {}
	if !ops.isTerminal(inFd) || !ops.isTerminal(outFd) {
		return noop
	}
	state, err := ops.getState(inFd)
	if err != nil {
		return noop
	}
	return func() {
		if !ops.isForeground(inFd) {
			return
		}
		// A terminal with `stty tostop` stops a writer with SIGTTOU when it is
		// not in the foreground. omac is checked to be the foreground above,
		// but ignore the signal anyway so a race cannot suspend omac here.
		prev := signalIgnore(syscall.SIGTTOU)
		defer signalReset(syscall.SIGTTOU, prev)
		_ = ops.restore(inFd, state)
		_, _ = io.WriteString(out, terminalResetSeq)
	}
}
