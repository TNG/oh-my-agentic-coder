package stableport

import (
	"fmt"
	"net"
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
	isFree := func(int) error { return nil }
	got := Select(31000, isFree, func() int { t.Fatal("fallback must not run"); return 0 })
	if got != 31000 {
		t.Errorf("Select = %d, want 31000 (preferred free)", got)
	}
}

// TestSelect_PreferredBusyScans asserts Select scans the window
// forward when the preferred port is busy and returns the next free port.
func TestSelect_PreferredBusyScans(t *testing.T) {
	busy := map[int]bool{31000: true, 31001: true}
	isFree := func(p int) error {
		if busy[p] {
			return fmt.Errorf("port %d is busy", p)
		}
		return nil
	}
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
	isFree := func(p int) error {
		if busy[p] {
			return fmt.Errorf("port %d busy", p)
		}
		return nil
	}
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

// TestIsFree_ReturnsError asserts that IsFree returns a non-nil error
// when the port is already in use, so callers can log the bind failure
// reason (issue #191).
func TestIsFree_ReturnsError(t *testing.T) {
	occ, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occ.Close()
	busyPort := occ.Addr().(*net.TCPAddr).Port
	if err := IsFree(busyPort); err == nil {
		t.Errorf("IsFree(%d) = nil, want non-nil error (port held)", busyPort)
	}
}

func TestSelect_FallbackWhenWindowFull(t *testing.T) {
	isFree := func(int) error { return fmt.Errorf("port busy") }
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

// --- Choose: the shared stable-port selection policy ----------------------
//
// Choose centralizes the (previously duplicated) proxy lifecycle: prefer
// the persisted control-file port, else the worktree hash port, scan the
// window when busy, and report WHY the preferred port could not be bound
// (issue #191) so the caller can log the real bind error (EADDRINUSE vs
// EPERM) instead of a bare warning. The isFree seam is injectable so the
// whole window can be simulated without binding real sockets.

// TestChoose_PersistedPreferred asserts Choose prefers the persisted
// control-file port over the fresh worktree hash (so the port stays stable
// across runs after the listener is torn down).
func TestChoose_PersistedPreferred(t *testing.T) {
	leaf := t.TempDir()
	if err := WritePreferred(leaf, "choose-port", 31000); err != nil {
		t.Fatal(err)
	}
	port, fallback := Choose("/worktree/feat-a", leaf, "choose-port", IsFree, RandomFree, nil)
	if port != 31000 {
		t.Errorf("Choose = %d, want persisted 31000 (file beats hash %d)", port, For("/worktree/feat-a"))
	}
	if fallback {
		t.Error("Choose: persisted preferred port must not be flagged as fallback")
	}
}

// TestChoose_HashWhenNoControlFile asserts Choose derives the stable port
// from the worktree hash when no control file exists (fresh worktree).
func TestChoose_HashWhenNoControlFile(t *testing.T) {
	want := For("/worktree/feat-a")
	port, fallback := Choose("/worktree/feat-a", t.TempDir(), "choose-port", IsFree, RandomFree, nil)
	if port != want {
		t.Errorf("Choose = %d, want hash port %d", port, want)
	}
	if fallback {
		t.Error("Choose: hash-derived port must not be flagged as fallback")
	}
}

// TestChoose_ScannedNeighborNotFallbackAndPersisted asserts that when the
// preferred port is busy but a scan neighbor is free, the neighbor is
// returned with fallback=false (a scanned in-range port is NOT a fallback;
// the caller persists it — issue #191). The preferred port is forced to
// 31000 via the control file so the scan deterministically lands on 31002.
func TestChoose_ScannedNeighborNotFallbackAndPersisted(t *testing.T) {
	leaf := t.TempDir()
	if err := WritePreferred(leaf, "choose-port", 31000); err != nil {
		t.Fatal(err)
	}
	busy := map[int]bool{31000: true, 31001: true}
	isFree := func(p int) error {
		if busy[p] {
			return fmt.Errorf("port %d busy", p)
		}
		return nil
	}
	port, fallback := Choose("/worktree/feat-scan", leaf, "choose-port", isFree, func() int {
		t.Fatal("fallback must not run when a scan neighbor is free")
		return 0
	}, nil)
	if port != 31002 {
		t.Errorf("Choose = %d, want scan neighbor 31002", port)
	}
	if fallback {
		t.Error("Choose: scanned in-range neighbor must NOT be flagged as fallback")
	}
	// Caller-side outcome: fallback=false is the caller's persistence gate.
	// With a control leaf wired, the proxy persists the scanned neighbor
	// (the pure policy above does not write the file; the proxy does).
	if err := WritePreferred(leaf, "choose-port", port); err != nil {
		t.Fatal(err)
	}
	if got := ReadPreferred(leaf, "choose-port"); got != 31002 {
		t.Errorf("port file = %d, want scanned neighbor 31002 (scanned neighbor must be persisted)", got)
	}
}

// TestChoose_ReportsUnbindablePreferred asserts the reason WHY the
// preferred port could not be bound is reported through the onReason
// callback with the actual IsFree error (issue #191: EADDRINUSE vs EPERM
// vs sandbox-blocked). The preferred port alone being busy must report
// exactly once, for the preferred port, with the real error.
func TestChoose_ReportsUnbindablePreferred(t *testing.T) {
	preferred := For("/worktree/feat-reason")
	busyErr := fmt.Errorf("listen tcp 127.0.0.1:%d: bind: address already in use", preferred)
	reported := []int{}
	var reportedErr error
	onReason := func(port int, cause error) {
		reported = append(reported, port)
		reportedErr = cause
	}
	port, fallback := Choose("/worktree/feat-reason", "", "choose-port", func(p int) error {
		if p == preferred {
			return busyErr
		}
		return nil
	}, func() int { return 0 }, onReason)
	if port == 0 || port == preferred {
		t.Errorf("Choose = %d, want a scanned neighbor (preferred %d busy)", port, preferred)
	}
	if fallback {
		t.Error("Choose: scanned neighbor must not be flagged fallback")
	}
	if len(reported) != 1 || reported[0] != preferred {
		t.Errorf("reason reported for %v, want exactly [%d]", reported, preferred)
	}
	if reportedErr != busyErr {
		t.Errorf("reason error = %v, want the exact IsFree error %v", reportedErr, busyErr)
	}
}

// TestChoose_EphemeralFallbackIsFallback asserts a chosen==0 (window full
// AND random failed) is a TRUE fallback and reports the preferred-port
// bind failures — the caller must not persist it.
func TestChoose_EphemeralFallbackIsFallback(t *testing.T) {
	preferred := For("/worktree/feat-full")
	cause := fmt.Errorf("simulated EPERM on %d", preferred)
	port, fallback := Choose("/worktree/feat-full", "", "choose-port",
		func(int) error { return cause },
		func() int { return 0 },
		func(int, error) {})
	if port != 0 || !fallback {
		t.Errorf("Choose = %d, fallback %v; want 0,true (window full + random failed)", port, fallback)
	}
}

// TestChoose_OutOfRangeRandomIsFallback asserts a random fallback port
// OUTSIDE [StablePortMin, StablePortMax) is flagged fallback=true so the
// caller does NOT persist it (an ephemeral fallback must never poison the
// control file for the next run while a scanned neighbor always does).
func TestChoose_OutOfRangeRandomIsFallback(t *testing.T) {
	port, fallback := Choose("/worktree/feat-rand", "", "choose-port",
		func(int) error { return fmt.Errorf("port busy") },
		func() int { return 54321 }, nil)
	if port != 54321 {
		t.Errorf("Choose = %d, want the injected ephemeral 54321", port)
	}
	if !fallback {
		t.Error("Choose: out-of-range random port must be flagged fallback=true")
	}
}

// TestChoose_Wraps asserts the scan wraps at StablePortMax back to
// StablePortMin when the preferred port sits at the top of the range
// (identical wrap semantics as Select). The preferred port is forced to
// the top of the window by pre-seeding the control file: ReadPreferred
// accepts any in-range port, so the seeded 39999 makes the scan start
// there and wrap down through the bottom of the range.
func TestChoose_Wraps(t *testing.T) {
	leaf := t.TempDir()
	if err := WritePreferred(leaf, "choose-port", StablePortMax-1); err != nil {
		t.Fatal(err)
	}
	busy := map[int]bool{StablePortMax - 1: true, StablePortMin: true}
	isFree := func(p int) error {
		if busy[p] {
			return fmt.Errorf("port %d busy", p)
		}
		return nil
	}
	port, fallback := Choose("/worktree/feat-wrap", leaf, "choose-port", isFree, func() int {
		t.Fatal("fallback must not run")
		return 0
	}, nil)
	if port != StablePortMin+1 {
		t.Errorf("Choose = %d, want %d (wrap to bottom of range)", port, StablePortMin+1)
	}
	if fallback {
		t.Error("Choose: wrapped scanned neighbor must not be flagged fallback")
	}
}

// TestChoose_LegacyEmptyWorktreePath asserts the legacy random-port path:
// an empty worktree path returns 0, not-fallback, without touching the
// control file (the documented v1 behavior for callers that did not wire
// the worktree).
func TestChoose_LegacyEmptyWorktreePath(t *testing.T) {
	leaf := t.TempDir()
	if err := WritePreferred(leaf, "choose-port", 31000); err != nil {
		t.Fatal(err)
	}
	port, fallback := Choose("", leaf, "choose-port",
		func(int) error { t.Fatal("isFree must not be consulted"); return nil },
		func() int { t.Fatal("random fallback must not be consulted"); return 0 }, nil)
	if port != 0 || fallback {
		t.Errorf("Choose = %d, fallback %v; want 0,false (legacy random-port path)", port, fallback)
	}
	if got := ReadPreferred(leaf, "choose-port"); got != 31000 {
		t.Errorf("legacy path must not touch the control file: ReadPreferred = %d, want 31000", got)
	}
}
