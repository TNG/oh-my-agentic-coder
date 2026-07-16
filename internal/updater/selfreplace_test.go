package updater

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFsSelfReplacer_Replace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "omac")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed target: %v", err)
	}

	r := fsSelfReplacer{}
	if err := r.Replace(target, strings.NewReader("new"), 0o755); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(data) != "new" {
		t.Fatalf("content = %q, want new", data)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}

	// No leftover temp files.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly the target file, got %v", entries)
	}
}

func TestFsSelfReplacer_PermissionDenied(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root, permission checks don't apply")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	r := fsSelfReplacer{}
	err := r.Replace(filepath.Join(dir, "omac"), strings.NewReader("new"), 0o755)
	if err == nil {
		t.Fatalf("expected permission error")
	}
}
