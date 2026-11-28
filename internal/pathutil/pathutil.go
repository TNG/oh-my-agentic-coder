// Package pathutil provides shared path handling for the CLI, preferences and sandbox.
package pathutil

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// ExistingParentsWithin returns sorted, unique parent directories strictly
// below root. Inputs must be absolute; existing symlink parents are refused.
func ExistingParentsWithin(root string, paths []string) ([]string, error) {
	root = filepath.Clean(root)
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("parent root must be absolute")
	}
	prefix := strings.TrimSuffix(root, string(filepath.Separator)) + string(filepath.Separator)
	parents := map[string]bool{}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("protected path must be absolute: %s", path)
		}
		for dir := filepath.Dir(path); dir != root && strings.HasPrefix(dir, prefix); dir = filepath.Dir(dir) {
			if parents[dir] {
				continue
			}
			info, err := os.Lstat(dir)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || !info.IsDir() {
				return nil, fmt.Errorf("protected-path parent must be a real directory: %s", dir)
			}
			parents[dir] = true
		}
	}
	out := make([]string, 0, len(parents))
	for path := range parents {
		out = append(out, path)
	}
	slices.Sort(out)
	return out, nil
}
