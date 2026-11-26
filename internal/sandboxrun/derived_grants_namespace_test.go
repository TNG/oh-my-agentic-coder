package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// planBinaryNamespace uses the same checked plan consumed by the Linux backend.
func planBinaryNamespace(t *testing.T, g *Grants, inner []string) []string {
	t.Helper()
	dirs, names, err := resolveInnerBinaryPlan(inner, derivedGrantPolicyForGrants(g))
	if err != nil {
		t.Fatal(err)
	}
	g.derivedReadPaths, g.derivedReadDirs = dirs, names
	return dirs
}

func namespaceFromBwrap(argv []string) ([]*mount, []namespaceSymlink) {
	var mounts []*mount
	var links []namespaceSymlink
	for i := 0; i < len(argv) && argv[i] != "--"; i++ {
		switch argv[i] {
		case "--bind", "--ro-bind":
			mounts = append(mounts, &mount{path: argv[i+2], rw: argv[i] == "--bind"})
			i += 2
		case "--symlink":
			links = append(links, namespaceSymlink{path: argv[i+2], target: argv[i+1]})
			i += 2
		}
	}
	return mounts, links
}

func TestDerivedGrantsLinuxCanonicalGrantAlias(t *testing.T) {
	home, wd := stableHome(t), t.TempDir()
	realRoot := filepath.Join(home, "external-runtime")
	realTool := filepath.Join(realRoot, "bin", "tool")
	review446Script(t, realTool)
	alias := filepath.Join(home, ".nvm")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(alias, "bin", "tool")
	for _, mode := range []string{"read", "write", "allow"} {
		t.Run(mode, func(t *testing.T) {
			g := minimalDerivedGrants(wd, home)
			g.ReadPaths = []string{"/bin", "/usr"}
			switch mode {
			case "read":
				g.ReadPaths = append(g.ReadPaths, alias)
			case "write":
				g.WritePaths = []string{alias}
			case "allow":
				g.AllowPaths = append(g.AllowPaths, alias)
			}
			dirs := planBinaryNamespace(t, g, []string{tool})
			argv, err := BuildBwrapArgv(g, []string{resolveInnerBinaryPath([]string{tool})})
			if err != nil {
				t.Fatal(err)
			}
			mounts, links := namespaceFromBwrap(argv)
			real, err := filepath.EvalSymlinks(realTool)
			if err != nil {
				t.Fatal(err)
			}
			_, reachable := sandboxResolveWithLinks(real, mounts, links)
			t.Logf("derived=%v; stage2=%s; reachable=%v; argv=%s", dirs, real, reachable, strings.Join(argv, " "))
			if !reachable {
				t.Fatal("canonical target already authorized via alias, but Linux stage2 executes an unmounted canonical pathname")
			}
			if !slices.Contains(links, namespaceSymlink{path: filepath.Dir(realTool), target: filepath.Join(alias, "bin")}) {
				t.Fatal("canonical execution directory must link to the already-visible explicit grant")
			}
			for _, m := range mounts {
				if m.path == filepath.Dir(realTool) {
					t.Fatal("must not fix visibility by binding a mutable canonical descendant")
				}
			}
		})
	}
}

