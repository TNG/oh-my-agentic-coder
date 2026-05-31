package supervisor

import (
	"testing"
	"time"
)

// TestStopSidecarTracking verifies the bookkeeping of StopSidecar without
// spawning real processes: a Running with a nil Cmd.Process terminates as a
// no-op (terminate handles nil), so we can assert set membership directly.
func TestStopSidecarTracking(t *testing.T) {
	s := New(nil)
	s.children = []*Running{
		{Name: "a"},
		{Name: "b"},
		{Name: "c"},
	}

	if ok := s.StopSidecar("b", time.Second); !ok {
		t.Fatal("StopSidecar(b) returned false, want true")
	}
	if len(s.children) != 2 {
		t.Fatalf("after stop, len = %d, want 2", len(s.children))
	}
	for _, r := range s.children {
		if r.Name == "b" {
			t.Fatal("b still tracked after StopSidecar")
		}
	}

	// Stopping an unknown name is a no-op.
	if ok := s.StopSidecar("zzz", time.Second); ok {
		t.Fatal("StopSidecar(zzz) returned true, want false")
	}
	if len(s.children) != 2 {
		t.Fatalf("len changed on no-op stop: %d", len(s.children))
	}

	// Remaining order preserved (a, c).
	if s.children[0].Name != "a" || s.children[1].Name != "c" {
		t.Fatalf("order not preserved: %s, %s", s.children[0].Name, s.children[1].Name)
	}
}
