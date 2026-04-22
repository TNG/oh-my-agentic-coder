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
		MetaHash:            "sha256:abc",
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
