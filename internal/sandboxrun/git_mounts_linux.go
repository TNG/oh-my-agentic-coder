//go:build linux

package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type gitMountView struct {
	path, device, root string
}

func validateGitMountAliases(g *Grants) error {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("Git prototype cannot inspect mount aliases: %w", err)
	}
	return checkGitMountAliases(g, string(data))
}

func checkGitMountAliases(g *Grants, data string) error {
	var mounts []gitMountView
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(strings.TrimSpace(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			return fmt.Errorf("Git prototype cannot parse mount table")
		}
		root, path := unescape.Replace(fields[3]), unescape.Replace(fields[4])
		if !filepath.IsAbs(root) || !filepath.IsAbs(path) {
			return fmt.Errorf("Git prototype requires absolute mount paths")
		}
		mounts = append(mounts, gitMountView{path: path, device: fields[2], root: root})
	}
	views := func(path string) []gitMountView {
		var base gitMountView
		var nested []gitMountView
		for _, m := range mounts {
			if isUnder(path, m.path) && len(m.path) >= len(base.path) {
				base = m
			} else if path != m.path && isUnder(m.path, path) {
				nested = append(nested, m)
			}
		}
		if base.path != "" {
			rel, _ := filepath.Rel(base.path, path)
			nested = append(nested, gitMountView{path: path, device: base.device, root: filepath.Join(base.root, rel)})
		}
		return nested
	}
	protected := append([]string{filepath.Join(g.Workdir, ".git"), "/usr", "/lib", "/lib64", "/bin", "/sbin"}, g.ProtectedPaths...)
	var sensitive []gitMountView
	for _, path := range protected {
		sensitive = append(sensitive, views(path)...)
	}
	for _, path := range append(append([]string{}, g.AllowPaths...), g.WritePaths...) {
		for _, w := range views(path) {
			for _, p := range sensitive {
				if w.device == p.device && (isUnder(w.root, p.root) || isUnder(p.root, w.root)) && !isUnder(w.path, p.path) && !isUnder(p.path, w.path) {
					return fmt.Errorf("Git prototype refuses writable mount alias %s of protected path %s", w.path, p.path)
				}
			}
		}
	}
	return nil
}
