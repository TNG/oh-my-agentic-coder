package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writeHomebrewChain builds the layout a Homebrew-installed harness has under
// a non-default prefix, where no baseline grant covers the tree:
//
//	<root>/bin/opencode
//	  -> <root>/Cellar/opencode/1.18.15/bin/opencode
//	  -> <root>/Cellar/opencode/1.18.15/libexec/lib/node_modules/opencode-ai/bin/opencode.exe
//
// Returns the PATH-entry path (the first link).
func writeHomebrewChain(t *testing.T, root string) string {
	t.Helper()
	cellar := filepath.Join(root, "Cellar", "opencode", "1.18.15")
	realDir := filepath.Join(cellar, "libexec", "lib", "node_modules", "opencode-ai", "bin")
	for _, d := range []string{filepath.Join(root, "bin"), filepath.Join(cellar, "bin"), realDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	real := filepath.Join(realDir, "opencode.exe")
	if err := os.WriteFile(real, []byte("\xcf\xfa\xed\xfe-not-really"), 0o755); err != nil {
		t.Fatal(err)
	}
	mid := filepath.Join(cellar, "bin", "opencode")
	if err := os.Symlink(real, mid); err != nil {
		t.Fatal(err)
	}
	head := filepath.Join(root, "bin", "opencode")
	if err := os.Symlink(mid, head); err != nil {
		t.Fatal(err)
	}
	return head
}

// The kernel resolves a symlink chain one hop at a time and needs read access
// to each link on the way, so granting only the two ENDS of the chain — the
// PATH-entry dir and the fully resolved dir — leaves the middle link
// unreadable and the launch dies there.
//
// Homebrew under a non-default prefix (issue #229) is the layout that exposes
// it: the middle hop sits in <cellar>/bin while the real file sits in
// <cellar>/libexec/..., so neither end's grant covers it. macOS then reports
// the failed in-sandbox lookup of a bare argv[0] as
// "execvp() of 'opencode' failed: No such file or directory" — an error that
// names neither the deny nor the path, on a machine where `which opencode`
// answers fine.
func TestResolveInnerBinaryDirsGrantsEverySymlinkHop(t *testing.T) {
	root := t.TempDir()
	head := writeHomebrewChain(t, root)

	got := resolveInnerBinaryDirs([]string{head})

	cellar := filepath.Join(root, "Cellar", "opencode", "1.18.15")
	for _, want := range []string{
		filepath.Join(root, "bin"),
		filepath.Join(cellar, "bin"), // the hop both ends' grants miss
		filepath.Join(cellar, "libexec", "lib", "node_modules", "opencode-ai", "bin"),
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing symlink-chain dir %s in %v", want, got)
		}
	}
}

// The same chain reached the way a user actually launches it: a bare command
// name found on PATH rather than an absolute path.
func TestResolveInnerBinaryDirsGrantsEverySymlinkHopViaPATH(t *testing.T) {
	root := t.TempDir()
	writeHomebrewChain(t, root)
	t.Setenv("PATH", filepath.Join(root, "bin"))

	got := resolveInnerBinaryDirs([]string{"opencode"})

	want := filepath.Join(root, "Cellar", "opencode", "1.18.15", "bin")
	if !slices.Contains(got, want) {
		t.Errorf("missing intermediate hop %s in %v", want, got)
	}
}

// A cyclic chain must terminate rather than spin: the walk is fed paths from
// the host filesystem, so a loop is a hostile-input case, not a theoretical
// one. The dirs seen before the cycle closes are still returned — they are
// legitimate grants regardless of how the chain ends.
func TestSymlinkChainDirsTerminatesOnCycle(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a", "link"), filepath.Join(root, "b", "link")
	for _, d := range []string{filepath.Join(root, "a"), filepath.Join(root, "b")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}

	got := symlinkChainDirs(a)

	for _, want := range []string{filepath.Join(root, "a"), filepath.Join(root, "b")} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %s in %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("cycle produced %d dirs, want the 2 distinct ones: %v", len(got), got)
	}
}

// A plain, unlinked binary must yield its own directory and nothing beyond
// the spellings of that one directory. The chain walk replaced the previous
// Dir()/EvalSymlinks() pair, so the no-symlink case has to stay as narrow as
// it was — the temp root sits under the /var → /private/var link on macOS, so
// both spellings of the same directory are expected and neither widens the
// grant.
func TestSymlinkChainDirsPlainBinary(t *testing.T) {
	root := t.TempDir()
	exe := filepath.Join(root, "tool")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	got := symlinkChainDirs(exe)

	if !slices.Contains(got, root) {
		t.Errorf("symlinkChainDirs(%s) = %v, missing %s", exe, got, root)
	}
	for _, d := range got {
		if d != root && d != canonical {
			t.Errorf("symlinkChainDirs(%s) granted unrelated dir %s: %v", exe, d, got)
		}
	}
}
