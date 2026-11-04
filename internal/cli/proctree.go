package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// processParents returns pid → ppid for all visible processes: /proc on Linux, ps elsewhere; nil when neither works.
func processParents() map[int]int {
	if entries, err := os.ReadDir("/proc"); err == nil {
		parents := map[int]int{}
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
			if err != nil {
				continue
			}
			if ppid, ok := parseProcStatPPID(string(raw)); ok {
				parents[pid] = ppid
			}
		}
		if len(parents) > 0 {
			return parents
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-A", "-o", "pid=", "-o", "ppid=").Output()
	if err != nil {
		return nil
	}
	return parsePsParents(string(out))
}

// parseProcStatPPID reads ppid from /proc/<pid>/stat ("pid (comm) state ppid ..."); comm may contain spaces and parens.
func parseProcStatPPID(stat string) (int, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return 0, false
	}
	fields := strings.Fields(stat[i+1:])
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	return ppid, err == nil
}

// parsePsParents parses "pid ppid" lines.
func parsePsParents(out string) map[int]int {
	parents := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			parents[pid] = ppid
		}
	}
	return parents
}

// descendants returns every process below root in parents, root excluded.
func descendants(root int, parents map[int]int) []int {
	children := map[int][]int{}
	for pid, ppid := range parents {
		if pid != ppid {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var out []int
	seen := map[int]bool{root: true}
	queue := []int{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range children[cur] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				queue = append(queue, c)
			}
		}
	}
	return out
}

// stopSandboxTree ends a refused launch: every process below the sandbox child root, including ones that left
// its process group (OpenCode starts its service detached), then root itself.
//
// The tree is frozen with SIGSTOP first and re-scanned until no new process shows up, because OpenCode's client
// starts a new service when the old one exits; killing in place can leave a fresh, orphaned service behind.
// The frozen processes get SIGKILL; root (omac sandbox run) gets SIGTERM to exit cleanly, SIGKILL after 3s.
func stopSandboxTree(exited <-chan struct{}, root int) {
	frozen := map[int]bool{}
	for range 5 {
		fresh := false
		for _, pid := range descendants(root, processParents()) {
			if !frozen[pid] {
				frozen[pid] = true
				fresh = true
				_ = syscall.Kill(pid, syscall.SIGSTOP)
			}
		}
		if !fresh {
			break
		}
	}
	for pid := range frozen {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	_ = syscall.Kill(-root, syscall.SIGTERM)
	// exited ends when the caller has reaped root, so its pid cannot have been reused before the SIGKILL.
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-root, syscall.SIGKILL)
	}
}
