package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/opencodestate"
)

// protectDesktopStores keeps OpenCode Desktop's own stores (its project
// list and saved servers) out of the agent's reach for
// --for-opencode-desktop. Masking files inside a writable parent is not
// enough: on Linux an absent file or a renamed directory defeats it. So
// refuse any write grant that covers a store or one of its parent
// directories, and then deny the store paths themselves (reading and
// writing). A route through symlinks is checked as written, at every
// link and resolved, and denied as written and resolved
// (docs/adr/0002-desktop-store-symlinks.md).
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
	workdir := canonicalPath(g.Workdir)
	denied := append([]string{}, paths...)
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("OpenCode Desktop's file %q is not an absolute path. Set HOME to an absolute path", path)
		}
		real, links, err := resolveStorePath(path)
		if err != nil {
			return err
		}
		denied = append(denied, real)
		for _, route := range append([]string{path, real}, links...) {
			for p := route; ; p = filepath.Dir(p) {
				for i, w := range writes {
					if isUnder(p, w) {
						return fmt.Errorf("the agent could change OpenCode Desktop's file %q through the write grant %q%s", path, grants[i], narrowGrantAdvice(w, workdir))
					}
				}
				for i, r := range reads {
					if isUnder(p, r) {
						return fmt.Errorf("the agent could read OpenCode Desktop's folder %q, which holds Desktop's cookies and browser storage, through the read grant %q%s", filepath.Dir(path), g.ReadPaths[i], narrowGrantAdvice(r, workdir))
					}
				}
				if p == filepath.Dir(p) {
					break
				}
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
		out[i] = canonicalPath(grant)
		if out[i] == "" {
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

// resolveStorePath resolves the symlinks on the way to a store one link at
// a time, like the kernel does. It returns the real path and every link
// it passed, so the caller can check the folders that hold them. A link
// whose target is missing is followed as written: the real path is then
// where the target would appear. A store that is itself a symlink is
// refused, because its target could be any file.
func resolveStorePath(store string) (real string, links []string, err error) {
	resolved := "/"
	pending := strings.Split(store, string(filepath.Separator))
	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
			continue
		}
		next := filepath.Join(resolved, name)
		fi, err := os.Lstat(next)
		if os.IsNotExist(err) {
			return filepath.Join(append([]string{next}, pending...)...), links, nil
		}
		if err != nil {
			return "", nil, fmt.Errorf("inspect %q on the way to OpenCode Desktop's file %q: %w", next, store, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if len(pending) == 0 {
			return "", nil, fmt.Errorf("OpenCode Desktop's file %q is a symlink, so omac cannot keep it out of the agent's reach. Replace the link with the file it points to, or run without --for-opencode-desktop", store)
		}
		if len(links) == maxStoreLinks {
			return "", nil, fmt.Errorf("the path to OpenCode Desktop's file %q goes through more than %d symlinks, so omac cannot check it. Shorten the chain of links, or run without --for-opencode-desktop", store, maxStoreLinks)
		}
		links = append(links, next)
		target, err := os.Readlink(next)
		if err != nil {
			return "", nil, fmt.Errorf("inspect %q on the way to OpenCode Desktop's file %q: %w", next, store, err)
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	return resolved, links, nil
}

// maxStoreLinks bounds the symlinks resolveStorePath follows, like the
// kernel's limit on nested links.
const maxStoreLinks = 40
