//go:build darwin

package sandboxrun

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// sampleOpenDirs returns the directories of files currently open by
// the process group of pid, via lsof (ships with macOS). Best-effort:
// errors yield an empty sample.
func sampleOpenDirs(pid int) []string {
	// -g targets the process group; -Fn emits machine-readable name
	// fields ("n/path"). lsof exits non-zero when some fds are
	// inaccessible — ignore the exit code and parse what we got.
	out, _ := exec.Command("lsof", "-a", "-g", strconv.Itoa(pid), "-d", "0-999", "-Fn").Output()
	return parseLsofDirs(string(out))
}

// parseLsofDirs extracts directory paths from `lsof -Fn` output.
func parseLsofDirs(out string) []string {
	seen := map[string]bool{}
	var dirs []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "n/") {
			continue
		}
		path := line[1:]
		// Reduce files to their directory; keep dirs as-is. We cannot
		// stat reliably (file may be gone) — use a heuristic: lsof
		// reports both; trimming to Dir for everything is fine because
		// the aggregation collapses ancestors anyway.
		dir := filepath.Dir(path)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
	}
	return dirs
}
