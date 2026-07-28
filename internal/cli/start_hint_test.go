package cli

import (
	"testing"
	"time"
)

// TestBoundedKnownIDsNeverWaitsIndefinitely is the regression guard for the
// opencode session-list hang: the pre-exec session snapshot must never block
// the inner launch, no matter how slow (or stuck) the enumeration is. If
// someone reintroduces an unbounded wait on this path, this test hangs the
// package deadline instead of shipping.
func TestBoundedKnownIDsNeverWaitsIndefinitely(t *testing.T) {
	block := make(chan struct{})
	defer close(block) // release the never-returning goroutine when the test ends

	start := time.Now()
	got := boundedKnownIDs(func() map[string]struct{} {
		<-block // simulate an enumeration that never returns (the #145 hang)
		return map[string]struct{}{"unreachable": {}}
	}, 50*time.Millisecond)

	if got != nil {
		t.Errorf("want nil snapshot on timeout, got %v", got)
	}
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("boundedKnownIDs waited %v — it must not wait indefinitely", waited)
	}
}

// TestBoundedKnownIDsReturnsSnapshotWhenFast confirms the bound is transparent
// when enumeration completes in time: the snapshot is returned unchanged.
func TestBoundedKnownIDsReturnsSnapshotWhenFast(t *testing.T) {
	want := map[string]struct{}{"ses_a": {}, "ses_b": {}}
	got := boundedKnownIDs(func() map[string]struct{} { return want }, 2*time.Second)
	if len(got) != len(want) {
		t.Fatalf("want the enumerated snapshot %v, got %v", want, got)
	}
	for id := range want {
		if _, ok := got[id]; !ok {
			t.Errorf("snapshot dropped id %q", id)
		}
	}
}
