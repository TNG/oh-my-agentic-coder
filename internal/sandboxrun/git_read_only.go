package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/TNG/oh-my-agentic-coder/internal/pathutil"
)

// ValidateReadOnlyGit rejects grants that could expose a writable metadata alias.
func (g *Grants) ValidateReadOnlyGit() error {
	if !slices.Contains(g.AllowPaths, g.Workdir) {
		return fmt.Errorf("Git prototype requires a read-write workdir grant")
	}
	for _, p := range append(append([]string{}, g.AllowPaths...), g.WritePaths...) {
		if p == "/" {
			return fmt.Errorf("Git prototype does not support a writable root grant")
		}
		if p != g.Workdir && isUnder(g.Workdir, p) {
			return fmt.Errorf("Git prototype does not support writable ancestors of the checkout: %s", p)
		}
		for _, tools := range []string{"/usr", "/lib", "/lib64", "/bin", "/sbin"} {
			if isUnder(p, tools) || isUnder(tools, p) {
				return fmt.Errorf("Git prototype requires read-only system executables: %s", p)
			}
		}
		real, err := filepath.EvalSymlinks(p)
		if err == nil && real != p {
			return fmt.Errorf("Git prototype does not support aliased writable grants: %s", p)
		}
	}
	gitdir := filepath.Join(g.Workdir, ".git")
	info, err := os.Lstat(gitdir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("Git prototype requires a real .git directory")
	}
	return validateGitMountAliases(g)
}

// The service reuses launch-time deny paths. Pin their parents so the agent
// cannot move a masked secret to a path that a later worker would leave unmasked.
func pinGitDenyParents(g *Grants, mounts map[string]*mount) error {
	parents, err := pathutil.ExistingParentsWithin(g.Workdir, g.ProtectedPaths)
	if err != nil {
		return err
	}
	for _, dir := range parents {
		if _, exists := mounts[dir]; exists {
			continue
		}
		var covering *mount
		for _, m := range mounts {
			if isUnder(dir, m.path) && (covering == nil || len(m.path) > len(covering.path)) {
				covering = m
			}
		}
		if covering != nil {
			mounts[dir] = &mount{path: dir, rw: covering.rw}
		}
	}
	return nil
}
