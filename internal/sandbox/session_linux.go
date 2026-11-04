//go:build linux

package sandbox

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// detachedSessionGroups returns the process groups of the new sessions that
// pid's direct children created with setsid. For bwrap --new-session that is
// the sandbox's init process, whose group the inner command shares. Only a
// child that leads its own session and group counts, so a child that never
// called setsid (or a reused pid) is not signalled. Best-effort: an
// unreadable /proc yields nil.
func detachedSessionGroups(pid int) []int {
	var groups []int
	for _, child := range childPIDs(pid) {
		if g, err := unix.Getpgid(child); err != nil || g != child {
			continue
		}
		if sid, err := unix.Getsid(child); err != nil || sid != child {
			continue
		}
		groups = append(groups, child)
	}
	return groups
}

// childPIDs lists pid's direct children. /proc/<pid>/task/<tid>/children
// lists the children each thread forked, so read it for every thread. It
// needs CONFIG_PROC_CHILDREN; without it, scan every process's parent pid.
func childPIDs(pid int) []int {
	files, _ := filepath.Glob(filepath.Join("/proc", strconv.Itoa(pid), "task", "*", "children"))
	if len(files) == 0 {
		return scanChildPIDs(pid)
	}
	var out []int
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, field := range strings.Fields(string(data)) {
			if c, err := strconv.Atoi(field); err == nil {
				out = append(out, c)
			}
		}
	}
	return out
}

// scanChildPIDs finds pid's direct children by reading every process's
// parent pid from /proc/<n>/stat.
func scanChildPIDs(pid int) []int {
	var out []int
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		c, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid, ok := parentPID(c); ok && ppid == pid {
			out = append(out, c)
		}
	}
	return out
}

// parentPID reads the ppid field of /proc/<pid>/stat. The command name
// (field 2) may contain spaces and parentheses, so parse after the last ')'.
// Example: "42 (my (cmd)) S 7 ..." has ppid 7.
func parentPID(pid int) (int, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, false
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	return ppid, err == nil
}
