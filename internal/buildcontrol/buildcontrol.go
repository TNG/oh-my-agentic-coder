// Package buildcontrol owns the host-only OMAC build-control root: the
// trusted-state tree that lives OUTSIDE executor and outer-agent grants
// and is never included in any sandbox grant set. It is a sibling of the
// cache-scope directories under the shared cache root (~/.cache/omac/)
// so it survives cache-scope clears and is never writable by build code.
//
// Layout (spec §Serialization and control state):
//
//	<omac-cache-root>/build-control/
//	  approvals/<sha256(canonical-worktree)>.json
//	  approvals-by-repo/<sha256(canonical-repo-root)>/<manifest-digest>.json
//	  ports/<sha256(canonical-worktree)>/{credproxy,containerproxy}.port
//	  locks/<sha256(canonical-leaf)>.lock
//	  daemons/<sha256(canonical-leaf)>.json
//	  requests/<request-id>/gradle-control/
//
// The build-control root is created mode 0o700 (owner-only). The
// lockfile is mode 0o600, persistent, and NEVER unlinked (unlinking a
// flocked path can create a second inode and defeat serialization).
// Brokered and direct host invocations derive the same canonical-leaf
// key so they serialize across processes while repository code cannot
// unlink or replace the lock inode.
//
// Trusted state is namespaced by canonical worktree identity even when
// worktrees share a Gradle cache leaf: durable approvals and stable
// proxy-port preferences are keyed by sha256(canonical-worktree) so
// shared-leaf worktrees keep distinct approval and port records. Daemon
// records are keyed by sha256(canonical-leaf) so the leaf-associated
// daemon can be located for `omac build stop` (a later gate).
//
// This package is the single source of truth for the layout, the
// canonical-leaf / canonical-worktree hashing, and the lock acquisition
// seam. Other packages (buildmanifest, credproxy, containerproxy,
// stableport, buildrun, buildengine, cli) consume its path helpers
// rather than re-deriving paths.
package buildcontrol

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// RootName is the build-control root directory name, a sibling of
// cache-scope directories under the shared cache root. Exported so
// callers can construct diagnostic paths without re-hardcoding the
// literal.
const RootName = "build-control"

// approvalsDir, portsDir, locksDir, daemonsDir, requestsDir are the
// trusted-state subdirectories under the build-control root. Exported
// only within the package's own path helpers; callers use ApprovalPath,
// PortDir, LockPath, etc.
//
// approvalsByRepoDirName is the digest-indexed, repo-namespaced
// approval-reuse subdirectory (ADR 0005): it stores one record per
// (canonicalRepoRoot, manifestDigest) so an already-approved repo's
// unchanged worktrees can build without a fresh per-worktree approval.
// Exported so buildmanifest (which mirrors the path helpers without
// importing this package) and diagnostics can reference the literal.
const (
	approvalsDir           = "approvals"
	approvalsByRepoDirName = "approvals-by-repo"
	portsDir               = "ports"
	locksDir               = "locks"
	daemonsDir             = "daemons"
	requestsDir            = "requests"
)

// LockFileMode is the required mode for a build-control lockfile
// (owner-only, never world- or group-readable). Exported so tests can
// assert it.
const LockFileMode = 0o600

// RootMode is the required mode for the build-control root and its
// trusted-state subdirectories: owner-only (0o700). The root is never
// included in outer-agent or executor grants.
const RootMode = 0o700

// DefaultQueueTimeout bounds how long Acquire waits for a contended
// leaf lock before denying with ErrLockBusy. It mirrors buildrun's
// historical DefaultQueueTimeout so the engine's queue behavior is
// unchanged by the lock relocation.
const DefaultQueueTimeout = 30 * time.Second

