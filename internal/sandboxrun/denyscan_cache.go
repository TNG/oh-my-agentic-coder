package sandboxrun

import (
	"sync"
	"sync/atomic"
	"time"
)

// dirTimestampWindow is how close a directory's mtime/ctime must be to
// the read that cached it for that read to be re-checked on the next
// pass. It covers the coarse-clock window in which a later change can
// reuse the cached timestamp: HFS+ is 1 s, FAT/exFAT and common network
// mounts are 1-2 s. A read that fell inside the window costs one extra
// directory listing on the next pass, which is tiny at idle and removes
// the need for a periodic full re-walk. A variable so tests can control
// it.
var dirTimestampWindow = 2 * time.Second

// dirFingerprint identifies a directory's metadata state: the (sec,
// nsec) pairs of its mtime and ctime plus the inode and device from the
// raw stat. mtime changes whenever the entry set changes (create,
// unlink, rename all update it), so an unchanged mtime proves the entry
// set is unchanged. ctime also catches changes that leave mtime alone,
// such as a chmod that makes a previously unreadable folder readable —
// and it cannot be set from user space, so backdating mtime with
// utimensat still invalidates the fingerprint. ino and dev catch the
// directory being replaced wholesale.
//
// Limitation: directory timestamps come from the filesystem's coarse
// clock (whole seconds on HFS+, a few milliseconds on ext4, coarser
// still on FAT/exFAT and network mounts), so a change landing in the
// same tick as the previous read can leave every field unchanged. The
// mid-session watch handles this without a full re-walk: a read that
// falls within one timestamp tick of the directory's mtime/ctime is
// flagged and lists the directory once more on the next pass (see
// dirTimestampWindow and denyScanDirState.recheck).
type dirFingerprint struct {
	mtimeSec, mtimeNsec int64
	ctimeSec, ctimeNsec int64
	ino, dev            int64
}

func (f dirFingerprint) equal(o dirFingerprint) bool {
	return f.mtimeSec == o.mtimeSec && f.mtimeNsec == o.mtimeNsec &&
		f.ctimeSec == o.ctimeSec && f.ctimeNsec == o.ctimeNsec &&
		f.ino == o.ino && f.dev == o.dev
}

// denyScanCache remembers what the last walk of each scan root saw in
// every directory: its fingerprint, entry count and the subdirectories
// the walk descended into. The mid-session protected-file watch passes
// one cache to every periodic re-walk, so a pass can skip re-reading
// any directory whose fingerprint is unchanged. An unchanged directory
// cannot have gained or lost entries, so the cached subdirectory list
// keeps the traversal going and no new matches can hide inside it; a
// changed directory is read again like on the first pass.
//
// Why it exists: without the cache, every watch tick re-opened and
// re-read every directory of every granted root. Large grants are
// common — tool-chain and package-cache trees like ~/.cache/uv or an
// npm content store hold tens of thousands of directories — and on
// macOS every open()/readdir of a home-relative path pays a Seatbelt
// policy evaluation (`sandcastle_match_any_home_directory`). The
// full re-walk therefore burned multiple CPU cores permanently while
// the session sat idle. With the cache an idle pass costs one lstat
// per directory instead of open+readdir, and only directories that
// actually changed are read again.
//
// State is kept per scan root (the granted spelling), because two
// granted roots may overlap and each root's walk must count its own
// entries toward the scan cap. All methods are safe for the walk's
// concurrent per-root workers.
//
// Detection bound: a directory change that lands in the same
// coarse-clock tick as the previous read can share its timestamps (see
// dirFingerprint). The cache therefore flags a read that fell within
// dirTimestampWindow of the directory's mtime/ctime and lists that
// directory once more on the next pass (denyScanDirState.recheck), so a
// hidden change is detected one tick later. That covers filesystems
// whose timestamps are at most as coarse as dirTimestampWindow; the
// mid-session watch keeps an opt-in full pass as a backstop for coarser
// ones.
type denyScanCache struct {
	mu    sync.Mutex
	roots map[string]*denyScanRootState
}

