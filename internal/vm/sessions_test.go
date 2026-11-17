package vm

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRegisterSessionWritesAndReleasesMarker(t *testing.T) {
	l := NewLayout(t.TempDir())
	release, err := RegisterSession(l, 4242)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.SessionsDir, "4242")); err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	release()
	if _, err := os.Stat(filepath.Join(l.SessionsDir, "4242")); !os.IsNotExist(err) {
		t.Errorf("marker still present after release")
	}
}

func TestAttachedSessionsPrunesDeadPids(t *testing.T) {
	l := NewLayout(t.TempDir())
	for _, pid := range []int{10, 20, 30} {
		if _, err := RegisterSession(l, pid); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(l.SessionsDir, "garbage"), nil, 0o600)
	alive := func(pid int) bool { return pid == 20 }
	got, err := AttachedSessions(l, alive)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int{20}) {
		t.Errorf("AttachedSessions = %v, want [20]", got)
	}
	entries, _ := os.ReadDir(l.SessionsDir)
	if len(entries) != 1 {
		t.Errorf("dead markers not pruned: %d entries", len(entries))
	}
}

func TestAttachedSessionsMissingDirIsEmpty(t *testing.T) {
	got, err := AttachedSessions(NewLayout(t.TempDir()), ProcessAlive)
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestProcessAliveSelf(t *testing.T) {
	if !ProcessAlive(os.Getpid()) {
		t.Error("ProcessAlive(self) = false")
	}
}