// ErrLockCancelled is returned when a contended Acquire was cancelled
// while waiting for the lock (the caller's cancel channel closed). The
// engine maps this to ClassCancelled + the cancellation marker.
//
// Exported as a zero-value sentinel so callers can errors.Is against
// it; the struct's Is method accepts any ErrLockCancelled value.
var ErrLockCancelled = errLockCancelled{}

type errLockCancelled struct {
	path string
}

func (e errLockCancelled) Error() string {
	return fmt.Sprintf("cancelled while waiting for the build queue lock %s", e.path)
}
func (e errLockCancelled) Is(target error) bool {
	_, ok := target.(errLockCancelled)
	return ok
}

// ErrLockBusy is returned when the lock could not be acquired within
// the deadline. The engine maps this to ClassServiceFailure.
var ErrLockBusy = errLockBusy{}

type errLockBusy struct {
	path    string
	timeout time.Duration
}

func (e errLockBusy) Error() string {
	return fmt.Sprintf("another build is running in this cache leaf (queue lock %s held after %s)", e.path, e.timeout)
}
func (e errLockBusy) Is(target error) bool {
	_, ok := target.(errLockBusy)
	return ok
}

// HashLeaf returns sha256(canonicalLeaf) as a lowercase hex string —
// the key under which leaf-keyed trusted state (the lockfile and daemon
// records) is stored. Brokered and direct host invocations pass the
// SAME canonical leaf path so they derive the SAME key and serialize
// across processes.
func HashLeaf(canonicalLeaf string) string {
	return hash(canonicalLeaf)
}

// HashWorktree returns sha256(canonicalWorktree) as a lowercase hex
// string — the key under which worktree-keyed trusted state (durable
// approvals and stable proxy-port preferences) is stored. Worktrees
// that share a Gradle cache leaf keep distinct approval/port records
// because this key is the canonical WORKTREE identity, not the leaf.
func HashWorktree(canonicalWorktree string) string {
	return hash(canonicalWorktree)
}

// HashRepo returns sha256(canonicalRepoRoot) as a lowercase hex string
// — the key under which digest-indexed approval-reuse records are
// namespaced (ADR 0005). canonicalRepoRoot is the canonical
// (EvalSymlinks-resolved) `git rev-parse --git-common-dir` output.
// All linked worktrees of a repo share the same common dir, so they
// collapse to the same namespace; a separate clone has its own common
// dir and lands in a distinct namespace (no cross-clone reuse).
func HashRepo(canonicalRepoRoot string) string {
	return hash(canonicalRepoRoot)
}

func hash(s string) string {
	d := sha256.Sum256([]byte(s))
	return hex.EncodeToString(d[:])
}

// Root returns the absolute path of the build-control root under the
// shared cache root: <cacheRoot>/build-control. cacheRoot is the shared
// cache root (the parent of cache-scope directories — typically
// ~/.cache/omac). It must be non-empty.
func Root(cacheRoot string) string {
	return filepath.Join(cacheRoot, RootName)
}

// ApprovalPath returns the durable-approval record path for a canonical
// worktree: <root>/approvals/<sha256(worktree)>.json.
func ApprovalPath(cacheRoot, canonicalWorktree string) string {
	return filepath.Join(Root(cacheRoot), approvalsDir, HashWorktree(canonicalWorktree)+".json")
}

// ApprovalsByRepoDir returns the repository-namespaced directory for
// digest-indexed approval-reuse records:
// <root>/approvals-by-repo/<sha256(canonicalRepoRoot)>/. canonicalRepoRoot
// is the canonical (EvalSymlinks-resolved) `git rev-parse
// --git-common-dir` output; all linked worktrees of a repo collapse to
// the same namespace (ADR 0005).
func ApprovalsByRepoDir(cacheRoot, canonicalRepoRoot string) string {
	return filepath.Join(Root(cacheRoot), approvalsByRepoDirName, HashRepo(canonicalRepoRoot))
}

