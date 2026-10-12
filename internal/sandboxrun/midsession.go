package sandboxrun

import (
	"fmt"
	"slices"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

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
func WatchNewProtected(p *sandboxprofile.Profile, workdir string, interval time.Duration, onNew func(path string), stop <-chan struct{}) error {
	base := sandboxprofile.PlatformBaseline()
	roots, err := denyScanRoots(p, workdir)
	if err != nil {
		return err
	}
	protected := sandboxprofile.EffectiveProtectedPaths(base, p.Filesystem.OverrideDeny)
	resolve := func() ([]string, error) {
		out, byName, err := resolveDenyPaths(p.Filesystem.Deny, base.WorkdirProtected, p.Filesystem.OverrideDeny, roots, protected, nil)
		if err == nil && len(byName.roots) > 0 {
			// The folder grew past the cap during the session. The profile
			// has no name rule for it, so stop as an incomplete scan does.
			err = fmt.Errorf("sandbox grants: filesystem.deny scan of %s hit the %d-entry limit during the session", byName.roots[0], denyScanLimit.Load())
		}
		return out, err
	}

	known := map[string]bool{}
	initial, byName, err := resolveDenyPaths(p.Filesystem.Deny, base.WorkdirProtected, p.Filesystem.OverrideDeny, roots, protected, nil)
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
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			current, err := resolve()
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
