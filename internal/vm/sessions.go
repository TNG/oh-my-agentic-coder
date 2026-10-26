package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"
)

// RegisterSession records pid as attached. The marker is host-owned state
// (never granted to a sandbox), so a session cannot forge or remove it.
func RegisterSession(l Layout, pid int) (func(), error) {
	if err := os.MkdirAll(l.SessionsDir, 0o700); err != nil {
		return nil, fmt.Errorf("omac vm: sessions dir: %w", err)
	}
	p := filepath.Join(l.SessionsDir, strconv.Itoa(pid))
	if err := os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("omac vm: session marker: %w", err)
	}
	return func() { _ = os.Remove(p) }, nil
}

// AttachedSessions lists pids with a live marker, sorted. It removes markers
// of dead processes (a crashed session leaves one behind) and entries whose
// name is not a pid. A missing sessions directory means no sessions.
func AttachedSessions(l Layout, alive func(int) bool) ([]int, error) {
	entries, err := os.ReadDir(l.SessionsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("omac vm: read sessions dir: %w", err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || !alive(pid) {
			_ = os.Remove(filepath.Join(l.SessionsDir, e.Name()))
			continue
		}
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	return pids, nil
}

// ProcessAlive reports whether pid exists. EPERM means it exists but
// belongs to another user.
func ProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
