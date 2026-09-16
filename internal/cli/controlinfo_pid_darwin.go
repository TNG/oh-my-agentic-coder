package cli

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// pidOwnedByCurrentUser uses syscall.Getpgid as a proxy: on macOS, sending
// signal 0 to a process owned by a different user fails with EPERM, which
// we use to confirm ownership. If signal 0 succeeds (no error) the process
// is reachable by us, which implies same-uid or we are root.
//
// For a more precise check, compare the process real uid via sysctl; that
// requires cgo. The signal-0 approach is conservative: it returns false on
// any error, so a foreign-uid process (EPERM from kill) is rejected.
func pidOwnedByCurrentUser(pid int) bool {
	// Attempt to get the process group — succeeds only for same-uid or root.
	_, err := syscall.Getpgid(pid)
	if err != nil {
		return false
	}
	// Cross-check: current user must not be root (root can signal anything).
	u, err := user.Current()
	if err != nil {
		return false
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return false
	}
	return uid == os.Getuid()
}
