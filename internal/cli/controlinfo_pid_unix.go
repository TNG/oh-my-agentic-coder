//go:build !windows

package cli

import (
	"os"
	"syscall"
)

// pidLiveAndOwned returns true when pid names a running process owned by the
// current user. A dead or foreign-uid pid returns false, as does pid <= 0.
func pidLiveAndOwned(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks liveness without disturbing the process.
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	// Confirm the process is owned by us via /proc on Linux or kill errno on others.
	return pidOwnedByCurrentUser(pid)
}
