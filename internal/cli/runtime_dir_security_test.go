//go:build vuln

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Where a session keeps its runtime state.
//
// Each `omac start` creates a directory holding the bridge socket the harness
// talks to, plus sidecar pid files and logs. Its name was previously derived
// from the workdir hash, placed directly in shared /tmp, making it predictable
// and squattable. The fix uses os.MkdirTemp (random suffix) in the user's
// state directory, which is not shared with or writable by the sandbox.

// TestSecurityRuntimeDirIsNotPredictable asserts that two sessions on the same
// workdir do not land on the same, derivable runtime path.
func TestSecurityRuntimeDirIsNotPredictable(t *testing.T) {
	// Redirect runtime dir base to an isolated location.
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", t.TempDir())

	workdir := t.TempDir()

	first, err := createRuntimeDir(workdir)
	if err != nil {
		t.Fatalf("createRuntimeDir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(first) })

	// Control: the directory is real, private, and has the subdirectories a
	// session needs. Without it, a function that returned a fresh useless
	// path every time would satisfy the assertion below.
	fi, err := os.Stat(first)
	if err != nil || !fi.IsDir() {
		t.Fatalf("createRuntimeDir returned %s, which is not a directory (%v): the fixture is broken, not the security property", first, err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("runtime dir %s has mode %o, want 0700: the fixture is broken", first, perm)
	}
	for _, sub := range []string{"logs", "pids"} {
		if _, err := os.Stat(filepath.Join(first, sub)); err != nil {
			t.Fatalf("runtime dir is missing %s/ (%v): the fixture is broken", sub, err)
		}
	}

	second, err := createRuntimeDir(workdir)
	if err != nil {
		t.Fatalf("createRuntimeDir (second session): %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(second) })

	if first == second {
		t.Errorf("both sessions on this workdir got the same runtime directory %s: the path follows from the workdir alone, so the confined agent can place whatever it likes there before a session starts, including at the path the bridge socket will take", first)
	}
}

// TestSecurityRuntimeDirNotAdoptedFromForeignDir asserts that the runtime
// directory is never under shared /tmp (where a confined agent has write
// access), and that each session gets a fresh directory with only the
// expected subdirs — no pre-existing content.
func TestSecurityRuntimeDirNotAdoptedFromForeignDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", t.TempDir())
	workdir := t.TempDir()

	dir, err := createRuntimeDir(workdir)
	if err != nil {
		t.Fatalf("createRuntimeDir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	// The directory must not be a direct descendant of bare /tmp (the shared
	// system temp root, which the sandbox baseline may write-grant). A path
	// under the user's home (~/.local/state/...) is acceptable even if HOME
	// itself is redirected to a temp dir in tests.
	for _, sharedRoot := range []string{"/tmp", "/private/tmp"} {
		rel, relErr := filepath.Rel(sharedRoot, dir)
		if relErr == nil && len(rel) > 0 && rel[0] != '.' {
			// rel must contain at least two path elements: a private
			// sub-dir, then the actual runtime dir. A bare name (no slash)
			// means dir is a direct child of sharedRoot.
			if filepath.Dir(rel) == "." {
				t.Errorf("runtime dir %s is a direct child of shared temp root %s: "+
					"a confined agent write-granted that root can reach it", dir, sharedRoot)
			}
		}
	}

	// A fresh runtime dir must contain exactly logs/ and pids/ — no foreign files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("fresh runtime dir %s has %d entries (want 2: logs, pids): %v", dir, len(entries), entries)
	}
}
