package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/TNG/oh-my-agentic-coder/internal/opencodestate"
)

// Do not mask files inside writable parents: an absent file or a renamed
// directory defeats that approach on Linux. Keep the entire route read-only.
func protectDesktopStores(g *Grants, learn bool) error {
	if learn {
		return fmt.Errorf("--for-opencode-desktop cannot protect Desktop stores in --learn mode")
	}
	writes := append(append([]string{}, g.AllowPaths...), g.WritePaths...)
	for i, w := range writes {
		writes[i] = canonicalPath(w)
		if writes[i] == "" {
			return fmt.Errorf("cannot resolve Desktop write grant %q", w)
		}
	}
	paths := opencodestate.DesktopStorePaths()
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("Desktop store paths must be absolute")
		}
		for p := path; ; p = filepath.Dir(p) {
			fi, err := os.Lstat(p)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("inspect Desktop store path %q: %w", p, err)
			}
			if err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("Desktop store routing uses symlink %q; use real directories for trusted Desktop state", p)
			}
			for _, w := range writes {
				if isUnder(p, w) {
					return fmt.Errorf("Desktop store %q is mutable through write grant %q; grant project folders and only the Desktop opencode runtime subtree", path, w)
				}
			}
			if p == filepath.Dir(p) {
				break
			}
		}
	}
	g.ProtectedPaths = dedupe(append(g.ProtectedPaths, paths...))
	return nil
}
