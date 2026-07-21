//go:build e2e || e2e_fast

package e2e

import (
	"os"
	"path/filepath"
)

// hostCacheProbeMarker is the filename the self-audit cache probe writes into
// $HOME/.cache (and $HOME/Library/Caches) from inside the sandbox. The harness
// checks host-side that it never lands in the real cache roots — see
// hostCacheLeak and assertCacheHostRootDenied.
const hostCacheProbeMarker = "audit-host-marker.txt"

// hostCacheLeak reports whether the self-audit probe marker leaked into one of
// home's real cache roots (~/.cache or ~/Library/Caches), returning the leaked
// path when found.
//
// It deliberately ignores the scoped ~/.cache/omac/<digest> subtree: the probe
// writes to the cache ROOT, and only that root is the security boundary under
// test (issue #149). A successful in-sandbox write to the string path
// $HOME/.cache is not itself a breach — on Linux that path is a throwaway
// tmpfs mount-point parent bubblewrap auto-creates to host the scoped
// tool-cache bind — so this host-side presence check, not the probe's write
// result, is authoritative.
func hostCacheLeak(home string) (string, bool) {
	for _, root := range []string{
		filepath.Join(home, ".cache"),
		filepath.Join(home, "Library", "Caches"),
	} {
		leaked := filepath.Join(root, hostCacheProbeMarker)
		if _, err := os.Stat(leaked); err == nil {
			return leaked, true
		}
	}
	return "", false
}
