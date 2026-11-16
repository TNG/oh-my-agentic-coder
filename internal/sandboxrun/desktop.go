package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/TNG/oh-my-agentic-coder/internal/opencodestate"
)

// protectDesktopStores keeps OpenCode Desktop's own stores (its project
// list and saved servers) out of the agent's reach for
// --for-opencode-desktop. Masking files inside a writable parent is not
// enough: on Linux an absent file or a renamed directory defeats it. So
// refuse any write grant that covers a store or one of its parent
// directories, refuse symlinks on that route, and then deny the store
// paths themselves (reading and writing).
func protectDesktopStores(g *Grants, learn bool) error {
	if learn {
		return fmt.Errorf("--for-opencode-desktop cannot be combined with --learn: learn mode lifts the file restrictions that keep OpenCode Desktop's project list out of the agent's reach. Remove --learn")
	}
	grants := append(append([]string{}, g.AllowPaths...), g.WritePaths...)
	writes := make([]string, len(grants))
	for i, w := range grants {
		writes[i] = canonicalPath(w)
		if writes[i] == "" {
			return fmt.Errorf("cannot resolve the write grant %q, so omac cannot check that it keeps OpenCode Desktop's project list out of the agent's reach. Remove that grant or fix its path", w)
		}
	}
	paths := opencodestate.DesktopStorePaths()
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("the OpenCode Desktop project store path %q is not absolute. Set HOME, XDG_CONFIG_HOME, XDG_DATA_HOME and XDG_STATE_HOME to absolute paths", path)
		}
		for p := path; ; p = filepath.Dir(p) {
			fi, err := os.Lstat(p)
			if err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("inspect OpenCode Desktop project store path %q: %w", p, err)
			}
			if err == nil && fi.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("cannot protect the OpenCode Desktop project store %q because its path goes through the symlink %q, and omac protects Desktop's project list only on paths without symlinks. Point HOME, XDG_CONFIG_HOME, XDG_DATA_HOME and XDG_STATE_HOME at the real directories, or run without --for-opencode-desktop", path, p)
			}
			for i, w := range writes {
				if isUnder(p, w) {
					return fmt.Errorf("the OpenCode Desktop project store %q would be writable by the agent through the write grant %q. Narrow that grant (sandbox profile, --allow or --write) to your project folders", path, grants[i])
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
