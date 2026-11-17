package vm

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"

	"github.com/TNG/oh-my-agentic-coder/internal/prefs"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

// ProjectMapPath is ~/.config/omac/vm-projects.yaml (honoring
// $XDG_CONFIG_HOME), or "" when no global config directory resolves.
func ProjectMapPath() string {
	dir := prefs.GlobalDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "vm-projects.yaml")
}

// LoadProjectMap reads the file that maps a project path to the name of the
// VM its sessions use. A missing file is an empty map. Keys must be absolute
// paths and values valid VM names; any other content is an error naming path.
// Keys are returned canonicalized, and two keys for one directory are an
// error.
func LoadProjectMap(path string) (map[string]string, error) {
	if path == "" {
		return map[string]string{}, nil
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("omac vm: read %s: %w", path, err)
	}
	var m map[string]string
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("omac vm: parse %s: %w", path, err)
	}
	out := make(map[string]string, len(m))
	keyOf := make(map[string]string, len(m))
	for _, k := range slices.Sorted(maps.Keys(m)) {
		name := m[k]
		if !filepath.IsAbs(k) {
			return nil, fmt.Errorf("omac vm: %s: key %q is not an absolute path", path, k)
		}
		if err := ValidateName(name); err != nil {
			return nil, fmt.Errorf("omac vm: %s: entry %q: %w", path, k, err)
		}
		c := canonicalPath(k)
		if prev, dup := keyOf[c]; dup {
			return nil, fmt.Errorf("omac vm: %s: keys %q and %q name the same directory %s", path, prev, k, c)
		}
		keyOf[c], out[c] = k, name
	}
	return out, nil
}

// canonicalPath cleans p and resolves symlinks, so /tmp and /private/tmp
// compare equal; a path that does not exist keeps its cleaned form.
func canonicalPath(p string) string {
	p = filepath.Clean(p)
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// lookupPaths are the paths whose entries apply to dir, most specific
// first: dir itself, then the main repository of each linked worktree dir
// lies in. A worktree counts only when its .git is a regular file that
// sandboxrun.ResolveWorktreeCommonDir verifies: the sandbox can write the
// project, and a symlink or a planted pointer must not lead to another
// repository's entry.
func lookupPaths(dir string) []string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	abs = canonicalPath(abs)
	paths := []string{abs}
	for d := abs; ; d = filepath.Dir(d) {
		if fi, err := os.Lstat(filepath.Join(d, ".git")); err == nil && fi.Mode().IsRegular() {
			if common, _, ok := sandboxrun.ResolveWorktreeCommonDir(d); ok && filepath.Base(common) == ".git" {
				paths = append(paths, canonicalPath(filepath.Dir(common)))
			}
		}
		if filepath.Dir(d) == d {
			return paths
		}
	}
}

// MappedInstance returns the VM the project map assigns to dir: the entry
// with the longest key that is dir or one of its parents, tried for each
// of lookupPaths in turn. ok is false when no entry applies.
func MappedInstance(dir string) (name string, ok bool, err error) {
	m, err := LoadProjectMap(ProjectMapPath())
	if err != nil || len(m) == 0 {
		return "", false, err
	}
	for _, p := range lookupPaths(dir) {
		for d := p; ; d = filepath.Dir(d) {
			if name, ok := m[d]; ok {
				return name, true, nil
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	return "", false, nil
}
