package audit

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// defaultLogName is the single growing file that accumulates the trail
// across restarts. A per-event run_id keeps runs distinguishable.
const defaultLogName = "audit.jsonl"

// DefaultDir resolves the persistent, central, host-level directory for the
// audit trail. It must survive restarts and live outside the ephemeral
// per-run runtime dir. Resolution (design D7):
//
//   - Linux: $XDG_STATE_HOME/omac/audit, else $HOME/.local/state/omac/audit
//   - macOS: $HOME/Library/Logs/omac/audit
//   - fallback (no home): ${TMPDIR}/omac/audit, with persistNotGuaranteed=true
//
// The returned dir is not yet created; DefaultPath / the sink create it.
func DefaultDir() (dir string, persistNotGuaranteed bool) {
	if runtime.GOOS == "darwin" {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			return filepath.Join(home, "Library", "Logs", "omac", "audit"), false
		}
		return filepath.Join(os.TempDir(), "omac", "audit"), true
	}
	// Linux / other unix: XDG state dir.
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "omac", "audit"), false
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "state", "omac", "audit"), false
	}
	return filepath.Join(os.TempDir(), "omac", "audit"), true
}

// DefaultPath returns the default audit log file path (DefaultDir joined
// with the default filename) and whether persistence is guaranteed. The
// directory is not created here.
func DefaultPath() (path string, persistNotGuaranteed bool) {
	dir, warn := DefaultDir()
	return filepath.Join(dir, defaultLogName), warn
}

// EffectivePath returns the resolved log path for a Config: the explicit
// Path when set, else DefaultPath. Returns "" when auditing is disabled.
// Callers use this to pass the same path down to the sandbox subprocess.
func EffectivePath(cfg Config) string {
	if !cfg.Enabled {
		return ""
	}
	if cfg.Path != "" {
		return cfg.Path
	}
	p, _ := DefaultPath()
	return p
}

// ensureDir creates the parent directory of path with mode 0700.
func ensureDir(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("audit: create dir %s: %w", dir, err)
	}
	return nil
}
