package skillconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	// Load on a missing file returns an empty Store.
	s, err := Load(dir)
	if err != nil {
		t.Fatalf("Load (missing): %v", err)
	}
	if s.Version == 0 {
		t.Errorf("Load (missing): Version should be set, got 0")
	}
	if s.Skills == nil {
		t.Error("Load (missing): Skills map should be non-nil")
	}

	// Set + Save.
	s.Set("echo-rest", "ECHO_GREETING", "hello")
	s.Set("echo-rest", "ECHO_MAX_TICK", "100")
	s.Set("slack", "SLACK_DEFAULT_CHANNEL", "#general")
	if err := Save(dir, s); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// File must exist with mode 0600.
	info, err := os.Stat(Path(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %o, want 0600", mode)
	}

	// Reload + verify.
	s2, err := Load(dir)
	if err != nil {
		t.Fatalf("Load (after save): %v", err)
	}
	for _, c := range []struct{ skill, field, want string }{
		{"echo-rest", "ECHO_GREETING", "hello"},
		{"echo-rest", "ECHO_MAX_TICK", "100"},
		{"slack", "SLACK_DEFAULT_CHANNEL", "#general"},
	} {
		got, ok := s2.Get(c.skill, c.field)
		if !ok {
			t.Errorf("Get(%q,%q): not present", c.skill, c.field)
			continue
		}
		if got != c.want {
			t.Errorf("Get(%q,%q) = %q, want %q", c.skill, c.field, got, c.want)
		}
	}
}

func TestStore_GetMissing(t *testing.T) {
	s := &Store{}
	if v, ok := s.Get("nope", "neither"); ok || v != "" {
		t.Errorf("Get on empty store: got (%q,%v), want (\"\",false)", v, ok)
	}
}

func TestStore_UnsetCollapsesSkill(t *testing.T) {
	s := &Store{}
	s.Set("a", "X", "1")
	if !s.Unset("a", "X") {
		t.Fatal("Unset on present field should return true")
	}
	if _, ok := s.Skills["a"]; ok {
		t.Errorf("Skills[\"a\"] should be removed after its last field")
	}
	if s.Unset("a", "X") {
		t.Error("Unset on already-removed field should return false")
	}
}

func TestStore_RemoveSkill(t *testing.T) {
	s := &Store{}
	s.Set("a", "X", "1")
	s.Set("a", "Y", "2")
	if !s.RemoveSkill("a") {
		t.Error("RemoveSkill on present skill should return true")
	}
	if s.RemoveSkill("a") {
		t.Error("RemoveSkill on absent skill should return false")
	}
}

func TestStore_FieldsForSorted(t *testing.T) {
	s := &Store{}
	s.Set("a", "ZULU", "z")
	s.Set("a", "ALPHA", "a")
	s.Set("a", "MIKE", "m")
	got := s.FieldsFor("a")
	want := []string{"ALPHA", "MIKE", "ZULU"}
	if len(got) != len(want) {
		t.Fatalf("FieldsFor: got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("FieldsFor[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if s.FieldsFor("nope") != nil {
		t.Error("FieldsFor on unknown skill should return nil")
	}
}

func TestStore_SaveCreatesDotOpencodeDir(t *testing.T) {
	dir := t.TempDir()
	// Pre-condition: .opencode/ does not exist yet.
	if _, err := os.Stat(filepath.Join(dir, ".opencode")); !os.IsNotExist(err) {
		t.Fatalf(".opencode pre-exists: %v", err)
	}
	s := &Store{}
	s.Set("x", "K", "v")
	if err := Save(dir, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, ".opencode"))
	if err != nil {
		t.Fatalf(".opencode not created: %v", err)
	}
	if !info.IsDir() {
		t.Error(".opencode should be a directory")
	}
}

func TestStore_BadJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "parse skill-config") {
		t.Fatalf("Load: expected parse error, got %v", err)
	}
}
