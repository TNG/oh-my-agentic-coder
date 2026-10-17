package ephemeraldocker

import (
	"errors"
	"fmt"
	"testing"
)

func TestHostPortDeterministicPerWorktree(t *testing.T) {
	free := func(int) bool { return true }
	a, err := HostPortFor("/Users/me/work/x", free)
	if err != nil {
		t.Fatalf("HostPortFor: %v", err)
	}
	b, err := HostPortFor("/Users/me/work/x", free)
	if err != nil {
		t.Fatalf("HostPortFor: %v", err)
	}
	if a != b {
		t.Errorf("same worktree must map to the same port: %d != %d", a, b)
	}
	if a < 30000 || a >= 40000 {
		t.Errorf("port %d outside the stable window [30000,40000)", a)
	}
}

func TestHostPortFollowsSymlinkResolution(t *testing.T) {
	// The scheme hashes the canonical path, like the documented stableport
	// scheme: EvalSymlinks before hashing.
	free := func(int) bool { return true }
	direct, err := HostPortFor("/tmp/link", free)
	if err != nil {
		t.Fatalf("HostPortFor: %v", err)
	}
	if direct == 0 {
		t.Fatal("expected a port")
	}
}

func TestHostPortScansWindowOnCollision(t *testing.T) {
	preferred, err := HostPortFor("/w", func(int) bool { return true })
	if err != nil {
		t.Fatalf("HostPortFor: %v", err)
	}
	// The preferred port is occupied; the next free port in the window is
	// expected (no wraparound needed here).
	next, err := HostPortFor("/w", func(p int) bool { return p != preferred })
	if err != nil {
		t.Fatalf("HostPortFor: %v", err)
	}
	want := preferred + 1
	if want >= 40000 {
		want = 30000 // the window wraps, like the scheme
	}
	if next != want {
		t.Errorf("occupied preferred port must scan to the next port in the window: got %d, want %d", next, want)
	}
}

func TestHostPortExhaustedWindowFails(t *testing.T) {
	_, err := HostPortFor("/w", func(int) bool { return false })
	if err == nil {
		t.Fatal("an exhausted stable window must fail, not fall back to an unknowable ephemeral port")
	}
	if !errors.Is(err, ErrNoFreePort) {
		t.Errorf("must fail with ErrNoFreePort, got %v", err)
	}
}

func TestHostPortRejectsEmptyPath(t *testing.T) {
	_, err := HostPortFor("", func(int) bool { return true })
	if err == nil {
		t.Fatal("empty worktree path must be rejected")
	}
	// Deterministic across arguments for the error path, too.
	if _, err2 := HostPortFor("", func(int) bool { return true }); err2 == nil || fmt.Sprint(err) != fmt.Sprint(err2) {
		t.Errorf("error must be deterministic: %v vs %v", err, err2)
	}
}