// ApprovalByRepoPath returns the digest-indexed, repo-namespaced
// approval-reuse record path:
// <root>/approvals-by-repo/<sha256(canonicalRepoRoot)>/<manifestDigest>.json.
// A changed manifest has a different digest and therefore a different
// record — distinct capabilities never reuse one another's approval.
func ApprovalByRepoPath(cacheRoot, canonicalRepoRoot, manifestDigest string) string {
	return filepath.Join(ApprovalsByRepoDir(cacheRoot, canonicalRepoRoot), manifestDigest+".json")
}

// PortDir returns the stable-proxy-port directory for a canonical
// worktree: <root>/ports/<sha256(worktree)>/. Port files (credproxy.port,
// containerproxy.port) live inside. Each worktree gets its own
// subdirectory so shared-leaf worktrees keep distinct port preferences.
func PortDir(cacheRoot, canonicalWorktree string) string {
	return filepath.Join(Root(cacheRoot), portsDir, HashWorktree(canonicalWorktree))
}

// LockPath returns the leaf-keyed lockfile path:
// <root>/locks/<sha256(leaf)>.lock. Brokered and direct invocations
// pass the same canonical leaf so they derive the same path and
// serialize across processes.
func LockPath(cacheRoot, canonicalLeaf string) string {
	return filepath.Join(Root(cacheRoot), locksDir, HashLeaf(canonicalLeaf)+".lock")
}

// DaemonPath returns the daemon-ownership record path for a canonical
// leaf: <root>/daemons/<sha256(leaf)>.json. A later gate uses this for
// the pending-to-active daemon handshake and verified trusted daemon
// control.
func DaemonPath(cacheRoot, canonicalLeaf string) string {
	return filepath.Join(Root(cacheRoot), daemonsDir, HashLeaf(canonicalLeaf)+".json")
}

// RequestDir returns the per-request control-bundle directory for a
// request id: <root>/requests/<request-id>/. The host creates the
// complete control bundle (gradle.properties, init.d scripts,
// per-run executor control files) under the gradle-control/ subdir
// here and projects it read-only onto the Gradle leaf paths required
// by the wrapper. A later gate wires this projection.
func RequestDir(cacheRoot, requestID string) string {
	return filepath.Join(Root(cacheRoot), requestsDir, requestID)
}

// EnsureRoot creates the build-control root and its trusted-state
// subdirectories with mode 0o700 if absent, and verifies the root's
// mode/ownership if present. It is idempotent. cacheRoot is the shared
// cache root (parent of cache-scope dirs). The root is NEVER included
// in outer-agent or executor grants — callers must not grant it.
//
// Returns the absolute path of the root on success.
func EnsureRoot(cacheRoot string) (string, error) {
	if cacheRoot == "" {
		return "", errors.New("buildcontrol: empty cache root")
	}
	root := Root(cacheRoot)
	for _, sub := range []string{
		root,
		filepath.Join(root, approvalsDir),
		filepath.Join(root, approvalsByRepoDirName),
		filepath.Join(root, portsDir),
		filepath.Join(root, locksDir),
		filepath.Join(root, daemonsDir),
		filepath.Join(root, requestsDir),
	} {
		if err := ensurePrivateDir(sub); err != nil {
			return "", fmt.Errorf("buildcontrol: prepare %s: %w", sub, err)
		}
	}
	return root, nil
}

// ensurePrivateDir creates path with mode 0o700 if absent, and verifies
// it is a non-smlink directory owned by the current user when present.
// A symlink is rejected (a symlinked root could redirect trusted state
// into an executor-writable path).
func ensurePrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.MkdirAll(path, RootMode); err != nil {
			return err
		}
		if err := os.Chmod(path, RootMode); err != nil {
			return err
		}
		return nil
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%q is a symlink (refusing to use a symlinked trusted-state dir)", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", path)
	}
	// Re-assert the mode in case a prior version or a stray chmod
	// loosened it (defensive; the root must stay owner-only).
	if info.Mode().Perm() != RootMode {
		if err := os.Chmod(path, RootMode); err != nil {
			return fmt.Errorf("chmod %q to 0%o: %w", path, RootMode, err)
		}
	}
	return nil
}

