package prefs

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestLoadMissingReturnsEmpty(t *testing.T) {
	isolate(t)
	p, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if p.SuppressPluginWarning {
		t.Error("fresh prefs should not suppress the plugin warning")
	}
	if p.Version != SchemaVersion {
		t.Errorf("version = %d, want %d", p.Version, SchemaVersion)
	}
}

func TestSaveThenLoadRoundTrip(t *testing.T) {
	isolate(t)
	if err := Save(&Store{SuppressPluginWarning: true}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// File should exist at the resolved path with 0600.
	p := Path()
	if p == "" {
		t.Fatal("Path() empty")
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.SuppressPluginWarning {
		t.Error("SuppressPluginWarning did not round-trip")
	}
}

func TestGlobalDirHonorsXDG(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if got, want := GlobalDir(), filepath.Join(xdg, "omac"); got != want {
		t.Errorf("GlobalDir = %q, want %q", got, want)
	}
}

func TestProtectedWatchIgnoreRoundTrip(t *testing.T) {
	isolate(t)
	store, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if store.IgnoresProtectedPath("/work/.env.local") {
		t.Fatal("fresh store must not ignore a path")
	}
	if err := store.AddProtectedWatchIgnore("/work/.env.local"); err != nil {
		t.Fatalf("AddProtectedWatchIgnore: %v", err)
	}
	// Idempotent.
	if err := store.AddProtectedWatchIgnore(filepath.Clean("/work/.env.local")); err != nil {
		t.Fatalf("second add: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !got.IgnoresProtectedPath("/work/.env.local") {
		t.Fatalf("path did not round-trip: %v", got.ProtectedWatchIgnore)
	}
	if len(got.ProtectedWatchIgnore) != 1 {
		t.Fatalf("exceptions = %v, want one", got.ProtectedWatchIgnore)
	}
}

func TestProtectedWatchProjectIgnoreRoundTrip(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	path := filepath.Join(root, ".worktrees", "a", ".env.local")
	store, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if store.IgnoresProtectedProjectPath(path) {
		t.Fatal("fresh store must not ignore a project path")
	}
	if err := store.AddProtectedWatchProjectIgnore(root, ".env.local"); err != nil {
		t.Fatalf("AddProtectedWatchProjectIgnore: %v", err)
	}
	// Idempotent.
	if err := store.AddProtectedWatchProjectIgnore(root, ".env.local"); err != nil {
		t.Fatalf("second add: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ProtectedWatchProjectIgnores) != 1 {
		t.Fatalf("project exceptions = %v, want one", got.ProtectedWatchProjectIgnores)
	}
	if !got.IgnoresProtectedProjectPath(path) {
		t.Fatalf("path did not round-trip: %v", got.ProtectedWatchProjectIgnores)
	}
	if got.IgnoresProtectedProjectPath(filepath.Join(root, "a", ".env")) {
		t.Fatal("a different basename must not match")
	}
	if got.IgnoresProtectedPath(path) {
		t.Fatal("a project exception must not become an exact-path exception")
	}
}

func TestProtectedWatchIgnoreBatchSavesAll(t *testing.T) {
	isolate(t)
	store, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 10)
	for i := range paths {
		paths[i] = filepath.Join("/work", fmt.Sprintf("%d/.env", i))
	}
	if err := store.AddProtectedWatchIgnores(paths); err != nil {
		t.Fatalf("AddProtectedWatchIgnores: %v", err)
	}
	if len(store.ProtectedWatchIgnore) != len(paths) {
		t.Fatalf("exceptions = %v, want %d", store.ProtectedWatchIgnore, len(paths))
	}
	// A second batch with the same paths is a no-op, not a duplicate append.
	if err := store.AddProtectedWatchIgnores(paths); err != nil {
		t.Fatalf("second AddProtectedWatchIgnores: %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ProtectedWatchIgnore) != len(paths) {
		t.Fatalf("persisted exceptions = %v, want %d", got.ProtectedWatchIgnore, len(paths))
	}
	for _, path := range paths {
		if !got.IgnoresProtectedPath(path) {
			t.Fatalf("path %q did not round-trip", path)
		}
	}
}

func TestProtectedWatchIgnoreSymlinkedSpelling(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	real := filepath.Join(base, "repo")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(real, ".env")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	store, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddProtectedWatchIgnore(filepath.Join(link, ".env")); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !got.IgnoresProtectedPath(secret) {
		t.Fatal("the canonical spelling must match a symlinked exception")
	}
	if !got.IgnoresProtectedPath(filepath.Join(link, ".env")) {
		t.Fatal("the symlinked spelling must match too")
	}
}

func TestProtectedWatchProjectIgnoreBoundary(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	store, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddProtectedWatchProjectIgnore(root, ".env.local"); err != nil {
		t.Fatal(err)
	}
	sibling := root + "-other"
	if store.IgnoresProtectedProjectPath(filepath.Join(sibling, ".env.local")) {
		t.Fatal("a sibling directory with the same prefix must keep the warning")
	}
}

func TestProtectedWatchProjectIgnoreSymlinkedRoot(t *testing.T) {
	isolate(t)
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "a", ".env.local")
	if err := os.WriteFile(path, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	store, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddProtectedWatchProjectIgnore(link, ".env.local"); err != nil {
		t.Fatal(err)
	}
	if !store.IgnoresProtectedProjectPath(filepath.Join(link, "a", ".env.local")) {
		t.Fatal("a symlinked worktree must match the canonical root")
	}
	if !store.IgnoresProtectedProjectPath(path) {
		t.Fatal("the canonical path must match too")
	}
}

func TestProtectedWatchProjectIgnoreRejectsBadName(t *testing.T) {
	isolate(t)
	store, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddProtectedWatchProjectIgnore(t.TempDir(), "sub/.env"); err == nil {
		t.Fatal("a name with a separator must be rejected")
	}
	if err := store.AddProtectedWatchProjectIgnore(t.TempDir(), ""); err == nil {
		t.Fatal("an empty name must be rejected")
	}
	for _, name := range []string{".", ".."} {
		if err := store.AddProtectedWatchProjectIgnore(t.TempDir(), name); err == nil {
			t.Fatalf("name %q must be rejected", name)
		}
	}
}