func TestDerivedGrantsRelativeSymlinkTraversal(t *testing.T) {
	home, wd := stableHome(t), t.TempDir()
	trusted := filepath.Join(home, "trusted")
	review446Script(t, filepath.Join(trusted, "bin", "tool"))
	if err := os.MkdirAll(filepath.Join(wd, "pivot"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(wd, "pivot"), filepath.Join(trusted, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wd, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	review446Script(t, filepath.Join(wd, "bin", "tool"))
	link := filepath.Join(trusted, "link")
	if err := os.Symlink("alias/../bin/tool", link); err != nil {
		t.Fatal(err)
	}
	real, _, hops, err := walkSymlinkPath(link, false)
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(link)
	if err != nil || real != want {
		t.Fatalf("kernel-order traversal got %s, want %s, err %v", real, want, err)
	}
	if !pathCoveredByAny(filepath.Join(wd, "pivot"), hops) {
		t.Fatalf("missing writable traversal history: %v", hops)
	}
	g := minimalDerivedGrants(wd, home)
	g.ReadPaths = []string{"/bin", "/usr"}
	if _, err := resolveInnerBinaryDirs([]string{link}, derivedGrantPolicyForGrants(g)); err == nil {
		t.Fatal("relative link through writable component must taint derivation")
	}
}

func TestDerivedGrantsLinuxEnvAncestorAlias(t *testing.T) {
	home, wd := stableHome(t), t.TempDir()
	realRoot := filepath.Join(home, "runtime")
	review446Script(t, filepath.Join(realRoot, "bin", "tool"))
	alias := filepath.Join(home, "runtime-current")
	if err := os.Symlink("runtime", alias); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(alias, "bin", "tool")
	g := minimalDerivedGrants(wd, home)
	g.ReadPaths = []string{"/bin", "/usr"}
	inner := []string{"/usr/bin/env", "FOO=bar", tool}
	dirs := planBinaryNamespace(t, g, inner)
	argv, err := BuildBwrapArgv(g, inner)
	if err != nil {
		t.Fatal(err)
	}
	mounts, links := namespaceFromBwrap(argv)
	_, reachable := sandboxResolveWithLinks(tool, mounts, links)
	t.Logf("derived=%v; env-command=%s; reachable=%v; argv=%s", dirs, tool, reachable, strings.Join(argv, " "))
	if !reachable {
		t.Fatal("Linux does not rewrite the wrapped command; canonical-only grants removed its ancestor alias from the namespace")
	}
	if !slices.Contains(links, namespaceSymlink{path: filepath.Dir(tool), target: filepath.Join(realRoot, "bin")}) {
		t.Fatal("env's original command spelling must link to the checked runtime directory")
	}
}

func TestDerivedGrantsLinuxShebangAncestorAlias(t *testing.T) {
	home, wd := stableHome(t), t.TempDir()
	realRoot := filepath.Join(home, "runtime")
	review446Script(t, filepath.Join(realRoot, "bin", "interp"))
	alias := filepath.Join(home, "runtime-current")
	if err := os.Symlink("runtime", alias); err != nil {
		t.Fatal(err)
	}
	interp := filepath.Join(alias, "bin", "interp")
	tool := filepath.Join(home, "tools", "tool")
	review446Script(t, tool)
	if err := os.WriteFile(tool, []byte("#!"+interp+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	g := minimalDerivedGrants(wd, home)
	g.ReadPaths = []string{"/bin", "/usr"}
	dirs := planBinaryNamespace(t, g, []string{tool})
	argv, err := BuildBwrapArgv(g, []string{resolveInnerBinaryPath([]string{tool})})
	if err != nil {
		t.Fatal(err)
	}
	mounts, links := namespaceFromBwrap(argv)
	_, reachable := sandboxResolveWithLinks(interp, mounts, links)
	t.Logf("derived=%v; shebang-interpreter=%s; reachable=%v", dirs, interp, reachable)
	if !reachable {
		t.Fatal("kernel uses the script's original shebang; mounting only the canonical interpreter directory loses its ancestor alias")
	}
	if !slices.Contains(links, namespaceSymlink{path: filepath.Dir(interp), target: filepath.Join(realRoot, "bin")}) {
		t.Fatal("kernel's original shebang spelling must link to the checked interpreter directory")
	}
}

func TestDerivedGrantsNamespaceAliasSwapDoesNotAddHostSource(t *testing.T) {
	home := stableHome(t)
	realDir := filepath.Join(home, "runtime", "bin")
	review446Script(t, filepath.Join(realDir, "tool"))
	alias := filepath.Join(home, "runtime-current")
	if err := os.Symlink("runtime", alias); err != nil {
		t.Fatal(err)
	}
	tool := filepath.Join(alias, "bin", "tool")
	g := minimalDerivedGrants(t.TempDir(), home)
	g.ReadPaths = []string{"/bin", "/usr"}
	planBinaryNamespace(t, g, []string{"/usr/bin/env", "FOO=bar", tool})
	secret := filepath.Join(home, ".config", "private")
	review446Script(t, filepath.Join(secret, "bin", "tool"))
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, alias); err != nil {
		t.Fatal(err)
	}
	argv, err := BuildBwrapArgv(g, []string{"/usr/bin/env", "FOO=bar", tool})
	if err != nil {
		t.Fatal(err)
	}
	mounts, links := namespaceFromBwrap(argv)
	if !slices.Contains(links, namespaceSymlink{path: filepath.Dir(tool), target: realDir}) {
		t.Fatal("lowering must keep the checked target, not re-follow the changed host alias")
	}
	for _, m := range mounts {
		if isUnder(m.path, alias) || isUnder(m.path, secret) {
			t.Fatalf("unchecked alias/secret became a new host bind: %v", argv)
		}
	}
}

func TestDerivedGrantsNamespaceMutableSupportSwapFailsClosed(t *testing.T) {
	home, wd := stableHome(t), t.TempDir()
	tool := filepath.Join(wd, "prefix", "bin", "tool")
	review446Script(t, tool)
	benign := filepath.Join(wd, "benign")
	if err := os.Mkdir(benign, 0o755); err != nil {
		t.Fatal(err)
	}
	lib := filepath.Join(wd, "prefix", "lib")
	if err := os.Symlink(benign, lib); err != nil {
		t.Fatal(err)
	}
	g := minimalDerivedGrants(wd, home)
	g.ReadPaths = []string{"/bin", "/usr"}
	planBinaryNamespace(t, g, []string{tool})
	if _, err := BuildBwrapArgv(g, []string{tool}); err != nil {
		t.Fatalf("control: covered benign support alias must work: %v", err)
	}
	secret := filepath.Join(home, ".config", "private")
	review446Script(t, filepath.Join(secret, "tool"))
	if err := os.Remove(lib); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, lib); err != nil {
		t.Fatal(err)
	}
	if argv, err := BuildBwrapArgv(g, []string{tool}); err == nil || argv != nil {
		t.Fatal("must not restore a namespace link inside the writable host-backed grant after the swap")
	}
}

func TestDerivedGrantsNamespaceProtectsCanonicalAliasPaths(t *testing.T) {
	home := stableHome(t)
	realRoot := filepath.Join(home, "runtime")
	review446Script(t, filepath.Join(realRoot, "bin", "tool"))
	secret := filepath.Join(realRoot, "bin", "token")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "runtime-current")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	g := minimalDerivedGrants(t.TempDir(), home)
	g.ReadPaths = []string{"/bin", "/usr", alias}
	g.ProtectedPaths = []string{secret}
	tool := filepath.Join(alias, "bin", "tool")
	planBinaryNamespace(t, g, []string{tool})
	argv, err := BuildBwrapArgv(g, []string{resolveInnerBinaryPath([]string{tool})})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, " "), "--ro-bind /dev/null "+filepath.Join(alias, "bin", "token")) {
		t.Fatalf("a protected canonical name must be masked at the backing namespace path: %v", argv)
	}
}

