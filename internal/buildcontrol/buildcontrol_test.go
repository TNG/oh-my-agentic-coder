package buildcontrol

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHash_StableAndDistinct(t *testing.T) {
	leaf1 := "/home/u/.cache/omac/abc/gradle"
	leaf2 := "/home/u/.cache/omac/def/gradle"
	wt1 := "/repo/worktree-a"
	wt2 := "/repo/worktree-b"
	if HashLeaf(leaf1) == HashLeaf(leaf2) {
		t.Error("distinct leaves hashed the same")
	}
	if HashWorktree(wt1) == HashWorktree(wt2) {
		t.Error("distinct worktrees hashed the same")
	}
	leafHash1 := HashLeaf(leaf1)
	leafHash2 := HashLeaf(leaf1)
	if leafHash1 != leafHash2 {
		t.Error("hash not stable")
	}
	// Shared leaf but distinct worktrees: leaf hash equal, worktree hash distinct.
	if leafHash1 != HashLeaf(leaf1) {
		t.Error("leaf hash not stable")
	}
	if HashWorktree(wt1) == HashWorktree(wt2) {
		t.Error("distinct worktrees hashed the same under shared leaf")
	}
}

func TestPaths_UnderRoot(t *testing.T) {
	root := "/home/u/.cache/omac"
	leaf := "/home/u/.cache/omac/abc/gradle"
	wt := "/repo/worktree"
	ap := ApprovalPath(root, wt)
	if !strings.HasPrefix(ap, filepath.Join(root, "build-control", "approvals")) {
		t.Errorf("approval path %q not under build-control/approvals", ap)
	}
	if !strings.HasSuffix(ap, ".json") {
		t.Errorf("approval path %q missing .json suffix", ap)
	}
	pd := PortDir(root, wt)
	if !strings.HasPrefix(pd, filepath.Join(root, "build-control", "ports")) {
		t.Errorf("port dir %q not under build-control/ports", pd)
	}
	lp := LockPath(root, leaf)
	if !strings.HasPrefix(lp, filepath.Join(root, "build-control", "locks")) {
		t.Errorf("lock path %q not under build-control/locks", lp)
	}
	if !strings.HasSuffix(lp, ".lock") {
		t.Errorf("lock path %q missing .lock suffix", lp)
	}
	// Shared leaf, distinct worktrees: same lock path, distinct approval/port paths.
	wt2 := "/repo/other-worktree"
	lockPath1 := LockPath(root, leaf)
	lockPath2 := LockPath(root, leaf)
	if lockPath1 != lockPath2 {
		t.Error("leaf lock path not stable")
	}
	if ApprovalPath(root, wt) == ApprovalPath(root, wt2) {
		t.Error("distinct worktrees share approval path")
	}
	if PortDir(root, wt) == PortDir(root, wt2) {
		t.Error("distinct worktrees share port dir")
	}
}

func TestEnsureRoot_CreatesPrivateDirs(t *testing.T) {
	cacheRoot := t.TempDir()
	root, err := EnsureRoot(cacheRoot)
	if err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	for _, sub := range []string{
		root,
		filepath.Join(root, "approvals"),
		filepath.Join(root, "ports"),
		filepath.Join(root, "locks"),
		filepath.Join(root, "daemons"),
		filepath.Join(root, "requests"),
	} {
		info, err := os.Stat(sub)
		if err != nil {
			t.Errorf("missing %s: %v", sub, err)
			continue
		}
		if info.Mode().Perm() != RootMode {
			t.Errorf("%s mode = %o, want %o", sub, info.Mode().Perm(), RootMode)
		}
	}
	// Idempotent.
	if _, err := EnsureRoot(cacheRoot); err != nil {
		t.Errorf("EnsureRoot not idempotent: %v", err)
	}
}

func TestEnsureRoot_RejectsSymlink(t *testing.T) {
	cacheRoot := t.TempDir()
	root := filepath.Join(cacheRoot, "build-control")
	link := filepath.Join(cacheRoot, "build-control-link")
	// Point a symlink at where the root would be.
	if err := os.Symlink(root, link); err != nil {
		// Some sandboxes block symlinks; skip if so.
		t.Skipf("cannot create symlink: %v", err)
	}
	// Replace the expected root path with the symlink by removing the
	// real dir entry and symlinking instead — here we just test that a
	// symlinked dir is rejected when EnsureRoot finds one.
	// Create the symlink as the root path.
	_ = os.Remove(root)
	if err := os.Symlink(filepath.Join(cacheRoot, "elsewhere"), root); err != nil {
		t.Skipf("cannot create symlink at root: %v", err)
	}
	_, err := EnsureRoot(cacheRoot)
	if err == nil {
		t.Error("EnsureRoot accepted a symlinked root")
	}
}

func TestAcquire_NoContention(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf/gradle"
	l, err := Acquire(cacheRoot, leaf, time.Second, nil)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer l.Release()
	if l.Path() != LockPath(cacheRoot, leaf) {
		t.Errorf("Path = %q, want %q", l.Path(), LockPath(cacheRoot, leaf))
	}
}

