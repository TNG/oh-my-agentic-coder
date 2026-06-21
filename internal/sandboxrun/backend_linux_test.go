//go:build linux

package sandboxrun

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveInnerBinaryDir(t *testing.T) {
	// Empty / blank argv resolves to nothing.
	if got := resolveInnerBinaryDir(nil); got != "" {
		t.Errorf("nil argv = %q, want empty", got)
	}
	if got := resolveInnerBinaryDir([]string{""}); got != "" {
		t.Errorf("blank argv[0] = %q, want empty", got)
	}

	// A name not on PATH resolves to nothing rather than guessing.
	if got := resolveInnerBinaryDir([]string{"definitely-not-a-real-binary-xyz"}); got != "" {
		t.Errorf("missing binary = %q, want empty", got)
	}

	// A real binary on a version-manager-style path is resolved to its
	// containing directory, following symlinks (mimicking a mise shim
	// -> installs/<ver>/bin/<bin> layout).
	root := t.TempDir()
	realDir := filepath.Join(root, "installs", "opencode", "1.2.3", "bin")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realBin := filepath.Join(realDir, "opencode")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	shimDir := filepath.Join(root, "shims")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(shimDir, "opencode")
	if err := os.Symlink(realBin, shim); err != nil {
		t.Fatal(err)
	}

	// Resolve via an absolute path to the shim: must follow the symlink
	// to the real install dir, not return the shim dir.
	if got := resolveInnerBinaryDir([]string{shim}); got != realDir {
		t.Errorf("symlinked binary dir = %q, want %q", got, realDir)
	}

	// Resolve via PATH lookup (the `which opencode` case).
	t.Setenv("PATH", shimDir)
	if got := resolveInnerBinaryDir([]string{"opencode"}); got != realDir {
		t.Errorf("PATH-resolved binary dir = %q, want %q", got, realDir)
	}
}
