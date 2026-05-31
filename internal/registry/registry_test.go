package registry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	// .opencode will be created lazily by Save.
	r := &Registry{}
	r.Upsert(Entry{
		Name:                "slack",
		SkillDir:            ".opencode/skills/slack",
		BundleHash:          "sha256:abc",
		RegisteredAt:        time.Unix(1700000000, 0).UTC(),
		DeclaredSecretNames: []string{"SLACK_BOT_TOKEN"},
	})
	if err := Save(dir, r); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded.Registered) != 1 || loaded.Registered[0].Name != "slack" {
		t.Fatalf("unexpected registry: %+v", loaded)
	}
	if loaded.Version != SchemaVersion {
		t.Fatalf("version = %d, want %d", loaded.Version, SchemaVersion)
	}
	if _, err := os.Stat(filepath.Join(dir, ".opencode", "sidecar.json")); err != nil {
		t.Fatalf("registry file missing: %v", err)
	}
}

func TestRemove(t *testing.T) {
	r := &Registry{}
	r.Upsert(Entry{Name: "a"})
	r.Upsert(Entry{Name: "b"})
	if !r.Remove("a") {
		t.Fatal("expected remove true")
	}
	if r.Remove("a") {
		t.Fatal("expected second remove false")
	}
	if len(r.Registered) != 1 || r.Registered[0].Name != "b" {
		t.Fatalf("unexpected after remove: %+v", r)
	}
}

func TestWithLock(t *testing.T) {
	dir := t.TempDir()
	if err := WithLock(dir, func() error { return nil }); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := os.Stat(LockPath(dir)); err != nil {
		t.Fatalf("lock file missing: %v", err)
	}
}

// TestGlobalRoundTrip verifies the user-global registry writes to
// $XDG_CONFIG_HOME/omac/sidecar.json and round-trips independently of
// any workdir.
func TestGlobalRoundTrip(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)

	want := filepath.Join(xdg, "omac", "sidecar.json")
	if got := GlobalPath(); got != want {
		t.Fatalf("GlobalPath() = %q, want %q", got, want)
	}

	r := &Registry{}
	r.Upsert(Entry{Name: "tng-email", SkillDir: "/abs/skills/tng-email", BundleHash: "sha256:xyz"})
	if err := WithGlobalLock(func() error { return SaveGlobal(r) }); err != nil {
		t.Fatalf("SaveGlobal: %v", err)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("global registry file missing: %v", err)
	}
	loaded, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if len(loaded.Registered) != 1 || loaded.Registered[0].Name != "tng-email" {
		t.Fatalf("unexpected global registry: %+v", loaded)
	}
}

// TestGlobalDirXDGPrecedence confirms XDG_CONFIG_HOME wins over HOME.
func TestGlobalDirXDGPrecedence(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir())
	if got, want := GlobalDir(), filepath.Join(xdg, "omac"); got != want {
		t.Fatalf("GlobalDir() = %q, want %q", got, want)
	}
}

// TestLoadGlobalMissingIsEmpty confirms a missing global file yields an
// empty registry rather than an error, so callers can always merge it.
func TestLoadGlobalMissingIsEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	r, err := LoadGlobal()
	if err != nil {
		t.Fatalf("LoadGlobal on empty: %v", err)
	}
	if len(r.Registered) != 0 {
		t.Fatalf("expected empty registry, got %+v", r)
	}
}
