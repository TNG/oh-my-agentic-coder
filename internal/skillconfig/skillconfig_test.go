package skillconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGlobalRoundTrip verifies the user-global store writes to
// $XDG_CONFIG_HOME/omac/skill-config.yaml and round-trips independently
// of any workdir.
func TestGlobalRoundTrip(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	want := filepath.Join(xdg, "omac", "skill-config.yaml")
	if got := GlobalPath(); got != want {
		t.Fatalf("GlobalPath() = %q, want %q", got, want)
	}

	s, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal (missing): %v", err)
	}
	s.Set("tng-email", "IMAP_HOST", "imap.example.com")
	if err := SaveGlobal(s); err != nil {
		t.Fatalf("SaveGlobal: %v", err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("global skill-config file missing: %v", err)
	}
	loaded, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if v, ok := loaded.Get("tng-email", "IMAP_HOST"); !ok || v != "imap.example.com" {
		t.Fatalf("round-trip mismatch: got %q ok=%v", v, ok)
	}
}

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

// TestStore_FileIsYAML asserts that Save produces a YAML document
// (not JSON or some other shape). We don't pin the exact byte layout
// — yaml.v3 may legitimately reformat between minor releases — but we
// do assert that the file:
//   - has the canonical .yaml extension via Path()
//   - contains the human-friendly `key: value` form rather than JSON's
//     `"key": "value"` (which would slip through round-trip because
//     YAML is a JSON superset)
func TestStore_FileIsYAML(t *testing.T) {
	dir := t.TempDir()
	if filepath.Ext(Path(dir)) != ".yaml" {
		t.Fatalf("Path %q must end in .yaml", Path(dir))
	}
	s := &Store{}
	s.Set("echo-rest", "ECHO_GREETING", "hello")
	if err := Save(dir, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got := string(raw)
	// YAML wire form: bare keys, no surrounding {}.
	if strings.Contains(got, `"echo-rest": {`) || strings.HasPrefix(got, "{") {
		t.Errorf("Save produced JSON-shaped output, want YAML.\n%s", got)
	}
	if !strings.Contains(got, "echo-rest:") || !strings.Contains(got, "ECHO_GREETING: hello") {
		t.Errorf("Save output does not look like YAML.\n%s", got)
	}
}

// TestStore_BadYAML covers the malformed-input path. The bytes below
// are deliberately invalid YAML (mismatched flow mapping) so the
// parser raises a syntax error rather than producing an unexpected
// (but valid) document.
func TestStore_BadYAML(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(dir), []byte("{ unterminated: [flow,"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "parse skill-config") {
		t.Fatalf("Load: expected parse error, got %v", err)
	}
}

func TestDefaultsRoundTrip(t *testing.T) {
	s := &Store{Version: SchemaVersion, Skills: map[string]map[string]string{}}
	// Simulate first register: store a value AND mirror to defaults.
	s.Set("slack", "REGION", "eu")
	s.SetDefault("slack", "REGION", "eu")

	v, ok := s.GetDefault("slack", "REGION")
	if !ok || v != "eu" {
		t.Fatalf("GetDefault = %q,%v want eu,true", v, ok)
	}
	// Backfill case: a value present in Skills but not Defaults gets mirrored.
	s2 := &Store{Version: SchemaVersion, Skills: map[string]map[string]string{"x": {"F": "1"}}}
	if _, ok := s2.GetDefault("x", "F"); ok {
		t.Fatal("precondition: no default yet")
	}
	s2.SetDefault("x", "F", "1")
	if v, ok := s2.GetDefault("x", "F"); !ok || v != "1" {
		t.Errorf("backfilled default = %q,%v", v, ok)
	}
}
