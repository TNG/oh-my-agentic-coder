package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeInstallPrefix builds a Unix install prefix at root: an executable at
// <root>/<bin>/<name> plus one file in each of subdirs.
func writeInstallPrefix(t *testing.T, root, bin, name string, subdirs ...string) string {
	t.Helper()
	binDir := filepath.Join(root, bin)
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(binDir, name)
	if err := os.WriteFile(exe, []byte("\x7fELF-not-really"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, sub := range subdirs {
		dir := filepath.Join(root, sub)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "support"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return exe
}

// A prefix-layout tool loads its runtime support from sibling directories of
// its bin dir, not from the bin dir itself: a JDK's bin/java needs
// lib/libjli.so to start at all and conf/security/java.security before it can
// open a TLS connection. Granting only the bin dir yields
// "error while loading shared libraries: libjli.so".
func TestResolveInnerBinaryDirsGrantsInstallPrefixSupportDirs(t *testing.T) {
	root := t.TempDir()
	exe := writeInstallPrefix(t, root, "bin", "java", "lib", "conf")

	got := resolveInnerBinaryDirs([]string{exe})

	for _, want := range []string{
		filepath.Join(root, "bin"),
		filepath.Join(root, "lib"),
		filepath.Join(root, "conf"),
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
}

// Support dirs that do not exist must not be emitted: bwrap --ro-bind aborts
// the launch on a missing source.
func TestResolveInnerBinaryDirsSkipsAbsentSupportDirs(t *testing.T) {
	root := t.TempDir()
	exe := writeInstallPrefix(t, root, "bin", "tool", "lib")

	got := resolveInnerBinaryDirs([]string{exe})

	if !slices.Contains(got, filepath.Join(root, "lib")) {
		t.Errorf("existing lib missing from %v", got)
	}
	for _, absent := range []string{"lib64", "libexec", "conf"} {
		if p := filepath.Join(root, absent); slices.Contains(got, p) {
			t.Errorf("absent %s granted: %v", p, got)
		}
	}
}

// A personal ~/bin is not a self-contained install tree, so its parent must
// not be treated as a prefix — that would widen the grant to ~/lib and
// friends off the back of any tool the user drops in ~/bin.
func TestResolveInnerBinaryDirsNeverTreatsHomeAsInstallPrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	exe := writeInstallPrefix(t, home, "bin", "tool", "lib", "conf")

	got := resolveInnerBinaryDirs([]string{exe})

	for _, forbidden := range []string{home, filepath.Join(home, "lib"), filepath.Join(home, "conf")} {
		if slices.Contains(got, forbidden) {
			t.Errorf("granted %s under $HOME: %v", forbidden, got)
		}
	}
	if !slices.Contains(got, filepath.Join(home, "bin")) {
		t.Errorf("the bin dir itself must still be granted: %v", got)
	}
}

// The prefix must never be the directory itself, only its support subdirs:
// granting <prefix> would expose every sibling tree, not just the runtime.
func TestResolveInnerBinaryDirsNeverGrantsPrefixRoot(t *testing.T) {
	root := t.TempDir()
	exe := writeInstallPrefix(t, root, "bin", "tool", "lib", "share")

	got := resolveInnerBinaryDirs([]string{exe})

	if slices.Contains(got, root) {
		t.Errorf("prefix root granted: %v", got)
	}
	if p := filepath.Join(root, "share"); slices.Contains(got, p) {
		t.Errorf("share is not a runtime support dir, granted anyway: %v", got)
	}
}

// Only conventional executable dirs identify a prefix. A binary sitting in an
// arbitrary directory has no install prefix to derive.
func TestResolveInnerBinaryDirsIgnoresNonBinParent(t *testing.T) {
	root := t.TempDir()
	exe := writeInstallPrefix(t, root, "opt", "tool", "lib")

	got := resolveInnerBinaryDirs([]string{exe})

	if p := filepath.Join(root, "lib"); slices.Contains(got, p) {
		t.Errorf("derived a prefix from a non-bin parent: %v", got)
	}
}

// A shebang interpreter can itself be prefix-layout (e.g. a version-managed
// node at <prefix>/bin/node), so it needs the same treatment.
func TestResolveInterpreterDirsGrantsInstallPrefixSupportDirs(t *testing.T) {
	root := t.TempDir()
	exe := writeInstallPrefix(t, root, "bin", "node", "lib")

	got := resolveInterpreterDirs(exe)

	if !slices.Contains(got, filepath.Join(root, "lib")) {
		t.Errorf("interpreter prefix lib missing from %v", got)
	}
}
