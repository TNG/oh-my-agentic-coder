//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package sandbox

import (
	"errors"
	"strings"
	"testing"

	"golang.org/x/term"
)

// fakeTerminal records what saveTerminalWith does to a pretend terminal.
type fakeTerminal struct {
	terminal   map[int]bool
	foreground bool
	getErr     error
	restored   int
}

func (f *fakeTerminal) ops() terminalOps {
	return terminalOps{
		isTerminal:   func(fd int) bool { return f.terminal[fd] },
		getState:     func(int) (*term.State, error) { return &term.State{}, f.getErr },
		restore:      func(int, *term.State) error { f.restored++; return nil },
		isForeground: func(int) bool { return f.foreground },
	}
}

func TestSaveTerminalRestoresModesAndResetsTUIState(t *testing.T) {
	f := &fakeTerminal{terminal: map[int]bool{0: true, 1: true}, foreground: true}
	var out strings.Builder
	saveTerminalWith(f.ops(), 0, &out, 1)()

	if f.restored != 1 {
		t.Fatalf("termios restored %d times, want 1", f.restored)
	}
	if out.String() != terminalResetSeq {
		t.Fatalf("wrote %q, want %q", out.String(), terminalResetSeq)
	}
}

func TestSaveTerminalSkipsWhenNotInteractive(t *testing.T) {
	cases := map[string]map[int]bool{
		"stdin redirected":  {1: true},
		"stdout redirected": {0: true},
	}
	for name, ttys := range cases {
		t.Run(name, func(t *testing.T) {
			f := &fakeTerminal{terminal: ttys, foreground: true}
			var out strings.Builder
			saveTerminalWith(f.ops(), 0, &out, 1)()
			if f.restored != 0 || out.Len() != 0 {
				t.Fatalf("touched a non-interactive terminal: restored=%d wrote=%q", f.restored, out.String())
			}
		})
	}
}

func TestSaveTerminalSkipsWhenStateUnreadable(t *testing.T) {
	f := &fakeTerminal{terminal: map[int]bool{0: true, 1: true}, foreground: true, getErr: errors.New("ioctl")}
	var out strings.Builder
	saveTerminalWith(f.ops(), 0, &out, 1)()
	if f.restored != 0 || out.Len() != 0 {
		t.Fatalf("restored=%d wrote=%q, want nothing", f.restored, out.String())
	}
}

// omac in the background must not change the terminal under another job.
func TestSaveTerminalSkipsWhenNotForeground(t *testing.T) {
	f := &fakeTerminal{terminal: map[int]bool{0: true, 1: true}, foreground: false}
	var out strings.Builder
	saveTerminalWith(f.ops(), 0, &out, 1)()
	if f.restored != 0 || out.Len() != 0 {
		t.Fatalf("restored=%d wrote=%q, want nothing", f.restored, out.String())
	}
}

// Every sequence must be a mode reset ("l") or cursor show, never something
// that moves the cursor or clears the screen after a well-behaved program.
func TestTerminalResetSeqOnlyResetsModes(t *testing.T) {
	for _, part := range strings.Split(terminalResetSeq, "\x1b")[1:] {
		if !strings.HasPrefix(part, "[?") {
			t.Fatalf("sequence %q is not a private mode switch", part)
		}
		if part != "[?25h" && !strings.HasSuffix(part, "l") {
			t.Fatalf("sequence %q enables a mode", part)
		}
		if part == "[?1049l" || part == "[?1047l" {
			t.Fatalf("sequence %q leaves the alternate screen and may move the cursor", part)
		}
	}
}
