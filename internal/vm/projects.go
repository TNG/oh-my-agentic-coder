package vm

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/TNG/oh-my-agentic-coder/internal/prefs"
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
// A key has to be the repository root that ProjectKey returns: a worktree
// or a subdirectory as a key never matches. Keys are returned canonicalized
// like ProjectKey's result, and two keys for one directory are an error.
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

// gitTimeout bounds the git call of ProjectKey, so a stalled network mount
// cannot hang omac.
var gitTimeout = 5 * time.Second

// ProjectKey is the path a directory is looked up by in the project map: the
// directory that holds the .git of its repository, so every worktree of one
// repository shares an entry, else the directory itself.
func ProjectKey(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		abs = dir
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", abs, "rev-parse", "--git-common-dir")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return canonicalPath(abs)
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(abs, common)
	}
	common = filepath.Clean(common)
	if filepath.Base(common) != ".git" {
		// A bare repository or a submodule: no work tree sits above it.
		return canonicalPath(abs)
	}
	return canonicalPath(filepath.Dir(common))
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
