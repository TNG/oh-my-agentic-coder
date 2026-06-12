//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package sandbox

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// claimTerminalFor sets the controlling terminal's foreground process group
// to pgid (so terminal-driver-driven SIGINT/SIGQUIT/SIGTSTP from the
// keyboard are delivered there). It returns a non-nil restore function
// that must be called when the foreground child exits, even on the
// non-terminal path (then it's a no-op).
//
// Strategy:
//
//  1. Open /dev/tty for read+write. If that fails, we have no controlling
//     terminal; both the claim and the restore are no-ops.
//  2. Read the current foreground pgid via TIOCGPGRP.
//  3. Block SIGTTOU around tcsetpgrp, because tcsetpgrp from a non-foreground
//     pgid would otherwise stop us with SIGTTOU. Doing the call from within
//     a SIGTTOU block + ignoring the signal is the standard workaround used
//     by shells (bash, zsh, ksh) when they hand the terminal off to a job.
//  4. Set the new pgid.
//  5. The returned function reverses these steps when called.
func claimTerminalFor(pgid int) (tty *os.File, restore func()) {
	noop := func() {}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// No controlling terminal (daemonized, CI, redirected stdin, etc.)
		return nil, noop
	}
	fd := int(tty.Fd())

	prevPgid, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	if err != nil {
		_ = tty.Close()
		return nil, noop
	}

	// Ignore SIGTTOU so the imminent tcsetpgrp does not stop us.
	prevTTOU := signalIgnore(syscall.SIGTTOU)
	prevTTIN := signalIgnore(syscall.SIGTTIN)

	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, pgid); err != nil {
		// Best effort: restore signals and bail. Most likely cause is that
		// stdin/stdout were redirected to a non-tty even though /dev/tty
		// existed (it usually does on macOS even from launchd contexts).
		signalReset(syscall.SIGTTOU, prevTTOU)
		signalReset(syscall.SIGTTIN, prevTTIN)
		_ = tty.Close()
		return nil, noop
	}

	restore = func() {
		// Hand the terminal back to whoever owned it before us. Errors
		// here are non-fatal and not user-actionable.
		_ = unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, prevPgid)
		// Do NOT reset SIGTTOU/SIGTTIN back to SIG_DFL here. The
		// caller (ExecWithReady) runs this restore func via defer
		// after the child exits, while goroutines may still be
		// writing to the TTY (stderr). If we restore SIG_DFL, the
		// kernel re-raises SIGTTOU on every background TTY write,
		// and the Go runtime's signal handler spins on each
		// delivery — a busy-wait loop that pins a CPU until the
		// process finally exits. Keeping SIG_IGN until process exit
		// is harmless: omac is shutting down and no code path
		// benefits from SIGTTOU/SIGTTIN having their default (stop)
		// disposition during teardown.
		_ = tty.Close()
	}
	return tty, restore
}

// signalIgnore sets true SIG_IGN behaviour for the given signal and returns
// a token that signalReset can use to put it back.
//
// We use signal.Ignore (which installs the real SIG_IGN disposition) rather
// than signal.Notify with a drain goroutine. The latter installs an *active*
// Go-runtime handler that catches and queues every delivery of the signal —
// it does not ignore it. That distinction is critical here: after
// claimTerminalFor hands the terminal foreground to the child, omac becomes a
// background process whose stdio is still wired to the controlling tty. On
// Linux the terminal driver then re-raises SIGTTIN/SIGTTOU continuously on
// every background tty access. With a Notify+drain handler those repeated
// signals were dispatched into a channel and spun on in a tight `for range
// ch` loop, pinning a CPU until the child exited (and, if the child wedged
// after Ctrl-C, indefinitely). signal.Ignore drops the signals in the kernel
// with no per-delivery goroutine work, so there is nothing to busy-spin on.
type sigToken struct{ sig syscall.Signal }

func signalIgnore(sig syscall.Signal) sigToken {
	signal.Ignore(sig)
	return sigToken{sig: sig}
}

func signalReset(sig syscall.Signal, _ sigToken) {
	// Restore the default disposition for the signal. (We only ever ignore
	// SIGTTIN/SIGTTOU here, whose default action is to stop the process —
	// the correct behaviour for a foreground process touching the tty.)
	signal.Reset(sig)
}
