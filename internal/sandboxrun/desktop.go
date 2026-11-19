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
// directories, and then deny the store paths themselves (reading and
// writing). A route through a symlink is checked and denied both as
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
	workdir := canonicalPath(g.Workdir)
	denied := append([]string{}, paths...)
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("OpenCode Desktop's file %q is not an absolute path. Set HOME to an absolute path", path)
		}
		real, err := realStorePath(path)
		if err != nil {
			return err
		}
		denied = append(denied, real)
		for _, route := range []string{path, real} {
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

// realStorePath returns the store path with every symlink resolved. A
// link whose target is missing is followed as written, so the check also
// covers the place where its target would appear. A store that is itself
// a symlink is refused: its target could be any file.
func realStorePath(store string) (string, error) {
	if fi, err := os.Lstat(store); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("OpenCode Desktop's file %q is a symlink, so omac cannot keep it out of the agent's reach. Replace the link with the file it points to, or run without --for-opencode-desktop", store)
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect OpenCode Desktop's file %q: %w", store, err)
	}
	path := store
	for range maxDanglingLinks {
		link, err := danglingLink(path)
		if err != nil {
			return "", err
		}
		if link == "" {
			if real := canonicalPath(path); real != "" {
				return real, nil
			}
			break
		}
		target, err := os.Readlink(link)
		if err != nil {
			return "", fmt.Errorf("inspect OpenCode Desktop's folder %q: %w", link, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(canonicalPath(filepath.Dir(link)), target)
		}
		rest, _ := filepath.Rel(link, path)
		path = filepath.Join(target, rest)
	}
	return "", fmt.Errorf("cannot resolve OpenCode Desktop's file %q, so omac cannot keep it out of the agent's reach", store)
}

// maxDanglingLinks bounds the chain of missing link targets realStorePath
// follows, like the kernel's limit on nested links.
const maxDanglingLinks = 40

// danglingLink returns the lowest folder above path that is a symlink to
// a missing target, or "" when there is none.
func danglingLink(path string) (string, error) {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		fi, err := os.Lstat(dir)
		if err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("inspect OpenCode Desktop's folder %q: %w", dir, err)
		}
		if err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if _, err := os.Stat(dir); os.IsNotExist(err) {
				return dir, nil
			} else if err != nil {
				return "", fmt.Errorf("inspect OpenCode Desktop's folder %q: %w", dir, err)
			}
		}
		if dir == filepath.Dir(dir) {
			return "", nil
		}
	}
}
