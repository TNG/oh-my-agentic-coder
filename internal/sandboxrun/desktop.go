package sandboxrun

import (
	"fmt"
	"path/filepath"

	"github.com/TNG/oh-my-agentic-coder/internal/opencodestate"
	"github.com/TNG/oh-my-agentic-coder/internal/pathutil"
)

// protectDesktopStores keeps OpenCode Desktop's own stores (its project
// list and saved servers) out of the agent's reach for
// --for-opencode-desktop. Masking files inside a writable parent is not
// enough: on Linux an absent file or a renamed directory defeats it. So
// refuse any write grant that covers a store or one of its parent
// directories, and then deny the store paths themselves (reading and
// writing). A route through symlinks is checked with the same
// opencodestate.StoreRoute that project discovery uses, and denied as
// written and resolved (docs/adr/0002-desktop-store-symlinks.md).
//
// Read grants that cover a store's folder are refused too: Desktop keeps
// its cookies and browser storage next to the stores, and on Linux bwrap
// can hide only the stores that exist at launch.
func protectDesktopStores(g *Grants, learn bool) error {
	if learn {
		return fmt.Errorf("cannot be combined with --learn, because learn mode lifts the file restrictions that keep OpenCode Desktop's project list out of the agent's reach. Remove --learn")
	}
	grants := append(append([]string{}, g.AllowPaths...), g.WritePaths...)
	writes, err := canonicalGrants("write", grants)
	if err != nil {
		return err
	}
	reads, err := canonicalGrants("read", g.ReadPaths)
	if err != nil {
		return err
	}
	paths := opencodestate.DesktopStorePaths()
	if len(paths) == 0 {
		return fmt.Errorf("cannot find OpenCode Desktop's project list because HOME is not set and XDG_STATE_HOME is not an absolute path, so omac cannot keep it out of the agent's reach. Set HOME, or export XDG_STATE_HOME as an absolute path")
	}
	workdir := pathutil.Canonical(g.Workdir)
	denied := append([]string{}, paths...)
	for _, path := range paths {
		route, err := opencodestate.ResolveStoreRoute(path)
		if err != nil {
			return err
		}
		denied = append(denied, route.Real)
		for i, w := range writes {
			if route.CoveredBy(w) {
				return fmt.Errorf("the agent could change OpenCode Desktop's file %q through the write grant %q%s", path, grants[i], narrowGrantAdvice(w, workdir))
			}
		}
		for i, r := range reads {
			if route.CoveredBy(r) {
				return fmt.Errorf("the agent could read OpenCode Desktop's folder %q, which holds Desktop's cookies and browser storage, through the read grant %q%s", filepath.Dir(path), g.ReadPaths[i], narrowGrantAdvice(r, workdir))
			}
		}
	}
	g.ProtectedPaths = dedupe(append(g.ProtectedPaths, denied...))
	return nil
}

// canonicalGrants resolves the symlinks in each grant of the given kind
// ("write" or "read"), keeping the order.
func canonicalGrants(kind string, grants []string) ([]string, error) {
	out := make([]string, len(grants))
	for i, grant := range grants {
		out[i] = pathutil.Canonical(grant)
		if !filepath.IsAbs(out[i]) {
			return nil, fmt.Errorf("cannot resolve the %s grant %q, so omac cannot check that it keeps OpenCode Desktop's project list out of the agent's reach. Remove that grant or fix its path", kind, grant)
		}
	}
	return out, nil
}

// narrowGrantAdvice tells a user of omac serve how to remove a grant that
// covers Desktop's files. Both arguments are canonical paths.
func narrowGrantAdvice(grant, workdir string) string {
	if grant == workdir {
		return ", the folder omac serve was started in. Start omac serve from a project folder instead"
	}
	return `. Narrow that grant to your project folders in the "filesystem" section of your sandbox profile`
}

// desktopRefusal is an error from protectDesktopStores. Only omac serve
// passes --for-opencode-desktop, so Run reports it as serve's own error.
type desktopRefusal struct{ err error }

func (r desktopRefusal) Error() string { return "--for-opencode-desktop: " + r.err.Error() }
func (r desktopRefusal) Unwrap() error { return r.err }