// denyScanRootState is one root's cached walk state. The lstats,
// readdirs and skips counters are diagnostics: tests use them to assert
// unchanged directories are skipped; nothing else reads them.
type denyScanRootState struct {
	mu       sync.Mutex
	dirs     map[string]*denyScanDirState
	lstats   atomic.Int64
	readdirs atomic.Int64
	skips    atomic.Int64
}

// denyScanDirState is what the last pass saw in one directory. count is
// the number of readdir entries (what the walk charges against the
// scan cap); subdirs are the resolved paths the walk descended into,
// after the same pruning the walk applies — protected, skip-listed and
// glob-matched folders are not descended, so they are not listed.
type denyScanDirState struct {
	isDir   bool
	fp      dirFingerprint
	count   int64
	subdirs []string
	// recheck marks a read that happened within dirTimestampWindow of
	// the directory's own mtime/ctime. A later change in that same
	// coarse-clock tick would leave the fingerprint unchanged, so the
	// next pass must read this directory once more even though fp
	// matches.
	recheck bool
}

func newDenyScanCache() *denyScanCache {
	return &denyScanCache{roots: map[string]*denyScanRootState{}}
}

// state returns the per-root cache entry, creating it on first use.
func (c *denyScanCache) state(root string) *denyScanRootState {
	c.mu.Lock()
	defer c.mu.Unlock()
	rc, ok := c.roots[root]
	if !ok {
		rc = &denyScanRootState{dirs: map[string]*denyScanDirState{}}
		c.roots[root] = rc
	}
	return rc
}

// unchanged reports the cached state of dir when the walk can skip
// re-reading it. A non-directory (a granted file used as a scan root)
// never yields entries to match, so once known it is skipped for good;
// a directory is skipped only while its fingerprint is unchanged and
// its last read did not fall inside the coarse-timestamp window.
func (r *denyScanRootState) unchanged(dir string, fp dirFingerprint, isDir bool) (*denyScanDirState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.dirs[dir]
	if !ok || e.isDir != isDir {
		return nil, false
	}
	if e.recheck {
		return nil, false
	}
	if !isDir || e.fp.equal(fp) {
		return e, true
	}
	return nil, false
}

// record stores one directory's fresh read and reconciles the subtree:
// cached subdirectories that no longer appear in subdirs were removed
// or renamed, so their cached state is dropped. A later pass then reads
// such a path from scratch should it reappear.
func (r *denyScanRootState) record(dir string, fp dirFingerprint, isDir bool, count int64, subdirs []string) {
	// A read that lands in the same coarse-clock tick as the directory's
	// own timestamps leaves a window in which a later change reuses those
	// timestamps. Flag it so the next pass reads the directory once more
	// even though the fingerprint cannot tell the difference.
	recheck := false
	if isDir {
		for _, ts := range [...]time.Time{
			time.Unix(fp.mtimeSec, fp.mtimeNsec),
			time.Unix(fp.ctimeSec, fp.ctimeNsec),
		} {
			if age := time.Since(ts); age >= 0 && age <= dirTimestampWindow {
				recheck = true
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.dirs[dir]; ok {
		seen := make(map[string]bool, len(subdirs))
		for _, s := range subdirs {
			seen[s] = true
		}
		for _, s := range old.subdirs {
			if !seen[s] {
				r.forgetLocked(s)
			}
		}
	}
	r.dirs[dir] = &denyScanDirState{isDir: isDir, fp: fp, count: count, subdirs: subdirs, recheck: recheck}
}

// forget drops dir and everything below it from the cache, e.g. after
// an lstat failed between discovery and read.
func (r *denyScanRootState) forget(dir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgetLocked(dir)
}

func (r *denyScanRootState) forgetLocked(dir string) {
	e, ok := r.dirs[dir]
	if !ok {
		return
	}
	delete(r.dirs, dir)
	for _, s := range e.subdirs {
		r.forgetLocked(s)
	}
}
