// Package plugin installs and detects omac's client-side harness bridge
// assets — currently the OpenCode Desktop multi-directory plugin
// (omac-multidir.ts), the OpenCode-side counterpart to `omac serve`.
//
// The canonical plugin source is embedded into the omac binary (see
// assets/omac-multidir.ts) so `omac plugin install` works in any folder,
// independent of the source tree it was built from.
//
// Plugins can be installed either project-locally (under a workdir's
// bridge directory, e.g. .opencode/plugins) or globally (under the
// harness's user config dir, e.g. ~/.config/opencode/plugins). OpenCode
// auto-loads both locations at startup.
package plugin

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
)

// MultiDirFileName is the on-disk name of the multidir plugin file inside
// a harness's bridge directory (e.g. ".opencode/plugins/omac-multidir.ts").
const MultiDirFileName = "omac-multidir.ts"

// multiDirSource is the canonical OpenCode multidir plugin, embedded at
// build time. It is kept byte-for-byte in sync with
// .opencode/plugins/omac-multidir.ts.
//
//go:embed assets/omac-multidir.ts
var multiDirSource []byte

// MultiDirSource returns the embedded plugin source bytes.
func MultiDirSource() []byte {
	out := make([]byte, len(multiDirSource))
	copy(out, multiDirSource)
	return out
}

// MultiDirPath returns the absolute path the multidir plugin occupies for
// a workdir given the harness's project-relative bridge directory
// (e.g. ".opencode/plugins"). bridgeDir must be non-empty.
func MultiDirPath(workdir, bridgeDir string) string {
	return filepath.Join(workdir, bridgeDir, MultiDirFileName)
}

// MultiDirPathIn returns the absolute path the multidir plugin occupies
// inside an already-absolute plugins directory (e.g. the global
// ~/.config/opencode/plugins). dir must be non-empty.
func MultiDirPathIn(dir string) string {
	return filepath.Join(dir, MultiDirFileName)
}

// IsMultiDirInstalled reports whether the multidir plugin file exists at
// the expected path under workdir. A non-nil error means the path could
// not be stat'd for a reason other than "does not exist" (e.g. a
// permission problem); callers may treat that as "unknown".
func IsMultiDirInstalled(workdir, bridgeDir string) (bool, error) {
	if bridgeDir == "" {
		return false, fmt.Errorf("harness has no bridge directory")
	}
	return isMultiDirInstalledIn(filepath.Join(workdir, bridgeDir))
}

// IsMultiDirInstalledIn reports whether the multidir plugin file exists
// in an already-absolute plugins directory (e.g. the global plugins dir).
func IsMultiDirInstalledIn(dir string) (bool, error) {
	if dir == "" {
		return false, fmt.Errorf("no plugins directory")
	}
	return isMultiDirInstalledIn(dir)
}

func isMultiDirInstalledIn(dir string) (bool, error) {
	_, err := os.Stat(filepath.Join(dir, MultiDirFileName))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// ConflictError is returned by the install routines when a differing
// plugin file already exists and force was not set. It carries the path
// so callers can tell the user exactly what to overwrite or delete,
// rather than suggesting a command that would hit the same guard again.
type ConflictError struct {
	Path string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("a different %s already exists at %s; re-run with --force to overwrite", MultiDirFileName, e.Path)
}

// InstallResult describes what an install call did, for friendly output.
type InstallResult struct {
	// Path is the absolute file path the plugin was written to.
	Path string
	// Overwrote is true when an existing plugin file was replaced.
	Overwrote bool
	// Unchanged is true when the file already existed with identical
	// content and was left untouched (no write performed).
	Unchanged bool
}

// InstallMultiDir writes the embedded multidir plugin into workdir's
// bridge directory, creating the directory tree as needed. The write is
// atomic (temp-file + rename).
//
// If the file already exists with identical content it is left as-is
// (Unchanged=true). If it exists with different content and force is
// false, an error is returned so the caller can warn the user instead of
// clobbering local edits; with force=true it is overwritten.
func InstallMultiDir(workdir, bridgeDir string, force bool) (InstallResult, error) {
	if bridgeDir == "" {
		return InstallResult{}, fmt.Errorf("harness has no bridge directory")
	}
	return installMultiDirIn(filepath.Join(workdir, bridgeDir), force)
}

// InstallMultiDirIn writes the embedded multidir plugin into an
// already-absolute plugins directory (e.g. ~/.config/opencode/plugins),
// with the same semantics as InstallMultiDir.
func InstallMultiDirIn(dir string, force bool) (InstallResult, error) {
	if dir == "" {
		return InstallResult{}, fmt.Errorf("no plugins directory")
	}
	return installMultiDirIn(dir, force)
}

func installMultiDirIn(dir string, force bool) (InstallResult, error) {
	dest := filepath.Join(dir, MultiDirFileName)

	overwrote := false
	if existing, err := os.ReadFile(dest); err == nil {
		if bytesEqual(existing, multiDirSource) {
			return InstallResult{Path: dest, Unchanged: true}, nil
		}
		if !force {
			return InstallResult{Path: dest}, &ConflictError{Path: dest}
		}
		overwrote = true
	} else if !os.IsNotExist(err) {
		return InstallResult{}, fmt.Errorf("stat existing plugin: %w", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("create plugins dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, MultiDirFileName+".tmp-*")
	if err != nil {
		return InstallResult{}, fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(multiDirSource); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return InstallResult{}, fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return InstallResult{}, fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return InstallResult{}, fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		os.Remove(tmpPath)
		return InstallResult{}, fmt.Errorf("rename plugin into place: %w", err)
	}
	return InstallResult{Path: dest, Overwrote: overwrote}, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
