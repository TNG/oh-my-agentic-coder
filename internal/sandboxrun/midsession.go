package sandboxrun

import (
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
	scan, err := denyScanRoots(p, workdir)
	if err != nil {
		return err
	}
	protected := sandboxprofile.EffectiveProtectedPaths(base, p.Filesystem.OverrideDeny)
	resolve := func(roots []string) ([]string, []string) {
		return resolveDenyPaths(p.Filesystem.Deny, base.WorkdirProtected, p.Filesystem.OverrideDeny, roots, protected, scan.toolchain, nil)
	}

	known := map[string]bool{}
	initial, incomplete := resolve(scan.roots)
	for _, m := range initial {
		known[m] = true
	}
	// A root the launch scan could not finish (launch asked the user
	// about it) is left out of the ticks: re-walking it up to the cap
	// every tick is costly, and where the cap falls shifts as files come
	// and go, so a tick would report old files as new.
	roots := slices.DeleteFunc(slices.Clone(scan.roots), func(r string) bool { return slices.Contains(incomplete, r) })

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-ticker.C:
			current, _ := resolve(roots)
			for _, m := range current {
				if !known[m] {
					known[m] = true
					onNew(m)
				}
			}
		}
	}
}