// Lock is an exclusive flock on the leaf-keyed lockfile under the
// build-control root. The kernel releases the lock when the holding
// process exits (crash included), so NO stale-lock cleanup is needed.
// The lockfile is PERSISTENT and NEVER unlinked — unlinking a flocked
// path can let another request create and lock a second inode,
// defeating serialization.
type Lock struct {
	path string
	f    *os.File
}

// Path returns the lockfile path (for diagnostics).
func (l *Lock) Path() string { return l.path }

// Release drops the lock and closes the file. It does NOT delete the
// lockfile (the lockfile is persistent; deletion would race a concurrent
// open and orphan the lock).
func (l *Lock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// Acquire takes an exclusive flock on the leaf-keyed lockfile under
// the build-control root, blocking up to timeout for a contended lock.
// On success the caller MUST defer Release. A zero/negative timeout
// substitutes DefaultQueueTimeout.
//
// A nil cancel channel waits the full timeout, non-cancellable. A
// non-nil cancel channel makes the wait individually cancellable: while
// waiting for a contended lock Acquire also selects on `cancel`, and if
// `cancel` closes it closes the open lockfile (without holding the
// flock) and returns ErrLockCancelled promptly, rather than waiting
// the full timeout.
//
// The lockfile is created with mode 0o600 if missing. It is NEVER
// unlinked — neither here, on Release, nor on `omac build stop` (a
// persistent lockfile prevents a concurrent request from creating and
// locking a second inode after an unlink).
//
// cacheRoot is the shared cache root; canonicalLeaf is the resolved
// Gradle cache leaf (buildrun.GradleLeaf(cacheDir)). Brokered and
// direct host invocations pass the same canonical leaf so they derive
// the same lock path and serialize across processes.
func Acquire(cacheRoot, canonicalLeaf string, timeout time.Duration, cancel <-chan struct{}) (*Lock, error) {
	if timeout <= 0 {
		timeout = DefaultQueueTimeout
	}
	if _, err := EnsureRoot(cacheRoot); err != nil {
		return nil, err
	}
	path := LockPath(cacheRoot, canonicalLeaf)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, LockFileMode)
	if err != nil {
		return nil, fmt.Errorf("open build queue lock %s: %w", path, err)
	}
	// Non-blocking try first: the common case (no contention) returns
	// instantly without arming a timer.
	if err := tryLock(f); err == nil {
		return &Lock{path: path, f: f}, nil
	}
	// Contended: poll with a non-blocking try until the deadline, AND
	// select on the cancel channel so a queued request is individually
	// cancellable. flock has no native timeout, so a polling loop is the
	// only way to honor a deadline without leaking a goroutine blocked
	// on the syscall.
	deadline := time.Now().Add(timeout)
	interval := 100 * time.Millisecond
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		if err := tryLock(f); err == nil {
			return &Lock{path: path, f: f}, nil
		}
		select {
		case <-cancel:
			f.Close()
			return nil, errLockCancelled{path: path}
		case <-timer.C:
			if time.Now().After(deadline) {
				f.Close()
				return nil, errLockBusy{path: path, timeout: timeout}
			}
			timer.Reset(interval)
		}
	}
}

// tryLock attempts a non-blocking exclusive flock.
func tryLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// CacheRootFromCacheDir returns the shared cache root given a cache-scope
// dir. The cache-scope dir is <cacheRoot>/<digest>; the cache root is its
// parent. This is the inverse of toolcache's describe() layout. Returns
// "" when cacheDir is empty.
func CacheRootFromCacheDir(cacheDir string) string {
	if cacheDir == "" {
		return ""
	}
	return filepath.Dir(cacheDir)
}
