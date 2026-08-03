package stableport

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFor_Deterministic asserts the same canonical worktree
// path maps to the same port across calls (the core fix: the warm Gradle
// daemon's cached DOCKER_HOST stays valid across runs).
func TestFor_Deterministic(t *testing.T) {
	path := "/Users/x/repo/.worktrees/feat-a"
	first := For(path)
	for i := 0; i < 5; i++ {
		if got := For(path); got != first {
			t.Errorf("For not deterministic: %d then %d", first, got)
		}
	}
}

// TestFor_InRange asserts the port is in [30000,40000) — above
// common dev ports and below the macOS/Linux ephemeral range (49152–65535).
func TestFor_InRange(t *testing.T) {
	for _, p := range []string{
		"/Users/x/repo",
		"/home/y/repo/.worktrees/feat-b",
		"/Users/x/repo/.worktrees/feat-a",
		"/tmp/short",
	} {
		port := For(p)
		if port < StablePortMin || port >= StablePortMax {
			t.Errorf("For(%q) = %d, want in [%d,%d)", p, port, StablePortMin, StablePortMax)
		}
	}
}

// TestFor_DifferentPaths asserts distinct worktree paths yield
// distinct ports with high probability. A collision across a handful of
// distinct paths would indicate a broken hash; we assert a few distinct
// paths all differ.
func TestFor_DifferentPaths(t *testing.T) {
	paths := []string{
		"/Users/x/repo/.worktrees/feat-a",
		"/Users/x/repo/.worktrees/feat-b",
		"/Users/x/repo/.worktrees/feat-c",
		"/Users/x/other-repo",
		"/home/y/repo",
	}
	seen := map[int]string{}
	for _, p := range paths {
		port := For(p)
		if other, ok := seen[port]; ok {
			t.Errorf("port collision between %q and %q: both %d", other, p, port)
		}
		seen[port] = p
	}
}

// TestFor_CanonicalizesSymlinks asserts the hash is taken over
// the symlink-resolved path so a worktree reached via different symlink
// chains maps to the same port (the executor id and the port must agree
// on the canonical worktree).
func TestFor_CanonicalizesSymlinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if For(real) != For(link) {
		t.Errorf("For must canonicalize symlinks: real=%q link=%q differ", real, link)
	}
}

// TestSelect_PreferredFree asserts Select returns the preferred
// port when it is free.
func TestSelect_PreferredFree(t *testing.T) {
	isFree := func(int) bool { return true }
	got := Select(31000, isFree, func() int { t.Fatal("fallback must not run"); return 0 })
	if got != 31000 {
		t.Errorf("Select = %d, want 31000 (preferred free)", got)
	}
}

// TestSelect_PreferredBusyScans asserts Select scans the window
// forward when the preferred port is busy and returns the next free port.
func TestSelect_PreferredBusyScans(t *testing.T) {
	busy := map[int]bool{31000: true, 31001: true}
	isFree := func(p int) bool { return !busy[p] }
	got := Select(31000, isFree, func() int { t.Fatal("fallback must not run"); return 0 })
	if got != 31002 {
		t.Errorf("Select = %d, want 31002 (first free in window)", got)
	}
}

// TestSelect_WindowWraps asserts the scan wraps at StablePortMax back
// to StablePortMin so a preferred port near the top of the range still
// finds a free port near the bottom when the top is occupied.
func TestSelect_WindowWraps(t *testing.T) {
	// Preferred at StablePortMax-1; occupy it + the wrap target so the
	// scan lands two past the wrap.
	busy := map[int]bool{StablePortMax - 1: true, StablePortMin: true}
	isFree := func(p int) bool { return !busy[p] }
	got := Select(StablePortMax-1, isFree, func() int { t.Fatal("fallback must not run"); return 0 })
	if got != StablePortMin+1 {
		t.Errorf("Select = %d, want %d (wrap)", got, StablePortMin+1)
	}
}

// TestSelect_FallbackWhenWindowFull asserts Select calls the
// fallback when the whole window is occupied, so the build never wedges
// on a fully-occupied stable range (correctness over determinism).
// --- ReadPreferred edge cases (ticket 04) --------------------------------
//
// ReadPreferred reads the assigned port from a control-state file and
// returns 0 when the file is absent, unreadable, empty, garbled, or
// contains an out-of-range port. The existing tests only exercise the
// happy path (credproxy integration tests writing + reading valid ports).
// These unit tests lock down every error path.

// writePortFile is a helper that creates .omac-control under leaf and writes
// the named port file with the given content.
func writePortFile(t *testing.T, leaf, name, content string) {
	t.Helper()
	dir := filepath.Join(leaf, PortFileDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PortFilePath(leaf, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadPreferred_EmptyFile asserts an empty control file returns 0.
func TestReadPreferred_EmptyFile(t *testing.T) {
	leaf := t.TempDir()
	writePortFile(t, leaf, "test-port", "")
	if got := ReadPreferred(leaf, "test-port"); got != 0 {
		t.Errorf("ReadPreferred(empty file) = %d, want 0", got)
	}
}

// TestReadPreferred_GarbageFile asserts a non-numeric control file returns 0.
func TestReadPreferred_GarbageFile(t *testing.T) {
	leaf := t.TempDir()
	writePortFile(t, leaf, "test-port", "hello world")
	if got := ReadPreferred(leaf, "test-port"); got != 0 {
		t.Errorf("ReadPreferred(garbage) = %d, want 0", got)
	}
}

// TestReadPreferred_OutOfRangeLow asserts a port below StablePortMin returns 0.
func TestReadPreferred_OutOfRangeLow(t *testing.T) {
	leaf := t.TempDir()
	writePortFile(t, leaf, "test-port", "0")
	if got := ReadPreferred(leaf, "test-port"); got != 0 {
		t.Errorf("ReadPreferred(0) = %d, want 0 (below range)", got)
	}
}

// TestReadPreferred_OutOfRangeHigh asserts a port at StablePortMax (exclusive
// bound) returns 0.
func TestReadPreferred_OutOfRangeHigh(t *testing.T) {
	leaf := t.TempDir()
	writePortFile(t, leaf, "test-port", "40000")
	if got := ReadPreferred(leaf, "test-port"); got != 0 {
		t.Errorf("ReadPreferred(40000) = %d, want 0 (at exclusive bound)", got)
	}
}

// TestReadPreferred_MissingFile asserts that a non-existent control file
// returns 0 (the happy-path fallback when no port was previously persisted).
func TestReadPreferred_MissingFile(t *testing.T) {
	leaf := t.TempDir()
	if got := ReadPreferred(leaf, "test-port"); got != 0 {
		t.Errorf("ReadPreferred(missing) = %d, want 0", got)
	}
}

func TestSelect_FallbackWhenWindowFull(t *testing.T) {
	isFree := func(int) bool { return false }
	called := false
	fb := func() int { called = true; return 35000 }
	got := Select(31000, isFree, fb)
	if !called {
		t.Fatal("fallback must run when the whole window is occupied")
	}
	if got != 35000 {
		t.Errorf("Select = %d, want fallback 35000", got)
	}
}
