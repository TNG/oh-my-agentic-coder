package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// namespaceSymlink joins two sandbox pathnames. Neither name is a new host
// bind source: this cannot expose data outside the mounts already admitted.
type namespaceSymlink struct {
	path   string
	target string
}

// binaryNamespaceLinks restores execution spellings without rebinding mutable
// host aliases. In particular, a granted alias's host target need not itself
// be mounted, while an ungranted interpreter alias still needs its original
// spelling for the kernel's shebang lookup.
func binaryNamespaceLinks(dirs []derivedReadDir, mounts []*mount) ([]namespaceSymlink, error) {
	dirs = append([]derivedReadDir(nil), dirs...)
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].path < dirs[j].path })
	var links []namespaceSymlink
	for _, d := range dirs {
		if namespaceHasDirectory(d.path, d.real, mounts, links) {
			continue
		}
		// --dir/--symlink must only modify the fresh namespace, never a
		// host-backed tree or a parent of an existing mount/link.
		conflict := innermostMount(d.path, mounts) != nil || aboveAnyMount(d.path, mounts)
		for _, link := range links {
			conflict = conflict || isUnder(d.path, link.path) || isUnder(link.path, d.path)
		}
		if conflict {
			return nil, fmt.Errorf("cannot restore command pathname %q inside an existing sandbox mount; grant the execution path explicitly", d.path)
		}
		target := ""
		if namespaceHasDirectory(d.real, d.real, mounts, links) {
			target = d.real
		} else {
			for _, m := range mounts {
				root := canonicalPath(m.path)
				if root == "" {
					continue
				}
				if rel, ok := directoryRelativeTo(d.real, root); ok {
					candidate := filepath.Join(m.path, rel)
					if namespaceHasDirectory(candidate, d.real, mounts, links) {
						target = candidate
						break
					}
				}
			}
		}
		if target == "" {
			return nil, fmt.Errorf("checked command directory %q has no visible sandbox path; grant its execution path explicitly", d.real)
		}
		links = append(links, namespaceSymlink{path: d.path, target: target})
	}
	return links, nil
}

func namespaceHasDirectory(path, real string, mounts []*mount, links []namespaceSymlink) bool {
	resolved, ok := sandboxResolveWithLinks(path, mounts, links)
	if !ok || innermostMount(resolved, mounts) == nil {
		return false
	}
	a, aerr := os.Stat(resolved)
	b, berr := os.Stat(real)
	return aerr == nil && berr == nil && a.IsDir() && os.SameFile(a, b)
}

// directoryRelativeTo preserves case-sensitive identities, including when
// two spellings identify the same directory on a case-insensitive volume.
func directoryRelativeTo(path, root string) (string, bool) {
	if isUnder(path, root) {
		rel, err := filepath.Rel(root, path)
		return rel, err == nil
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", false
	}
	rel := "."
	for p := path; ; p = filepath.Dir(p) {
		if pi, err := os.Stat(p); err == nil && os.SameFile(pi, info) {
			return rel, true
		}
		if p == filepath.Dir(p) {
			return "", false
		}
		rel = filepath.Join(filepath.Base(p), rel)
	}
}
