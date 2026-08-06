//go:build linux

package procidentity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// identifyNative resolves a pid's identity on Linux by reading /proc.
//
//   - Executable: /proc/<pid>/exe symlink target (os.Readlink).
//   - MainClass: parsed from /proc/<pid>/cmdline (NUL-separated argv);
//     the Gradle daemon bootstrap class if present.
//   - StartIdentity: /proc/<pid>/stat field 22 (`starttime` in clock
//     ticks since boot) — opaque string the caller compares for equality.
//
// A sandbox that blocks /proc reads surfaces as ErrUnverifiable (so the
// caller fails closed); a dead pid surfaces as ErrNoSuchProcess.
func identifyNative(pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, ErrNoSuchProcess
	}
	procRoot := fmt.Sprintf("/proc/%d", pid)

	// Liveness + existence: stat the proc dir.
	if _, err := os.Stat(procRoot); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Identity{}, ErrNoSuchProcess
		}
		return Identity{}, fmt.Errorf("%w: stat %s: %v", ErrUnverifiable, procRoot, err)
	}

	exe, err := os.Readlink(filepath.Join(procRoot, "exe"))
	if err != nil {
		// A missing /proc/<pid>/exe (process exited between stat and
		// readlink, or sandbox blocks the symlink) is unverifiable, not
		// "no such process" — we already saw the dir.
		if errors.Is(err, os.ErrNotExist) {
			return Identity{}, ErrNoSuchProcess
		}
		return Identity{}, fmt.Errorf("%w: readlink exe: %v", ErrUnverifiable, err)
	}

	cmdlineBytes, err := os.ReadFile(filepath.Join(procRoot, "cmdline"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Identity{}, ErrNoSuchProcess
		}
		return Identity{}, fmt.Errorf("%w: read cmdline: %v", ErrUnverifiable, err)
	}
	mainClass := parseGradleMainClass(cmdlineBytes)

	statBytes, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Identity{}, ErrNoSuchProcess
		}
		return Identity{}, fmt.Errorf("%w: read stat: %v", ErrUnverifiable, err)
	}
	startTime, err := parseStartTime(string(statBytes))
	if err != nil {
		return Identity{}, fmt.Errorf("%w: parse stat starttime: %v", ErrUnverifiable, err)
	}

	return Identity{
		Executable:   exe,
		MainClass:    mainClass,
		StartIdentity: startTime,
	}, nil
}
