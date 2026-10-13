//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// detachedSessionGroups returns the process groups of pid's direct children
// that differ from pgid, i.e. the groups a child created by calling setsid.
// For bwrap --new-session that is the sandbox's init process, whose group
// the inner command shares. Best-effort: an unreadable /proc yields nil.
func detachedSessionGroups(pid, pgid int) []int {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"))
	if err != nil {
		return nil
	}
	var groups []int
	for _, f := range strings.Fields(string(data)) {
		child, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		g, err := syscall.Getpgid(child)
		if err != nil || g == pgid {
			continue
		}
		groups = append(groups, g)
	}
	return groups
}