func TestDerivedGrantsNamespaceParentDirectoryIsNotReadableCoverage(t *testing.T) {
	home := stableHome(t)
	tool := filepath.Join(home, "runtime", "bin", "tool")
	review446Script(t, tool)
	child := filepath.Join(filepath.Dir(tool), "child")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	mounts := []*mount{{path: child}}
	if namespaceHasDirectory(filepath.Dir(tool), filepath.Dir(tool), mounts, nil) {
		t.Fatal("bwrap-created ancestors of mounts do not expose their host directory contents")
	}
}

func TestDerivedGrantsNamespaceProtectsOriginalAliasPaths(t *testing.T) {
	home := stableHome(t)
	realRoot := filepath.Join(home, "runtime")
	review446Script(t, filepath.Join(realRoot, "bin", "tool"))
	secret := filepath.Join(realRoot, "bin", "token")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(home, "runtime-current")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	g := minimalDerivedGrants(t.TempDir(), home)
	g.ReadPaths = []string{"/bin", "/usr"}
	g.ProtectedPaths = []string{filepath.Join(alias, "bin", "token")}
	tool := filepath.Join(alias, "bin", "tool")
	planBinaryNamespace(t, g, []string{"/usr/bin/env", tool})
	argv, err := BuildBwrapArgv(g, []string{"/usr/bin/env", tool})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, " "), "--ro-bind /dev/null "+secret) {
		t.Fatalf("protected alias spelling must be masked at its canonical backing path: %v", argv)
	}
}

