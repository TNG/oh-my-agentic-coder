package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// gitRootTimeout bounds the git call that resolves a project root. The watch
// runs it only when the first decision dialog opens, so a hung git must not
// hold the dialog or the scan loop.
const gitRootTimeout = 2 * time.Second

// resolveGitProjectRoot returns the canonical root of the repository that
// contains workdir. It uses the git common dir so every linked worktree maps
// to the main repository, which is the scope a project exception needs.
//
// It returns false when workdir is not a git checkout, when git fails or times
// out, for a bare repository, or when the reported root does not contain
// workdir. The last check is the security boundary: a forged .git or
// core.worktree must not scope an exception to another project.
func resolveGitProjectRoot(workdir string) (string, bool) {
	absWorkdir, err := filepath.Abs(workdir)
	if err != nil {
		return "", false
	}
	absWorkdir = canonicalPath(absWorkdir)

	ctx, cancel := context.WithTimeout(context.Background(), gitRootTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", absWorkdir, "rev-parse",
		"--path-format=absolute", "--git-common-dir", "--show-toplevel")
	cmd.Env = gitRootEnv()
	// A killed helper may leave a child holding the stdout pipe. Bound that
	// wait so the dialog cannot hang on a broken git.
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		// Non-git workdir, bare repository (no --show-toplevel), timeout,
		// or a dubious-ownership refusal: no project scope.
		return "", false
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return "", false
	}
	commonDir := strings.TrimSpace(lines[0])
	topLevel := strings.TrimSpace(lines[len(lines)-1])
	if commonDir == "" || topLevel == "" {
		return "", false
	}

	// The common dir of a normal checkout and of a linked worktree is
	// <root>/.git, so its parent is the main repository. A submodule's
	// common dir lives under .git/modules/<name>, where the parent is not
	// the project root, so fall back to the worktree's own toplevel there.
	root := topLevel
	if filepath.Base(commonDir) == ".git" {
		root = filepath.Dir(commonDir)
	}
	root = canonicalPath(root)
	if root == "" || !pathUnder(root, absWorkdir) {
		return "", false
	}
	return root, true
}

// gitRootEnv keeps the user's git setup but removes the variables that could
// point the check at another repository. GIT_TERMINAL_PROMPT=0 keeps a broken
// credential helper from blocking the dialog.
func gitRootEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_DIR=") ||
			strings.HasPrefix(entry, "GIT_WORK_TREE=") ||
			strings.HasPrefix(entry, "GIT_COMMON_DIR=") {
			continue
		}
		out = append(out, entry)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0")
}

// pathUnder reports whether path is root or lies below it. The separator
// boundary keeps /repo-other from matching a root of /repo.
func pathUnder(root, path string) bool {
	root = canonicalPath(root)
	path = canonicalPath(path)
	if root == "" || path == "" {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// canonicalPath resolves symlinks in the deepest existing ancestor and
// re-grafts the remaining tail, so a path that does not exist yet still maps
// /var/... to /private/var/... on macOS.
func canonicalPath(path string) string {
	if path == "" {
		return ""
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	dir, tail := filepath.Dir(path), filepath.Base(path)
	for dir != "/" && dir != "." {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, tail)
		}
		dir, tail = filepath.Dir(dir), filepath.Join(filepath.Base(dir), tail)
	}
	return path
}
