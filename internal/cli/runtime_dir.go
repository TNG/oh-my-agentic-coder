package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// runtimeDirBase returns the host-only directory under which omac creates
// per-session runtime directories. It is never mounted into any sandbox.
//
//   - Linux: $XDG_RUNTIME_DIR/omac when set, else $HOME/.local/state/omac/run
//   - macOS: $TMPDIR/omac — launchd sets $TMPDIR to a per-user 0700 directory
//     under /var/folders/…/T/, keeping the socket path well under the 104-char
//     macOS Unix socket limit. This is not the shared /tmp.
func runtimeDirBase() (string, error) {
	if runtime.GOOS == "darwin" {
		return filepath.Join(os.TempDir(), "omac"), nil
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "omac"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "", fmt.Errorf("runtime dir: cannot resolve home: %w", err)
	}
	return filepath.Join(home, ".local", "state", "omac", "run"), nil
}

// newRuntimeDir creates a fresh per-session runtime directory with an
// unpredictable name under base, then creates the requested subdirs.
// It never silently adopts a pre-existing directory: if one exists and
// RemoveAll fails the function returns an error.
func newRuntimeDir(base, prefix string, subdirs []string) (string, error) {
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", fmt.Errorf("runtime dir base: %w", err)
	}
	dir, err := os.MkdirTemp(base, prefix)
	if err != nil {
		return "", fmt.Errorf("runtime dir: %w", err)
	}
	for _, sub := range subdirs {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o700); err != nil {
			_ = os.RemoveAll(dir)
			return "", fmt.Errorf("runtime dir subdir %s: %w", sub, err)
		}
	}
	return dir, nil
}
