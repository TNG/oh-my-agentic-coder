//go:build e2e || e2e_fast || vuln

package e2e

import (
	"os"
	"path/filepath"
	"runtime"
)

// sidecarLogGlob returns a glob pattern that matches omac sidecar log files
// produced by this user's running or recently-run sessions.
func sidecarLogGlob() string {
	base := sidecarLogBase()
	if base == "" {
		return ""
	}
	return filepath.Join(base, "omac-*", "logs", "*.log")
}

func sidecarLogBase() string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(os.TempDir(), "omac")
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "omac")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "omac", "run")
	}
	return ""
}
