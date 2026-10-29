package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/pathutil"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

// resolveGitProjectRoot returns the canonical root of the repository that
// contains workdir. It uses git discovery (walk up to the first .git entry)
// rather than `git rev-parse --show-toplevel`, because `--show-toplevel` is
// driven by core.worktree: that key lives in a config file the sandboxed agent
// can write, so it must never choose the scope. The root is the directory
// whose `.git` entry physically names the repository.
//
// A linked worktree maps to the main repository, so every worktree shares the
// same project scope. A submodule, separate git dir, or unvalidated gitfile
// stays at the directory that holds the `.git` file, never an ancestor.
//
// It returns false for a non-git checkout, a bare repository, a root that does
// not contain workdir, the filesystem root, the user's home directory, and a
// too-shallow ancestor. Those refusals are the security boundary: an agent that
// forges `.git` or `core.worktree` must not scope an exception to another
// project.
func resolveGitProjectRoot(workdir string) (string, bool) {
	absWorkdir, err := filepath.Abs(workdir)
	if err != nil {
		return "", false
	}
	absWorkdir = pathutil.Canonical(absWorkdir)
	if absWorkdir == "" {
		return "", false
	}

	root, ok := discoverRepoRoot(absWorkdir)
	if !ok {
		return "", false
	}
	root = pathutil.Canonical(root)
	if !acceptableProjectRoot(root, absWorkdir) {
		return "", false
	}
	return root, true
}

// discoverRepoRoot walks up from dir to the first `.git` entry and maps it to a
// repository root. It deliberately never reads core.worktree.
func discoverRepoRoot(dir string) (string, bool) {
	var dotgit string
	for {
		candidate := filepath.Join(dir, ".git")
		if _, err := os.Lstat(candidate); err == nil {
			dotgit = candidate
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}

	fi, err := os.Stat(dotgit)
	if err != nil {
		return "", false
	}
	if fi.IsDir() {
		// A normal checkout: the directory holding `.git` is the root.
		return dir, true
	}

	// A `.git` file is a linked worktree, a submodule, or a separate git dir.
	// Only a validated linked worktree maps to the main repository; anything
	// else stays at the directory that contains the file.
	if common, _, ok := sandboxrun.ResolveWorktreeCommonDir(dir); ok {
		return filepath.Dir(common), true
	}
	return dir, true
}

// acceptableProjectRoot applies the containment and breadth gates. The root
// must contain workdir, and it must not be the filesystem root, the user's home
// directory, or a strict ancestor of workdir with fewer than two components.
func acceptableProjectRoot(root, workdir string) bool {
	if root == "" || root == string(filepath.Separator) {
		return false
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && root == pathutil.Canonical(home) {
		return false
	}
	if !pathutil.Under(root, workdir) {
		return false
	}
	if root != workdir && pathDepth(root) < 2 {
		return false
	}
	return true
}

// pathDepth counts the path components below the volume root. "/" is 0,
// "/Users" is 1, "/Users/me" is 2.
func pathDepth(path string) int {
	path = filepath.Clean(path)
	path = strings.TrimPrefix(path, filepath.VolumeName(path))
	path = strings.Trim(path, string(filepath.Separator))
	if path == "" {
		return 0
	}
	return len(strings.Split(path, string(filepath.Separator)))
}
