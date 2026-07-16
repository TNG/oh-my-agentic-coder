package updater

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// SelfReplacer atomically swaps the file at path with the content read from
// r, applying mode.
type SelfReplacer interface {
	Replace(path string, r io.Reader, mode fs.FileMode) error
}

type fsSelfReplacer struct{}

// Replace writes r to a temp file in the same directory as path (so the
// subsequent rename is atomic and never crosses a filesystem boundary), then
// renames it over path. A permission error creating the temp file (e.g. path
// lives in a root-owned directory) surfaces directly via errors.Is(err,
// fs.ErrPermission).
func (fsSelfReplacer) Replace(path string, r io.Reader, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".omac-update-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once Rename below succeeds

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}
