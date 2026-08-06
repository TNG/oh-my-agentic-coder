package buildrun

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// BuildLockName is the legacy in-leaf queue lockfile, placed inside the
// cache leaf (GRADLE_USER_HOME) so two `omac build` invocations sharing
// the SAME cache leaf serialize on the same file. Serialization is keyed
// by resolved Gradle cache leaf, NOT by worktree: requests sharing a
// leaf serialize; requests using distinct leaves may run concurrently
// (spec §Serialization and control state). This legacy in-leaf lock is
// used only when the engine has no host-only build-control root
// (buildengine.acquireLeafLock with an empty CacheRoot — the
// no-parent direct-host path and unmigrated tests). The authoritative
// lock lives in the host-only build-control root at
// <cacheRoot>/build-control/locks/<sha256(leaf)>.lock (ticket 06):
// persistent, never unlinked, never in executor grants. Repository
// code cannot unlink or replace that lock inode to defeat serialization.
//
// This is the single documented contract constant; there is no
// unexported alias (P5 collapsed the redundant `buildLockName`).
const BuildLockName = ".omac-build.lock"

// DefaultQueueTimeout bounds how long AcquireCtx waits for a contended
// leaf lock before denying with ExitServiceFailure. Short enough
// that a wedged prior build surfaces as a clear denial rather than an
// indefinite hang, long enough that a quick predecessor finishes and the
// caller proceeds.
const DefaultQueueTimeout = 30 * time.Second

// BuildLock is an exclusive flock on the leaf-keyed queue lockfile.
// The kernel releases the lock when the holding process exits (crash
// included), so NO stale-lock cleanup is needed.
type BuildLock struct {
	path string
	f    *os.File
}

// LockPath returns the lockfile path (for diagnostics / `stop` cleanup).
func (l *BuildLock) LockPath() string { return l.path }

// errLockCancelled is returned when a contended AcquireCtx was cancelled
// while waiting for the lock (the caller's cancel channel closed). The
// CLI maps this to ExitCancelled (4) + the cancellation marker — a
// queued request cancelled individually (spec.md:136: "queued requests
// are individually cancellable"), distinct from a busy-denial.
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

// ErrLockCancelled is the exported sentinel for errors.Is checks from
// the CLI (the CLI maps it to ExitCancelled rather than the default
// ExitServiceFailure a busy-denial produces).
var ErrLockCancelled = errLockCancelled{}

// errLockBusy is returned when the lock could not be acquired within the
// deadline. The CLI maps this to ExitServiceFailure with a clear message
// (the "another build is running" busy path).
type errLockBusy struct {
	path    string
	timeout time.Duration
}

func (e errLockBusy) Error() string {
	return fmt.Sprintf("another build is running in this worktree (queue lock %s held after %s)", e.path, e.timeout)
}
func (e errLockBusy) Is(target error) bool {
	_, ok := target.(errLockBusy)
	return ok
}

// ErrLockBusy is the exported sentinel for errors.Is checks from the
// CLI (the CLI maps it to ExitServiceFailure).
var ErrLockBusy = errLockBusy{}

// AcquireCtx takes an exclusive flock on the leaf-keyed queue lockfile,
// blocking up to timeout for a contended lock. On success the caller MUST
// defer Release. A zero/negative timeout substitutes
// DefaultQueueTimeout (NOT an immediate denial — the defensible default
// is to wait for a quick predecessor; an immediate denial would surface a
// transient contention as a hard service failure). (P6: the doc
// previously lied that zero denies immediately; the code has always
// substituted the default.)
//
// A nil cancel channel waits the full timeout, non-cancellable. A non-nil
// cancel channel makes the wait individually cancellable (spec.md:136:
// "queued requests are individually cancellable" — e.g. a second `omac
// build` Ctrl-C unwinds the waiter without killing the running build):
// while waiting for a contended lock AcquireCtx also selects on `cancel`,
// and if `cancel` closes it releases the partial lock (closes the open
// lockfile without holding the flock) and returns ErrLockCancelled
// promptly, rather than waiting the full timeout.
//
// lockfileDir is the dir the lockfile lives in (the cache leaf); it must
// already exist (GrantsFor ensures the leaf). The lockfile itself is
// created if missing.
//
// Two outcomes on contention:
//   - cancelled-while-waiting (non-nil cancel only) → ErrLockCancelled;
//     the CLI maps this to ExitCancelled (4) + the cancellation marker.
//   - timed-out-waiting → ErrLockBusy ("another build is running"); the
//     CLI maps this to ExitServiceFailure (10).
func AcquireCtx(lockfileDir string, timeout time.Duration, cancel <-chan struct{}) (*BuildLock, error) {
	if timeout <= 0 {
		timeout = DefaultQueueTimeout
	}
	path := filepath.Join(lockfileDir, BuildLockName)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open build queue lock %s: %w", path, err)
	}
	// Non-blocking try first: the common case (no contention) returns
	// instantly without arming a timer.
	if err := tryLock(f); err == nil {
		return &BuildLock{path: path, f: f}, nil
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
			return &BuildLock{path: path, f: f}, nil
		}
		select {
		case <-cancel:
			// Cancelled while waiting: release the partial lock (close
			// the open file WITHOUT holding the flock) and return the
			// cancellation error, not a busy-denial.
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

// Release drops the lock and closes (does NOT delete) the lockfile. The
// file stays on disk so a concurrent AcquireCtx can open it; deletion would
// race a concurrent open and orphan the lock.
func (l *BuildLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

// tryLock attempts a non-blocking exclusive flock.
func tryLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