func TestAcquire_PersistentLockfileNeverUnlinked(t *testing.T) {
	// The lockfile is persistent: Release does NOT unlink it. A second
	// Acquire finds the same inode (the kernel released the flock on
	// close) and locks it.
	cacheRoot := t.TempDir()
	leaf := "/leaf/gradle"
	l1, err := Acquire(cacheRoot, leaf, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := l1.Path()
	l1.Release()
	// File still on disk.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lockfile removed after Release: %v", err)
	}
	// Same inode: open by path and compare to a fresh acquire's fd.
	info1, _ := os.Stat(path)
	l2, err := Acquire(cacheRoot, leaf, time.Second, nil)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	defer l2.Release()
	info2, _ := os.Stat(path)
	if !sameFile(info1, info2) {
		t.Errorf("lockfile inode changed between acquires (persistent lockfile violated)")
	}
}

func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b)
}

func TestAcquire_SerializesContended(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf/gradle"
	var inFlight, maxConcurrent int32
	var wg sync.WaitGroup
	for n := int32(0); n < 3; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, err := Acquire(cacheRoot, leaf, 10*time.Second, nil)
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer l.Release()
			cur := atomic.AddInt32(&inFlight, 1)
			if cur > atomic.LoadInt32(&maxConcurrent) {
				atomic.StoreInt32(&maxConcurrent, cur)
			}
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&inFlight, -1)
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&maxConcurrent) != 1 {
		t.Errorf("max concurrent = %d, want 1 (queue must serialize)", maxConcurrent)
	}
}

