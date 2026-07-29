package buildrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolved is a build request whose wrapper and project root have been
// verified against the canonical worktree: every path is absolute,
// symlink-resolved, and contained.
type Resolved struct {
	// Worktree is the canonical (EvalSymlinks) worktree root.
	Worktree string
	// ProjectDir is the canonical project root (--root resolved) — the
	// build's working directory.
	ProjectDir string
	// Wrapper is the canonical path of the repository-owned gradlew.
	Wrapper string
	// Args are the pass-through adapter arguments.
	Args []string
}

// Resolve canonicalizes the worktree and the requested root, enforces that
// the root lies inside the worktree (traversal and symlink escapes both
// rejected), and validates the repository-owned Gradle wrapper at
// <root>/gradlew: it must be an executable regular file, itself contained
// in the canonical worktree.
//
// The containment checks are against the canonical (EvalSymlinks-resolved)
// worktree root, so a root that textually sits inside the workdir but
// escapes through a symlink is rejected alongside plain ../ traversal.
func Resolve(workdir string, req Request) (Resolved, error) {
	wt, err := canonicalRoot(workdir)
	if err != nil {
		return Resolved{}, &RequestError{msg: fmt.Sprintf("canonicalize worktree %q: %v", workdir, err)}
	}

	// Root: may be relative (joined to the worktree) or absolute; Clean
	// first to collapse traversal, then contain-check against canonical.
	rootCandidate := req.Root
	if !filepath.IsAbs(rootCandidate) {
		rootCandidate = filepath.Join(wt, rootCandidate)
	}
	rootCandidate = filepath.Clean(rootCandidate)
	// Lexical containment first, so traversal/absolute escapes are
	// diagnosed as such even when the target does not exist.
	if !pathWithin(wt, rootCandidate) {
		return Resolved{}, &RequestError{msg: fmt.Sprintf(
			"build root %q resolves to %s, which is outside the worktree at %s; the root must lie inside the current worktree", req.Root, rootCandidate, wt)}
	}
	// A nonexistent root cannot be EvalSymlinks'd; existence is required
	// anyway because the wrapper must exist under it.
	if _, err := os.Lstat(rootCandidate); err != nil {
		return Resolved{}, &RequestError{msg: fmt.Sprintf("build root %q: %v", req.Root, err)}
	}
	root, err := filepath.EvalSymlinks(rootCandidate)
	if err != nil {
		return Resolved{}, &RequestError{msg: fmt.Sprintf("canonicalize build root %q: %v", req.Root, err)}
	}
	if !pathWithin(wt, root) {
		if rootCandidate != root && pathWithin(wt, rootCandidate) {
			return Resolved{}, &RequestError{msg: fmt.Sprintf(
				"build root %q resolves through a symlink to %s, which is outside the worktree; refusing to build from an escaped path", req.Root, root)}
		}
		return Resolved{}, &RequestError{msg: fmt.Sprintf(
			"build root %q resolves to %s, which is outside the worktree at %s; the root must lie inside the current worktree", req.Root, root, wt)}
	}

	wrapperCandidate := filepath.Join(root, "gradlew")
	fi, err := os.Lstat(wrapperCandidate)
	if err != nil {
		return Resolved{}, &RequestError{msg: fmt.Sprintf(
			"no repository-owned gradlew at %s: %v (the gradle adapter runs the worktree's wrapper, never a caller-supplied or host binary)", wrapperCandidate, err)}
	}
	wrapper, err := filepath.EvalSymlinks(wrapperCandidate)
	if err != nil {
		return Resolved{}, &RequestError{msg: fmt.Sprintf("canonicalize wrapper %q: %v", wrapperCandidate, err)}
	}
	if !pathWithin(wt, wrapper) {
		return Resolved{}, &RequestError{msg: fmt.Sprintf(
			"wrapper %q resolves through a symlink to %s, outside the worktree; refusing to execute an escaped wrapper", wrapperCandidate, wrapper)}
	}
	if fi.IsDir() || !fi.Mode().IsRegular() {
		return Resolved{}, &RequestError{msg: fmt.Sprintf("wrapper %q is not a regular file", wrapperCandidate)}
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return Resolved{}, &RequestError{msg: fmt.Sprintf(
			"wrapper %q is not executable (chmod +x %s)", wrapperCandidate, wrapperCandidate)}
	}

	return Resolved{
		Worktree:   wt,
		ProjectDir: root,
		Wrapper:    wrapper,
		Args:       req.Args,
	}, nil
}

// canonicalRoot returns the canonical absolute path of the worktree root.
func canonicalRoot(workdir string) (string, error) {
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// pathWithin reports whether p is root itself or lies beneath it
// (both are expected to be canonical absolute paths).
func pathWithin(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}
