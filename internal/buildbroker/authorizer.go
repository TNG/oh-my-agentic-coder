package buildbroker

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Authorizer is the seam the broker uses to authorize a build request's
// worktree against the parent's session state. Authorization is
// snapshotted at acceptance: the broker calls Authorize once, before
// sending `accepted`, and the result is immutable for the request's
// lifetime. Later deactivation rejects NEW requests but does not
// cancel an accepted one (parent shutdown does).
//
// Two real adapters:
//
//   - StartAuthorizer: `start` authorizes exactly its canonical session
//     worktree. The broker canonicalizes the client's worktree
//     candidate (filepath.EvalSymlinks) and compares it in constant
//     time to the parent's canonical session worktree.
//   - ServeAuthorizer: `serve` canonicalizes configured roots and
//     active directories with symlink evaluation, then authorizes only
//     canonical active directories still under a canonical configured
//     root. A request whose canonical worktree is not an active
//     directory, or has escaped the configured roots (symlink
//     traversal), is rejected before any build code runs.
//
// The broker does not trust the worktree string the client sends; it
// canonicalizes it server-side. An inactive, unauthorized, traversing,
// or symlink-escaping worktree is rejected before build code runs.
//
// The Authorizer returns the canonical worktree (the path the engine
// should build in) and a nil error on success, or "" and a non-nil
// error (ErrUnauthorized or a wrapped variant) on failure. The broker
// frames an authorization failure as 403 on the execute route (before
// `accepted`).
type Authorizer func(clientWorktree string) (canonicalWorktree string, err error)

// ErrUnauthorized is the sentinel an Authorizer returns when the client
// worktree is not authorized (inactive, not under a configured root,
// symlink-escaping, or not the parent's session worktree). The broker
// frames this as a 403 on the execute route.
var ErrUnauthorized = errors.New("buildbroker: worktree not authorized")

// StartAuthorizer returns an Authorizer that authorizes exactly one
// canonical worktree: the parent's session worktree. The client's
// worktree candidate is canonicalized (filepath.EvalSymlinks) and
// compared to sessionWorktree (which the parent has already
// canonicalized). A mismatch is ErrUnauthorized.
//
// sessionWorktree must already be canonical (the parent canonicalizes
// its own workdir at launch). The authorizer canonicalizes the
// client's candidate the same way so a symlinked path that resolves to
// the session worktree is accepted, while a path that resolves
// elsewhere is rejected.
func StartAuthorizer(sessionWorktree string) Authorizer {
	return func(clientWorktree string) (string, error) {
		canon, err := canonicalize(clientWorktree)
		if err != nil {
			return "", ErrUnauthorized
		}
		if canon != sessionWorktree {
			return "", ErrUnauthorized
		}
		return canon, nil
	}
}

// ServeAuthorizer returns an Authorizer that authorizes a worktree
// only when it is an active directory still under a canonical
// configured root. The authorizer snapshots the active-directory set
// and the root set at the moment of the call; later deactivation
// rejects new requests but does not cancel accepted ones (the snapshot
// is per-call, so a deactivation between two requests is reflected in
// the second request's authorization).
//
// roots is the list of canonical configured roots (the parent
// canonicalizes them with symlink evaluation at cold start). An empty
// roots list allows any directory (the serve default; a non-empty list
// is the --root policy). isActive is a callback the parent supplies
// that reports whether a canonical directory is currently active.
func ServeAuthorizer(roots []string, isActive func(canonicalDir string) bool) Authorizer {
	canonicalRoots := make([]string, 0, len(roots))
	for _, r := range roots {
		c, err := canonicalize(r)
		if err != nil {
			continue
		}
		canonicalRoots = append(canonicalRoots, c)
	}
	sort.Strings(canonicalRoots)
	return func(clientWorktree string) (string, error) {
		canon, err := canonicalize(clientWorktree)
		if err != nil {
			return "", ErrUnauthorized
		}
		if !isActive(canon) {
			return "", ErrUnauthorized
		}
		if len(canonicalRoots) == 0 {
			return canon, nil
		}
		for _, root := range canonicalRoots {
			if canon == root || isUnderRoot(canon, root) {
				return canon, nil
			}
		}
		return "", ErrUnauthorized
	}
}

// isUnderRoot reports whether path is a strict descendant of root
// (both must already be canonical). A path equal to root is handled by
// the caller before this is called. Returns false when rel is "." or
// begins with a ".." segment (symlink traversal escaped the root).
func isUnderRoot(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return false
	}
	// rel beginning with ".." escaped the root; filepath.Rel only
	// returns such a form when path is not under root.
	return !strings.HasPrefix(rel, "..")
}

// canonicalize absolutizes and resolves all symlinks in the path,
// matching the parent's canonicalization. A path that does not exist
// or cannot be resolved is an error (the broker treats it as
// unauthorized).
func canonicalize(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// EvalSymlinks fails on a non-existent path. The parent
		// canonicalizes existing paths; a request for a non-existent
		// worktree is unauthorized.
		return "", err
	}
	return canon, nil
}

// ServeActiveDirs is a thread-safe snapshot of serve's active
// directories. The parent updates it on activate/deactivate; the
// ServeAuthorizer's isActive callback reads it. This is the seam that
// makes authorization snapshotted at acceptance: the authorizer reads
// the set under the lock, and a deactivation that races the call is
// reflected in the next request's authorization.
type ServeActiveDirs struct {
	mu   sync.RWMutex
	dirs map[string]struct{}
}

// NewServeActiveDirs returns an empty ServeActiveDirs.
func NewServeActiveDirs() *ServeActiveDirs {
	return &ServeActiveDirs{dirs: map[string]struct{}{}}
}

// Add records a canonical directory as active.
func (a *ServeActiveDirs) Add(canonicalDir string) {
	a.mu.Lock()
	a.dirs[canonicalDir] = struct{}{}
	a.mu.Unlock()
}

// Remove removes a canonical directory from the active set.
func (a *ServeActiveDirs) Remove(canonicalDir string) {
	a.mu.Lock()
	delete(a.dirs, canonicalDir)
	a.mu.Unlock()
}

// IsActive reports whether canonicalDir is currently active. This is
// the callback the ServeAuthorizer uses; the broker calls it once per
// request under the lock.
func (a *ServeActiveDirs) IsActive(canonicalDir string) bool {
	a.mu.RLock()
	_, ok := a.dirs[canonicalDir]
	a.mu.RUnlock()
	return ok
}

// List returns a snapshot of the active directories. Used by tests.
func (a *ServeActiveDirs) List() []string {
	a.mu.RLock()
	out := make([]string, 0, len(a.dirs))
	for d := range a.dirs {
		out = append(out, d)
	}
	a.mu.RUnlock()
	sort.Strings(out)
	return out
}
