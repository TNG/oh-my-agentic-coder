package sandboxrun

import (
	"fmt"
	"slices"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// WatchTuning carries the launcher-configurable knobs of
// WatchNewProtected. Zero fields fall back to the defaults.
type WatchTuning struct {
	// FullPassTicks enables an optional full uncached pass every Nth
	// tick. Zero (the default) leaves it off; N runs it every Nth tick.
	// The default walks incrementally and re-checks a directory once
	// when its last read fell inside the coarse-timestamp window, which
	// catches the same-tick case without a full pass. A full pass is a
	// backstop for filesystems whose timestamps are coarser than that
	// window; it trades detection latency (worst case N × tick interval)
	// and a periodic CPU burst for that extra coverage.
	FullPassTicks int

	// FullPassConcurrency is how many folders the optional full pass
	// reads in parallel. Zero (the default) runs it at the walk's normal
	// concurrency: a short full-core burst every FullPassTicks ticks.
	// A lower value flattens that burst at the cost of a proportionally
	// longer scan — at 2 a pass that took seconds takes most of the
	// tick interval, so the CPU drag can end up more noticeable than
	// the spike it replaces. It is clamped to at least 1 and at most
	// the walk's normal concurrency. Ignored while FullPassTicks is 0.
	FullPassConcurrency int
}

// resolvedTuning is WatchTuning with defaults filled in. fullPassTicks
// 0 means the full pass is disabled.
type resolvedTuning struct {
	fullPassTicks       int
	fullPassConcurrency int
}

func (t WatchTuning) resolve() resolvedTuning {
	r := resolvedTuning{fullPassTicks: t.FullPassTicks, fullPassConcurrency: t.FullPassConcurrency}
	if r.fullPassTicks < 0 {
		r.fullPassTicks = 0
	}
	return r
}

// fullPassDue reports whether this tick runs the optional full uncached
// pass. A cadence of 0 means the full pass is off.
func (t resolvedTuning) fullPassDue(ticks int) bool {
	return ticks > 0 && t.fullPassTicks > 0 && ticks%t.fullPassTicks == 0
}

// WatchNewProtected polls the deny-scan roots for files that match a
// protected pattern but were created after the initial snapshot,
// calling onNew for each. It blocks until stop is closed; the caller
// owns the goroutine.
//
// ResolveGrants masks only files that exist at launch, and the kernel
// enforcement it feeds (bwrap binds, Seatbelt profiles) is immutable
// for the session — so a mid-session match is readable by the agent.
// This closes the information gap: the parent notices and can warn
// the user that only a restart restores protection.
//
// Snapshot and ticks reuse resolveDenyPaths on the same roots, so a
// tick recomputes what launch would mask now. One alert per path is
// deliberate: a masked path keeps its mask even when the underlying
// file is recreated (the bind mount shadows the path, not the inode),
// and an already-alerted path must not re-alert on every tick.
//
// The walk carries a denyScanCache across ticks: the snapshot seeds it
// and every tick re-walks incrementally, re-reading only directories
// whose fingerprint changed since the previous pass. A tick's result
// lists only matches from re-read directories — every match of an
// unchanged directory was reported by an earlier pass, so new-match
// detection is unchanged — and an idle pass costs one lstat per
// directory instead of re-opening and re-reading the whole grant, which
// on macOS's Seatbelt-backed home paths burned multiple cores for as
// long as the session sat idle.
//
// Directory timestamps come from the filesystem's coarse clock, so a
// change can share the previous read's timestamps and slip past the
// fingerprint. A directory whose read fell within that coarse window is
// therefore listed once more on the next pass (see dirTimestampWindow),
// which catches the hidden change one tick later without a full re-walk.
// A full uncached pass remains available via tuning.FullPassTicks as an
// opt-in backstop for filesystems with timestamps coarser than that
// window; it is off by default.
func WatchNewProtected(p *sandboxprofile.Profile, workdir string, interval time.Duration, onNew func(path string), stop <-chan struct{}, tuning WatchTuning) error {
	t := tuning.resolve()
	base := sandboxprofile.PlatformBaseline()
	roots, err := denyScanRoots(p, workdir)
	if err != nil {
		return err
	}
	protected := sandboxprofile.EffectiveProtectedPaths(base, p.Filesystem.OverrideDeny)
	cache := newDenyScanCache()
	resolve := func(scanCache *denyScanCache, concurrency int) ([]string, error) {
		out, byName, err := resolveDenyPaths(p.Filesystem.Deny, base.WorkdirProtected, p.Filesystem.OverrideDeny, roots, protected, nil, scanCache, concurrency)
		if err == nil && len(byName.roots) > 0 {
			// The folder grew past the cap during the session. The profile
			// has no name rule for it, so stop as an incomplete scan does.
			err = fmt.Errorf("sandbox grants: filesystem.deny scan of %s hit the %d-entry limit during the session", byName.roots[0], denyScanLimit.Load())
		}
		return out, err
	}

	known := map[string]bool{}
	initial, byName, err := resolveDenyPaths(p.Filesystem.Deny, base.WorkdirProtected, p.Filesystem.OverrideDeny, roots, protected, nil, cache, denyScanConcurrency)
	if err != nil {
		return err
	}
	// A folder too large to scan is covered by a name rule (macOS), which
	// also hides files created during the session, so the ticks leave it
	// out instead of re-walking it up to the cap every interval.
	roots = slices.DeleteFunc(roots, func(r string) bool { return slices.Contains(byName.roots, r) })
	for _, m := range initial {
		known[m] = true
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	ticks := 0
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			ticks++
			// The optional full uncached pass re-reads every directory,
			// covering filesystems with timestamps coarser than the
			// per-directory re-check window. It leaves the cache
			// untouched; the next incremental pass re-reads the
			// directories that changed since it was last seeded, which
			// is redundant but never wrong.
			scanCache := cache
			concurrency := denyScanConcurrency
			if t.fullPassDue(ticks) {
				scanCache = nil
				// Opt-in throttle: a lower FullPassConcurrency trades a
				// shallower CPU burst for a longer scan (see
				// WatchTuning.FullPassConcurrency).
				if t.fullPassConcurrency > 0 {
					concurrency = t.fullPassConcurrency
				}
			}
			current, err := resolve(scanCache, concurrency)
			if err != nil {
				return err
			}
			for _, m := range current {
				if !known[m] {
					known[m] = true
					onNew(m)
				}
			}
		}
	}
}
