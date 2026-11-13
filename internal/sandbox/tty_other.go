//go:build !(darwin || linux || freebsd || netbsd || openbsd || dragonfly)

package sandbox

import "os"

// claimTerminalFor is a no-op on platforms without POSIX tty foreground
// process groups (e.g. native Windows). The non-tty path of Exec is
// already what we want there: signal forwarding to the child process is
// the only mechanism we use.
func claimTerminalFor(pgid int) (*os.File, func()) {
	_ = pgid
	return nil, func() {}
}

// saveTerminal is a no-op where there is no POSIX terminal to restore.
func saveTerminal(in, out *os.File) func() {
	_, _ = in, out
	return func() {}
}
