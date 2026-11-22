package approver

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A file cut short by a crash must not refuse every later answer: the next
// save moves it aside and starts over.
func TestCorruptStoreIsMovedAsideOnTheNextSave(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(`{"version":1,"verd`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(dir)
	if _, err := s.Has("x"); err == nil {
		t.Error("Has read a corrupt file without error")
	}
	if err := s.Add(Verdict{ID: "x", Skill: "k", Match: "GET /a"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if ok, err := s.Has("x"); !ok || err != nil {
		t.Errorf("Has after recovery = %v, %v", ok, err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, fileName+".corrupt")); err != nil || string(raw) != `{"version":1,"verd` {
		t.Errorf("corrupt file not kept: %q %v", raw, err)
	}
}

// Has caches the parsed file but sees a revoke by another process, which
// renames a new file into place.
func TestHasSeesAChangeByAnotherStore(t *testing.T) {
	dir := t.TempDir()
	server, cli := NewStore(dir), NewStore(dir)
	if err := cli.Add(Verdict{ID: "x", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := server.Has("x"); !ok {
		t.Fatal("stored verdict not found")
	}
	if ok, err := cli.Revoke("x"); !ok || err != nil {
		t.Fatalf("revoke: %v %v", ok, err)
	}
	if ok, _ := server.Has("x"); ok {
		t.Error("revoked verdict still found through the cache")
	}
	if err := cli.Add(Verdict{ID: "y"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := server.Has("y"); !ok {
		t.Error("new verdict not seen through the cache")
	}
}

func TestStoreRevokeWhere(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, v := range []Verdict{{ID: "1", Skill: "a"}, {ID: "2", Skill: "a"}, {ID: "3", Skill: "b"}} {
		if err := s.Add(v); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.RevokeWhere(func(v Verdict) bool { return v.Skill == "a" })
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if all, _ := s.List(); len(all) != 1 || all[0].ID != "3" {
		t.Fatalf("left: %v", all)
	}
	if n, err := s.RevokeWhere(func(Verdict) bool { return false }); n != 0 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
}
