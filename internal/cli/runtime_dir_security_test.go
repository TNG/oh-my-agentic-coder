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
// talks to, plus sidecar pid files and logs. Its name is the hex of a hash of
// the workdir, placed directly in the system temp directory — which on Linux
// is the shared, world-writable /tmp, and which the sandbox baseline grants
// the confined agent read-write.
//
// Naming it after the workdir makes it computable by anyone who knows the
// workdir, and the agent running in it certainly does. Whoever computes it
// first owns the path: the directory is created with MkdirAll, which accepts
// an entry that is already there and leaves its permissions alone, so a
// pre-placed directory is used as-is rather than rejected. What lands in it is
// the socket carrying every facade request of the session.
//
// The shared temp directory is the platform's, not omac's to change. Which
// name omac picks inside it is omac's, and a name nobody else can compute
// cannot be squatted before the session starts.

// TestSecurityRuntimeDirIsNotPredictable asserts that two sessions on the same
// workdir do not land on the same, derivable runtime path.
func TestSecurityRuntimeDirIsNotPredictable(t *testing.T) {
	// Redirect the system temp dir so nothing here touches the real one.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	workdir := t.TempDir()

	first, err := createRuntimeDir(workdir)
	if err != nil {
		t.Fatalf("createRuntimeDir: %v", err)
	}

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

	if first == second {
		t.Errorf("both sessions on this workdir got the same runtime directory %s: the path follows from the workdir alone, so the confined agent — which knows its own workdir and can write the shared temp directory — can place whatever it likes there before a session starts, including at the path the bridge socket will take", first)
	}
}
