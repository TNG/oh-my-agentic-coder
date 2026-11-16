// Package pathutil holds the small path helpers shared by the CLI and the
// user-global preferences: symlink-aware canonicalisation and a
// separator-boundary containment check.
package pathutil

import (
	"path/filepath"
	"strings"
)

// Canonical resolves symlinks in the deepest existing ancestor and re-grafts
// the remaining tail, so a path that does not exist yet still maps
// /var/... to /private/var/... on macOS. It returns "" for an empty input.
func Canonical(path string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	dir, tail := filepath.Dir(path), filepath.Base(path)
	for dir != "." {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, tail)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Volume root: stop instead of spinning on a path whose
			// parent is itself (filepath.Dir("C:\\") == "C:\\").
			break
		}
		dir, tail = parent, filepath.Join(filepath.Base(dir), tail)
	}
	return path
}

// Under reports whether path is root or lies below it. Both sides are
// canonicalised, and the separator boundary keeps /repo-other from matching a
// root of /repo.
func Under(root, path string) bool {
	root = Canonical(root)
	path = Canonical(path)
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
