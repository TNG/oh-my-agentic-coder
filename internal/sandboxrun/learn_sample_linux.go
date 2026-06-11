//go:build linux

package sandboxrun

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// sampleOpenDirs returns the directories of files currently open by
// pid's process tree, by walking /proc/<pid>/fd symlinks. Best-effort.
func sampleOpenDirs(pid int) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, p := range processTree(pid) {
		fdDir := filepath.Join("/proc", strconv.Itoa(p), "fd")
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
			if err != nil || !strings.HasPrefix(target, "/") {
				continue // pipes, sockets, anon inodes
			}
			dir := filepath.Dir(target)
			if !seen[dir] {
				seen[dir] = true
				dirs = append(dirs, dir)
			}
		}
	}
	return dirs
}

// processTree returns pid plus all descendant pids (via /proc children).
func processTree(root int) []int {
	out := []int{root}
	for i := 0; i < len(out); i++ {
		childrenFile := filepath.Join("/proc", strconv.Itoa(out[i]), "task", strconv.Itoa(out[i]), "children")
		data, err := os.ReadFile(childrenFile)
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(data)) {
			if pid, err := strconv.Atoi(f); err == nil {
				out = append(out, pid)
			}
		}
	}
	return out
}
