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
// A key has to be a path ProjectKey returns, such as a repository's root:
// a subdirectory or a linked worktree as a key never matches. Keys are
// returned canonicalized like ProjectKey's result, and two keys for one
// directory are an error.
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

// ProjectKey is the path a directory is looked up by in the project map:
// the nearest directory at or above dir that holds a .git, else dir itself.
// A linked worktree maps to the directory of its repository's .git instead,
// so all worktrees of a repository share an entry. A .git file counts as a
// linked worktree only when sandboxrun.ResolveWorktreeCommonDir verifies it:
// the sandbox can write .git, and git would follow a planted commondir or
// gitdir into another repository, and so into that repository's VM.
func ProjectKey(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	for d := abs; ; d = filepath.Dir(d) {
		fi, err := os.Lstat(filepath.Join(d, ".git"))
		if err == nil {
			if !fi.IsDir() {
				if common, _, ok := sandboxrun.ResolveWorktreeCommonDir(d); ok && filepath.Base(common) == ".git" {
					return canonicalPath(filepath.Dir(common))
				}
			}
			return canonicalPath(d)
		}
		if filepath.Dir(d) == d {
			return canonicalPath(abs)
		}
	}
}

// MappedInstance returns the VM the project map assigns to dir's repository;
// ok is false when there is no entry.
func MappedInstance(dir string) (name string, ok bool, err error) {
	m, err := LoadProjectMap(ProjectMapPath())
	if err != nil || len(m) == 0 {
		return "", false, err
	}
	name, ok = m[ProjectKey(dir)]
	return name, ok, nil
}
