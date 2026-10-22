package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

// bumpOffset makes successive bumps distinct even on filesystems whose
// coarse clock would give two bumps within the same second the same
// mtime, which would let the walk skip the directory again.
var bumpOffset atomic.Int64

// bump sets dir's mtime to a value well apart from any previous read's,
// so the next pass re-reads it even where the filesystem clock is coarse
// and the mutation would otherwise share the cached timestamp (HFS+
// ticks in whole seconds, ext4's clock is a few milliseconds coarse).
func bump(t *testing.T, dir string) {
	t.Helper()
	ts := time.Now().Add(time.Duration(bumpOffset.Add(1)) * time.Hour)
	if err := os.Chtimes(dir, ts, ts); err != nil {
		t.Fatal(err)
	}
}

// cacheTree builds a fixed shape the tests below reason about:
// root/{sub1,sub2}/{deep,a}/file and root/sub1/deep/.env. Four
// directories: root, sub1, sub2 and one deep per sub.
func cacheTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, sub := range []string{"sub1", "sub2"} {
		for _, leaf := range []string{"deep", "a"} {
			p := filepath.Join(root, sub, leaf)
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(p, "file"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(root, "sub1", "deep", ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func cacheWalk(t *testing.T, root string, cache *denyScanCache) []string {
	t.Helper()
	got, hitCap := walkOneDenyRoot(root, []string{".env"}, map[string]bool{}, make(chan struct{}, denyScanConcurrency), cache, denyScanConcurrency)
	slices.Sort(got)
	if hitCap {
		t.Fatalf("unexpected cap hit walking %s", root)
	}
	return got
}

// TestDenyScanCacheFirstPassMatchesSequential asserts the seeded pass
// (cache attached but empty) finds what the uncached walk finds.
func TestDenyScanCacheFirstPassMatchesSequential(t *testing.T) {
	steadyState(t)
	root := cacheTree(t)
	got := cacheWalk(t, root, newDenyScanCache())
	want := []string{filepath.Join(root, "sub1", "deep", ".env")}
	if !slices.Equal(got, want) {
		t.Fatalf("cached first pass got %v, want %v", got, want)
	}
}

// TestDenyScanCacheSkipsUnchangedDirs asserts the second pass reads
// nothing: every directory is lstat'd (the traversal must still reach
// them), no directory is re-read, and no match is reported again.
func TestDenyScanCacheSkipsUnchangedDirs(t *testing.T) {
	steadyState(t)
	root := cacheTree(t)
	cache := newDenyScanCache()
	rc := cache.state(root)

	if got := cacheWalk(t, root, cache); len(got) != 1 {
		t.Fatalf("first pass got %v", got)
	}
	readsAfterFirst := rc.readdirs.Load()
	if readsAfterFirst != 7 {
		t.Fatalf("first pass: %d readdirs, want 7 (root, sub1, sub2, deep+a per sub)", readsAfterFirst)
	}

	got := cacheWalk(t, root, cache)
	if len(got) != 0 {
		t.Fatalf("unchanged pass reported new matches: %v", got)
	}
	if rc.readdirs.Load() != readsAfterFirst {
		t.Errorf("unchanged pass re-read directories (%d -> %d readdirs)", readsAfterFirst, rc.readdirs.Load())
	}
	if rc.skips.Load() != 7 {
		t.Errorf("unchanged pass skipped %d directories, want 7", rc.skips.Load())
	}
	if lstats := rc.lstats.Load(); lstats != 14 {
		t.Errorf("two passes issued %d lstats, want 14 (every directory every pass)", lstats)
	}
}

// TestDenyScanCacheFindsNewMatchOnlyInChangedDirs creates a protected
// file in a deep untouched directory: only the ancestors on the way
// down changed, and exactly that subtree is re-read and matched.
func TestDenyScanCacheFindsNewMatchOnlyInChangedDirs(t *testing.T) {
	steadyState(t)
	root := cacheTree(t)
	cache := newDenyScanCache()
	if got := cacheWalk(t, root, cache); len(got) != 1 {
		t.Fatalf("first pass got %v", got)
	}
	readsAfterFirst := cache.state(root).readdirs.Load()

	// root untouched, root/sub1 gains a new directory, root/sub2 untouched.
	if err := os.Mkdir(filepath.Join(root, "sub1", "newdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub1", "newdir", ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bump(t, filepath.Join(root, "sub1"))

	got := cacheWalk(t, root, cache)
	want := filepath.Join(root, "sub1", "newdir", ".env")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("incremental pass got %v, want [%s]", got, want)
	}
	reads := cache.state(root).readdirs.Load()
	if reads-readsAfterFirst != 2 { // sub1 and sub1/newdir
		t.Errorf("incremental pass re-read %d directories, want 2", reads-readsAfterFirst)
	}
}

// TestDenyScanCacheReconcilesRemovedDirs removes a subtree, then
// recreates it with a match: the cache must not serve the stale state
// of the removed directories.
func TestDenyScanCacheReconcilesRemovedDirs(t *testing.T) {
	steadyState(t)
	root := cacheTree(t)
	cache := newDenyScanCache()
	if got := cacheWalk(t, root, cache); len(got) != 1 {
		t.Fatalf("first pass got %v", got)
	}

	if err := os.RemoveAll(filepath.Join(root, "sub1")); err != nil {
		t.Fatal(err)
	}
	bump(t, root)
	if got := cacheWalk(t, root, cache); len(got) != 0 {
		t.Fatalf("pass after removal reported %v", got)
	}

	if err := os.MkdirAll(filepath.Join(root, "sub1", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub1", "deep", ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bump(t, root)
	got := cacheWalk(t, root, cache)
	want := filepath.Join(root, "sub1", "deep", ".env")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("pass after recreation got %v, want [%s]", got, want)
	}
}

// TestDenyScanCacheFileRootSkipsReaddirs covers a granted file used as
// a scan root (e.g. ~/.gitconfig): the first pass attempts the read,
// later passes retire it with one lstat — even after the file is
// rewritten.
func TestDenyScanCacheFileRootSkipsReaddirs(t *testing.T) {
	steadyState(t)
	file := filepath.Join(t.TempDir(), "dotfile")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := newDenyScanCache()
	rc := cache.state(file)

	if got := cacheWalk(t, file, cache); len(got) != 0 {
		t.Fatalf("file root reported %v", got)
	}
	if rc.readdirs.Load() != 1 {
		t.Fatalf("first pass: %d readdirs, want 1", rc.readdirs.Load())
	}

	if err := os.WriteFile(file, []byte("rewritten"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cacheWalk(t, file, cache); len(got) != 0 {
		t.Fatalf("file root reported %v after rewrite", got)
	}
	if rc.readdirs.Load() != 1 {
		t.Errorf("file root was re-read after rewrite (%d readdirs)", rc.readdirs.Load())
	}
	if rc.skips.Load() != 1 {
		t.Errorf("file root skips = %d, want 1", rc.skips.Load())
	}
}

// TestDenyScanCacheChargesCapAcrossPasses grows a tree past a lowered
// cap between passes; the incremental pass must still sum the whole
// tree (cached counts for unchanged directories, fresh counts for
// changed ones) and report the cap like a full recount would.
func TestDenyScanCacheChargesCapAcrossPasses(t *testing.T) {
	steadyState(t)
	setDenyScanLimit(t, 40)
	setDenyByName(t, true)
	root := t.TempDir()
	big := filepath.Join(root, "big")
	if err := os.Mkdir(big, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range 10 {
		if err := os.WriteFile(filepath.Join(big, "n"), []byte{byte(i)}, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cache := newDenyScanCache()
	if got, hitCap := walkOneDenyRoot(root, []string{".env"}, map[string]bool{}, make(chan struct{}, denyScanConcurrency), cache, denyScanConcurrency); hitCap || len(got) != 0 {
		t.Fatalf("first pass: matches %v, cap %v", got, hitCap)
	}

	// Grow past the cap inside the one directory the second pass
	// re-reads; root itself is unchanged and skipped from reading.
	for i := range 50 {
		if err := os.WriteFile(filepath.Join(big, fmt.Sprintf("m%02d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bump(t, big)
	if _, hitCap := walkOneDenyRoot(root, []string{".env"}, map[string]bool{}, make(chan struct{}, denyScanConcurrency), cache, denyScanConcurrency); !hitCap {
		t.Fatal("incremental pass must report the cap after the tree outgrew it")
	}
}

// TestDenyScanCacheThrottledConcurrency asserts the throttled full pass
// (one reader at a time, as the watch's throttled full pass runs it)
// still walks correctly and finds the same matches.
func TestDenyScanCacheThrottledConcurrency(t *testing.T) {
	steadyState(t)
	root := resolvePath(t, cacheTree(t))
	got, hitCap := walkOneDenyRoot(root, []string{".env"}, map[string]bool{}, make(chan struct{}, 1), newDenyScanCache(), 1)
	slices.Sort(got)
	if hitCap {
		t.Fatal("unexpected cap hit")
	}
	if want := filepath.Join(root, "sub1", "deep", ".env"); len(got) != 1 || got[0] != want {
		t.Fatalf("throttled walk got %v, want [%s]", got, want)
	}
}

// TestDenyScanCacheSurvivesTransientReadError injects a failed readdir
// into one changed directory. The failed read must not be cached: its
// entries would be nil, and with that state cached under the current
// fingerprint, the directory below it would be invisible to every later
// pass while nothing on disk changes — a new match there would never be
// detected. Instead the stale entry keeps its old fingerprint, so the
// next pass re-reads the directory and finds the match.
func TestDenyScanCacheSurvivesTransientReadError(t *testing.T) {
	steadyState(t)
	root := resolvePath(t, cacheTree(t))
	cache := newDenyScanCache()
	if got := cacheWalk(t, root, cache); len(got) != 1 {
		t.Fatalf("first pass got %v", got)
	}

	// A new subtree inside sub1/deep. This changes sub1/deep's
	// fingerprint, so the next pass re-reads it — and that read fails.
	deep := filepath.Join(root, "sub1", "deep")
	if err := os.Mkdir(filepath.Join(deep, "kid"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "kid", ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bump(t, deep)

	hook := func(dir string) ([]os.DirEntry, error) {
		if dir == deep {
			return nil, fmt.Errorf("injected read failure")
		}
		return os.ReadDir(dir)
	}
	readDirHook.Store(&hook)
	t.Cleanup(func() { readDirHook.Store(nil) })
	if got := cacheWalk(t, root, cache); len(got) != 0 {
		t.Fatalf("failing pass reported %v", got)
	}
	readDirHook.Store(nil)

	// The failing pass must have left the cached state untouched, so
	// this pass re-reads sub1/deep and reports all its matches,
	// including the new one.
	got := cacheWalk(t, root, cache)
	want := []string{
		filepath.Join(deep, ".env"),
		filepath.Join(deep, "kid", ".env"),
	}
	if !slices.Equal(got, want) {
		t.Fatalf("pass after the transient failure got %v, want %v", got, want)
	}
}

// resolvePath resolves dir with EvalSymlinks: the walk works on the
// resolved spelling (on macOS /var/... becomes /private/var/...), and
// the tests below need the cache keys and the match spellings to match
// that.
func resolvePath(t *testing.T, dir string) string {
	t.Helper()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

// TestDenyScanCacheDetectsBackdatedMtime covers the ctime half of the
// fingerprint: an agent can set a directory's mtime back to the cached
// value with utimensat (os.Chtimes here), but cannot set ctime — the
// Chtimes call itself updates it — so the pass must still re-read and
// find the match an mtime-only fingerprint would hide.
func TestDenyScanCacheDetectsBackdatedMtime(t *testing.T) {
	steadyState(t)
	root := resolvePath(t, cacheTree(t))
	cache := newDenyScanCache()
	if got := cacheWalk(t, root, cache); len(got) != 1 {
		t.Fatalf("first pass got %v", got)
	}

	sub1 := filepath.Join(root, "sub1")
	cached := cache.state(root).dirs[sub1]
	if cached == nil {
		t.Fatal("sub1 was not cached")
	}
	if err := os.WriteFile(filepath.Join(sub1, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate sub1's mtime to the fingerprint the first pass cached.
	old := time.Unix(cached.fp.mtimeSec, cached.fp.mtimeNsec)
	if err := os.Chtimes(sub1, old, old); err != nil {
		t.Fatal(err)
	}

	got := cacheWalk(t, root, cache)
	want := filepath.Join(sub1, ".env")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("pass after backdating got %v, want [%s]", got, want)
	}
}

// TestDenyScanCacheDetectsChmodReveal covers the documented ctime use:
// a chmod leaves mtime alone, so an mtime-only fingerprint would serve
// the stale cached state of a folder whose readability changed.
func TestDenyScanCacheDetectsChmodReveal(t *testing.T) {
	steadyState(t)
	root := resolvePath(t, cacheTree(t))
	deep := filepath.Join(root, "sub1", "deep")
	cache := newDenyScanCache()
	if got := cacheWalk(t, root, cache); len(got) != 1 {
		t.Fatalf("first pass got %v", got)
	}
	readsAfterFirst := cache.state(root).readdirs.Load()

	// Two chmods: ctime changes, mtime does not.
	if err := os.Chmod(deep, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	got := cacheWalk(t, root, cache)
	want := filepath.Join(deep, ".env")
	if len(got) != 1 || got[0] != want {
		t.Fatalf("pass after chmod got %v, want [%s]", got, want)
	}
	if reads := cache.state(root).readdirs.Load(); reads-readsAfterFirst != 1 {
		t.Errorf("pass after chmod re-read %d directories, want 1", reads-readsAfterFirst)
	}
}

// steadyState makes dirTimestampWindow zero for tests that model a tree
// that has not been touched near the scan; otherwise every freshly
// created directory would be re-checked once, which is the intended
// production behavior but not what those tests assert.
func steadyState(t *testing.T) {
	t.Helper()
	old := dirTimestampWindow
	dirTimestampWindow = 0
	t.Cleanup(func() { dirTimestampWindow = old })
}

// TestDenyScanCacheRechecksCoarseTimestampWindow simulates the race the
// optional full pass exists for: a directory is read just before a
// change that lands in the same coarse-clock tick, so the fingerprint
// after the change equals the cached one. The read-time window must
// force one extra read on the next pass, which finds the new match
// without a full re-walk of the whole tree.
func TestDenyScanCacheRechecksCoarseTimestampWindow(t *testing.T) {
	steadyState(t)
	root := resolvePath(t, cacheTree(t))
	cache := newDenyScanCache()
	if got := cacheWalk(t, root, cache); len(got) != 1 {
		t.Fatalf("first pass got %v", got)
	}

	deep := filepath.Join(root, "sub2", "deep")
	e := cache.state(root).dirs[deep]
	if e == nil {
		t.Fatal("sub2/deep was not cached")
	}
	if err := os.WriteFile(filepath.Join(deep, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The hidden change: the cached entries predate .env, but the
	// fingerprint is the post-change one (identical to the cached
	// fingerprint on a coarse-clock filesystem).
	info, err := os.Lstat(deep)
	if err != nil {
		t.Fatal(err)
	}
	old := dirTimestampWindow
	dirTimestampWindow = time.Hour
	cache.state(root).record(deep, fingerprintOf(info), true, e.count, e.subdirs)
	dirTimestampWindow = old

	got := cacheWalk(t, root, cache)
	want := filepath.Join(deep, ".env")
	if !slices.Contains(got, want) {
		t.Fatalf("pass after a same-tick change got %v, want %s", got, want)
	}
}

// TestDenyScanCacheRecheckClears asserts the re-check is a one-shot: a
// directory read inside the window is listed once more, and that second
// read (now outside the window) is cached normally again. Without this a
// regression that always set recheck to true would re-read the whole
// tree on every tick and quietly bring the CPU burn back.
func TestDenyScanCacheRecheckClears(t *testing.T) {
	old := dirTimestampWindow
	t.Cleanup(func() { dirTimestampWindow = old })
	dirTimestampWindow = time.Hour

	root := resolvePath(t, cacheTree(t))
	cache := newDenyScanCache()
	if got := cacheWalk(t, root, cache); len(got) != 1 {
		t.Fatalf("first pass got %v", got)
	}

	// Model the next tick: the directory's timestamps are now outside
	// the window, so the re-check read must not flag it again.
	dirTimestampWindow = time.Nanosecond
	if got := cacheWalk(t, root, cache); len(got) != 1 {
		t.Fatalf("re-check pass got %v", got)
	}
	reads := cache.state(root).readdirs.Load()

	if got := cacheWalk(t, root, cache); len(got) != 0 {
		t.Fatalf("steady pass reported %v", got)
	}
	if after := cache.state(root).readdirs.Load(); after != reads {
		t.Errorf("steady pass re-read %d directories, want 0", after-reads)
	}
}