// Fresh R3 counterexample: the file symlink's target, not argv itself,
// contains a directory alias. The original spelling is still kernel input.
func TestDerivedGrantsLinuxSymlinkTargetAncestorAlias(t *testing.T) {
	for _, kind := range []string{"env", "shebang"} {
		t.Run(kind, func(t *testing.T) {
			home := stableHome(t)
			realDir := filepath.Join(home, "versions", "v1", "bin")
			review446Script(t, filepath.Join(realDir, "tool"))
			current := filepath.Join(home, "current")
			if err := os.Symlink("versions/v1", current); err != nil {
				t.Fatal(err)
			}
			shim := filepath.Join(home, "shims", "tool")
			if err := os.MkdirAll(filepath.Dir(shim), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../current/bin/tool", shim); err != nil {
				t.Fatal(err)
			}
			inner := []string{"/usr/bin/env", "FOO=bar", shim}
			if kind == "shebang" {
				outer := filepath.Join(home, "outer", "tool")
				review446Script(t, outer)
				if err := os.WriteFile(outer, []byte("#!"+shim+"\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				inner = []string{outer}
			}
			g := minimalDerivedGrants(t.TempDir(), home)
			g.ReadPaths = []string{"/bin", "/usr"}
			planBinaryNamespace(t, g, inner)
			argv, err := BuildBwrapArgv(g, inner)
			if err != nil {
				t.Fatal(err)
			}
			mounts, links := namespaceFromBwrap(argv)
			t.Logf("names=%v; mounts=%v; links=%v; argv=%v", g.derivedReadDirs, g.derivedReadPaths, links, argv)
			view := derivedGrantFixtureView(t, home, argv)
			if _, err := os.Stat(filepath.Join(view, "shims", "tool")); err != nil {
				t.Errorf("independent actual filesystem lookup of emitted fixture view: %v", err)
			}
			if _, ok := sandboxResolveWithLinks(shim, mounts, links); !ok {
				t.Fatalf("%s still needs %s -> ../current/bin/tool, but current/bin has no namespace representation", kind, shim)
			}
		})
	}
}
func TestDerivedGrantsLinuxExplicitRootMountCoverage(t *testing.T) {
	for _, mode := range []string{"read", "write", "allow"} {
		t.Run(mode, func(t *testing.T) {
			home := stableHome(t)
			tool := filepath.Join(home, "custom-runtime", "bin", "tool")
			review446Script(t, tool)
			g := minimalDerivedGrants(t.TempDir(), home)
			g.ReadPaths = []string{"/bin", "/usr"}
			switch mode {
			case "read":
				g.ReadPaths = append(g.ReadPaths, "/")
			case "write":
				g.WritePaths = []string{"/"}
			case "allow":
				g.AllowPaths = append(g.AllowPaths, "/")
			}
			planBinaryNamespace(t, g, []string{tool})
			argv, err := BuildBwrapArgv(g, []string{tool})
			t.Logf("derived=%v; argv=%v; err=%v", g.derivedReadPaths, argv, err)
			if err != nil {
				t.Fatalf("explicit root %s must not make a previously derivable command unlaunchable: %v", mode, err)
			}
		})
	}
}

// Independently reconstruct this fixture's emitted bind contents and links.
// This uses real filesystem lookup, not sandboxResolve's namespace model.
// It does not emulate or claim to execute bubblewrap.
func derivedGrantFixtureView(t *testing.T, home string, argv []string) string {
	t.Helper()
	view := t.TempDir()
	inHome := func(p string) bool { return strings.HasPrefix(p, home+"/") }
	mapPath := func(p string) string {
		if inHome(p) {
			return filepath.Join(view, strings.TrimPrefix(p, home+"/"))
		}
		return p
	}
	for i := 0; i < len(argv) && argv[i] != "--"; i++ {
		switch argv[i] {
		case "--bind", "--ro-bind":
			src, dest := argv[i+1], argv[i+2]
			i += 2
			if !inHome(dest) {
				continue
			}
			if err := os.MkdirAll(mapPath(dest), 0o755); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(src)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				s, d := filepath.Join(src, entry.Name()), filepath.Join(mapPath(dest), entry.Name())
				if entry.Type()&os.ModeSymlink != 0 {
					target, err := os.Readlink(s)
					if err != nil {
						t.Fatal(err)
					}
					err = os.Symlink(mapPath(target), d)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					data, err := os.ReadFile(s)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(d, data, 0o755); err != nil {
						t.Fatal(err)
					}
				}
			}
		case "--symlink":
			target, dest := argv[i+1], argv[i+2]
			i += 2
			if !inHome(dest) {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(mapPath(dest)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(mapPath(target), mapPath(dest)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return view
}