func TestAcquire_BrokeredAndDirectShareLock(t *testing.T) {
	// Brokered and direct host invocations derive the same canonical-leaf
	// key so they serialize across processes. Here we simulate two
	// independent callers (two goroutines) using the same cacheRoot +
	// canonical leaf — they must contend on the same lockfile.
	cacheRoot := t.TempDir()
	leaf := "/shared/leaf/gradle"
	l1, err := Acquire(cacheRoot, leaf, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer l1.Release()
	// A second Acquire with the same leaf must time out (contended).
	start := time.Now()
	_, err = Acquire(cacheRoot, leaf, 150*time.Millisecond, nil)
	d := time.Since(start)
	if err == nil {
		t.Fatal("expected busy denial, got lock")
	}
	if d < 100*time.Millisecond {
		t.Errorf("denied after %v, want to wait the 150ms timeout", d)
	}
}

func TestAcquire_CancelledWhileWaiting(t *testing.T) {
	cacheRoot := t.TempDir()
	leaf := "/leaf/gradle"
	holder, err := Acquire(cacheRoot, leaf, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	cancel := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := Acquire(cacheRoot, leaf, 30*time.Second, cancel)
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	close(cancel)
	err = <-done
	if err == nil {
		t.Fatal("expected cancellation error, got the lock")
	}
	if !isCancelled(err) {
		t.Errorf("err = %v, want ErrLockCancelled", err)
	}
}

func isCancelled(err error) bool {
	// The exported ErrLockCancelled sentinel matches via errors.Is.
	return errors.Is(err, ErrLockCancelled) || strings.Contains(err.Error(), "cancelled while waiting")
}

func TestCacheRootFromCacheDir(t *testing.T) {
	if got := CacheRootFromCacheDir(""); got != "" {
		t.Errorf("empty cacheDir: got %q, want empty", got)
	}
	if got := CacheRootFromCacheDir("/home/u/.cache/omac/abc123"); got != "/home/u/.cache/omac" {
		t.Errorf("CacheRootFromCacheDir: got %q, want /home/u/.cache/omac", got)
	}
}

// TestBuildControlRoot_NotAncestorOfCacheScope asserts the build-control
// root is a SIBLING of cache-scope directories, not a parent or
// ancestor. The outer-agent sandbox grants the cache-scope dir
// (`<cacheRoot>/<digest>`) via `--allow`; if the build-control root
// were a descendant of that dir, the grant would expose trusted state
// (durable approvals, stable ports, locks, daemon records) to the
// outer agent. The layout invariants:
//
//   - cacheRoot = CacheRootFromCacheDir(cacheScopeDir) = Dir(cacheScopeDir)
//   - buildControlRoot = Root(cacheRoot) = <cacheRoot>/build-control
//   - cacheScopeDir = <cacheRoot>/<digest>  (a sibling of build-control)
//
// So `<cacheScopeDir>` is NOT an ancestor of `<buildControlRoot>`, and a
// grant of `cacheScopeDir` cannot reach `buildControlRoot`. This is
// the trust-boundary guarantee for ticket 06: durable approvals, stable
// ports, locks, and daemon records are inaccessible from the outer
// agent even after clearing all OMAC_* env vars (the boundary is
// filesystem layout, not env filtering).
func TestBuildControlRoot_NotAncestorOfCacheScope(t *testing.T) {
	cacheRoot := "/home/u/.cache/omac"
	cacheScopeDir := filepath.Join(cacheRoot, "abc123-digest") // a cache-scope dir
	buildControlRoot := Root(cacheRoot)

	// Invariant 1: CacheRootFromCacheDir returns the parent of the
	// cache-scope dir (the shared cache root).
	gotRoot := CacheRootFromCacheDir(cacheScopeDir)
	if gotRoot != cacheRoot {
		t.Fatalf("CacheRootFromCacheDir(%q) = %q, want %q", cacheScopeDir, gotRoot, cacheRoot)
	}

	// Invariant 2: the build-control root is <cacheRoot>/build-control.
	if buildControlRoot != filepath.Join(cacheRoot, RootName) {
		t.Errorf("buildControlRoot = %q, want %q", buildControlRoot, filepath.Join(cacheRoot, RootName))
	}

	// Invariant 3: the cache-scope dir is NOT an ancestor of the
	// build-control root (the grant of cacheScopeDir cannot reach it).
	if isAncestor(cacheScopeDir, buildControlRoot) {
		t.Errorf("cache-scope dir %q is an ancestor of build-control root %q — an outer-agent --allow of the cache scope would expose trusted state",
			cacheScopeDir, buildControlRoot)
	}

	// Invariant 4: the build-control root is NOT a descendant of the
	// cache-scope dir (symmetric check).
	if isAncestor(buildControlRoot, cacheScopeDir) {
		t.Errorf("build-control root %q is an ancestor of cache-scope dir %q — unexpected layout",
			buildControlRoot, cacheScopeDir)
	}

	// Invariant 5: they share the same parent (they ARE siblings).
	if filepath.Dir(cacheScopeDir) != filepath.Dir(buildControlRoot) {
		t.Errorf("cache-scope dir (%q) and build-control root (%q) are not siblings (different parents)",
			filepath.Dir(cacheScopeDir), filepath.Dir(buildControlRoot))
	}

	// Invariant 6: every trusted-state path is under the build-control
	// root, NOT under the cache-scope dir. A grant of cacheScopeDir
	// cannot reach approvals, ports, locks, daemons, or requests.
	leaf := filepath.Join(cacheScopeDir, "gradle")
	wt := "/repo/worktree"
	reqID := "req-123"
	trustedPaths := map[string]string{
		"approval": ApprovalPath(cacheRoot, wt),
		"portDir":  PortDir(cacheRoot, wt),
		"lock":     LockPath(cacheRoot, leaf),
		"daemon":   DaemonPath(cacheRoot, leaf),
		"request":  RequestDir(cacheRoot, reqID),
	}
	for name, p := range trustedPaths {
		if !strings.HasPrefix(p, buildControlRoot+string(filepath.Separator)) {
			t.Errorf("trusted path %q (%s) is NOT under the build-control root %q", p, name, buildControlRoot)
		}
		if strings.HasPrefix(p, cacheScopeDir+string(filepath.Separator)) {
			t.Errorf("trusted path %q (%s) is UNDER the cache-scope dir %q — an outer-agent grant would expose it", p, name, cacheScopeDir)
		}
		if isAncestor(cacheScopeDir, p) {
			t.Errorf("trusted path %q (%s) is a descendant of cache-scope dir %q — grant exposure", p, name, cacheScopeDir)
		}
	}
}

// TestBuildControlRoot_NeverInExecutorGrants asserts the build-control
// root is NOT included in executor grants. The executor sandbox grants
// the cache leaf writable (for normal Gradle state) plus the control
// paths read-only (via WriteDenyPaths); the build-control root is
// host-only and never appears in either set. This test asserts the
// build-control paths are not derivable from the cache leaf path
// (they are derived from the cache ROOT, not the leaf).
func TestBuildControlRoot_NeverInExecutorGrants(t *testing.T) {
	cacheRoot := "/home/u/.cache/omac"
	cacheScopeDir := filepath.Join(cacheRoot, "digest-abc")
	leaf := filepath.Join(cacheScopeDir, "gradle") // executor gets leaf writable

	// The executor's grant set is derived from the leaf and the
	// control paths under it. The build-control root is derived from
	// the cache ROOT (parent of cacheScopeDir), NOT from the leaf.
	buildControlRoot := Root(CacheRootFromCacheDir(cacheScopeDir))

	// The build-control root is not the leaf, not a parent of the leaf,
	// and not a descendant of the leaf.
	if buildControlRoot == leaf {
		t.Fatalf("build-control root equals the leaf — executor grant would expose trusted state")
	}
	if isAncestor(leaf, buildControlRoot) {
		t.Errorf("leaf %q is an ancestor of build-control root %q — the executor's leaf grant would expose trusted state", leaf, buildControlRoot)
	}
	if isAncestor(buildControlRoot, leaf) {
		t.Errorf("build-control root %q is an ancestor of leaf %q — unexpected", buildControlRoot, leaf)
	}
}

// isAncestor reports whether candidate is an ancestor of path (i.e. path
// is candidate or a descendant of candidate). A path is its own ancestor.
func isAncestor(candidate, path string) bool {
	if candidate == path {
		return true
	}
	return strings.HasPrefix(path, candidate+string(filepath.Separator))
}
