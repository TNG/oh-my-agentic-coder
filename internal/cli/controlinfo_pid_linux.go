package cli

import (
	"fmt"
	"os"
	"strings"
)

// pidOwnedByCurrentUser checks /proc/<pid>/status for the Uid line and
// compares the real UID against os.Getuid().
func pidOwnedByCurrentUser(pid int) bool {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return false
	}
	uid := os.Getuid()
	want := fmt.Sprintf("%d", uid)
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		// Format: "Uid:\treal\teffective\tsaved\tfs"
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == want {
			return true
		}
		return false
	}
	return false
}
