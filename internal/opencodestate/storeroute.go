package opencodestate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/pathutil"
)

// StoreRoute is the way to one of Desktop's stores (see DesktopStorePaths)
// through the symlinks on its path (docs/adr/0002-desktop-store-symlinks.md).
// Project discovery and the sandbox both decide with CoveredBy, so a
// project that discovery keeps never makes the sandbox refuse to start.
type StoreRoute struct {
	// Store is the path as written.
	Store string
	// Real is Store with every symlink resolved. If a link's target is
	// missing, Real is where the target would appear.
	Real string
	// Links are the symlinks passed on the way. An agent that can write
	// the folder holding one could re-point it.
	Links []string
}

// maxStoreLinks bounds the symlinks ResolveStoreRoute follows, like the
// kernel's limit on nested links.
const maxStoreLinks = 40

// ResolveStoreRoute resolves the symlinks on the way to an absolute store
// path one link at a time, like the kernel does. A store that is itself a
// symlink is refused, because its target could be any file.
func ResolveStoreRoute(store string) (StoreRoute, error) {
	if !filepath.IsAbs(store) {
		return StoreRoute{}, fmt.Errorf("OpenCode Desktop's file %q is not an absolute path. Set HOME to an absolute path", store)
	}
	route := StoreRoute{Store: store}
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
			route.Real = filepath.Join(append([]string{next}, pending...)...)
			return route, nil
		}
		if err != nil {
			return StoreRoute{}, fmt.Errorf("inspect %q on the way to OpenCode Desktop's file %q: %w", next, store, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			resolved = next
			continue
		}
		if len(pending) == 0 {
			return StoreRoute{}, fmt.Errorf("OpenCode Desktop's file %q is a symlink, so omac cannot keep it out of the agent's reach. Replace the link with the file it points to, or run without --for-opencode-desktop", store)
		}
		if len(route.Links) == maxStoreLinks {
			return StoreRoute{}, fmt.Errorf("the path to OpenCode Desktop's file %q goes through more than %d symlinks, so omac cannot check it. Shorten the chain of links, or run without --for-opencode-desktop", store, maxStoreLinks)
		}
		route.Links = append(route.Links, next)
		target, err := os.Readlink(next)
		if err != nil {
			return StoreRoute{}, fmt.Errorf("inspect %q on the way to OpenCode Desktop's file %q: %w", next, store, err)
		}
		if filepath.IsAbs(target) {
			resolved = "/"
		}
		pending = append(strings.Split(target, string(filepath.Separator)), pending...)
	}
	route.Real = resolved
	return route, nil
}

// CoveredBy reports whether access to dir lets the agent reach the store:
// dir, with its symlinks resolved, holds the store as written, its real
// path, or one of the links on the way. A relative dir covers nothing.
func (r StoreRoute) CoveredBy(dir string) bool {
	dir = pathutil.Canonical(dir)
	if !filepath.IsAbs(dir) {
		return false
	}
	prefix := strings.TrimSuffix(dir, string(filepath.Separator)) + string(filepath.Separator)
	for _, p := range append([]string{r.Store, r.Real}, r.Links...) {
		if p == dir || strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// desktopStoreRoutes resolves the route to each of Desktop's stores. A
// store whose route can't be resolved is left out: the sandbox refuses to
// start because of it anyway and says why.
func desktopStoreRoutes() []StoreRoute {
	var routes []StoreRoute
	for _, store := range DesktopStorePaths() {
		if route, err := ResolveStoreRoute(store); err == nil {
			routes = append(routes, route)
		}
	}
	return routes
}
