package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// validateRequiredProtection fails closed when a backend cannot keep a trusted
// startup input immutable. Ordinary best-effort denies retain their behavior.
func (g *Grants) validateRequiredProtection(linux bool) error {
	if len(g.RequiredProtectedPaths) == 0 {
		return nil
	}
	writable := append(append([]string{}, g.WritePaths...), g.AllowPaths...)
	writableForms := make(map[string][]string, len(writable))
	for _, w := range writable {
		roots := pathForms(w)
		if parent := canonicalPath(filepath.Dir(w)); parent != "" {
			roots = append(roots, filepath.Join(parent, filepath.Base(w)))
		}
		writableForms[w] = dedupe(roots)
	}
	for _, p := range g.RequiredProtectedPaths {
		if !filepath.IsAbs(p) || !coveredByProtected(p, g.ProtectedPaths) {
			return fmt.Errorf("required protection for %q is disabled; keep its --deny-literal and remove --learn", p)
		}
		dependencies, err := requiredRoutingDependencies(p)
		if err != nil {
			return fmt.Errorf("cannot protect Desktop store %q: %w; replace the symlink with real directories or create its target on the host, then restart", p, err)
		}
		pinned := ancestorDirs([]string{p})
		for _, dependency := range dependencies {
			// Ordinary ancestors are pinned by Seatbelt and checked against
			// Linux grants below. A directory traversed before .. can
			// disappear from those final-path checks, however.
			if !dependency.symlink && slices.Contains(pinned, dependency.path) {
				continue
			}
			for _, w := range writable {
				for _, root := range writableForms[w] {
					// Compare the ENTRY, not its final target. A mutable
					// intermediate node can reroute an otherwise denied store.
					if isUnder(dependency.path, root) {
						return fmt.Errorf("cannot protect Desktop store %q: symlink routing dependency %q is writable via grant %q; replace the link with a direct real-directory path or narrow the writable grant", p, dependency.path, w)
					}
				}
			}
		}
		if !linux {
			continue
		}
		// A file mask cannot cover an absent file, or keep a writable
		// enclosing directory from being moved out of its masked spelling.
		// Sibling RW mounts and RO ancestors do not have either weakness.
		for _, w := range writable {
			if pathsOverlapBelow(p, w) {
				return fmt.Errorf("cannot protect Desktop store %q under writable Linux grant %q (including missing files and directory replacement); narrow that grant/workdir to project folders and the opencode runtime subtree, or move Desktop state outside it", p, w)
			}
		}
		for _, r := range g.ReadPaths {
			if pathsOverlapBelow(p, r) && !isUnder(p, r) {
				return fmt.Errorf("cannot protect Desktop store %q through aliased Linux read grant %q; grant its real path instead, or narrow the grant", p, r)
			}
		}
	}
	return nil
}

type requiredRoutingDependency struct {
	path    string
	symlink bool
}

// requiredRoutingDependencies uses the same component-wise walk and hop bound as
// sandboxResolve, but follows the host route rather than a mount namespace.
// EvalSymlinks alone loses intermediate dependencies. Dangling links refuse
// outright; missing ordinary directories are creation-guarded by Seatbelt.
func requiredRoutingDependencies(path string) ([]requiredRoutingDependency, error) {
	cur := "/"
	rest := strings.Split(filepath.Clean(path), "/")
	var dependencies []requiredRoutingDependency
	for hops := 0; len(rest) > 0; {
		name := rest[0]
		rest = rest[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		fi, err := os.Lstat(next)
		if os.IsNotExist(err) {
			return dependencies, nil // No existing routing nodes below this missing prefix.
		}
		if err != nil {
			return nil, fmt.Errorf("cannot inspect symlink routing at %q: %w", next, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			if fi.IsDir() {
				dependencies = append(dependencies, requiredRoutingDependency{path: next})
			}
			cur = next
			continue
		}
		if hops++; hops > maxSymlinkHops {
			return nil, fmt.Errorf("symlink routing exceeds %d hops at %q", maxSymlinkHops, next)
		}
		if _, err := os.Stat(next); err != nil {
			return nil, fmt.Errorf("dangling or unresolvable symlink routing at %q: %w", next, err)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return nil, fmt.Errorf("read symlink routing at %q: %w", next, err)
		}
		dependencies = append(dependencies, requiredRoutingDependency{path: next, symlink: true})
		if filepath.IsAbs(target) {
			cur = "/"
		}
		// Do not Clean/Join target: b/.. must resolve b (possibly another
		// symlink) before applying .., just as the filesystem does.
		rest = append(strings.Split(target, "/"), rest...)
	}
	return dependencies, nil
}

func coveredByProtected(p string, protected []string) bool {
	for _, d := range protected {
		if isUnder(p, d) {
			return true
		}
	}
	return false
}

func pathsOverlapBelow(p, root string) bool {
	for _, pf := range pathForms(p) {
		for _, rf := range pathForms(root) {
			if isUnder(pf, rf) {
				return true
			}
		}
	}
	return false
}
