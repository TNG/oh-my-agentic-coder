package buildrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeWrapper creates an executable regular file at <dir>/gradlew.
func makeWrapper(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "gradlew")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestResolveGradle(t *testing.T) {
	wt := t.TempDir()
	canonical, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(wt, "backend")
	makeWrapper(t, backend)

	t.Run("root inside worktree resolves wrapper", func(t *testing.T) {
		req, err := Resolve(wt, Request{Root: "backend", Args: []string{":help"}})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		wantWrapper := filepath.Join(canonical, "backend", "gradlew")
		if req.Wrapper != wantWrapper {
			t.Errorf("Wrapper = %q, want %q", req.Wrapper, wantWrapper)
		}
		wantProj := filepath.Join(canonical, "backend")
		if req.ProjectDir != wantProj {
			t.Errorf("ProjectDir = %q, want %q", req.ProjectDir, wantProj)
		}
		if req.Worktree != canonical {
			t.Errorf("Worktree = %q, want %q", req.Worktree, canonical)
		}
	})

	t.Run("dot root resolves worktree wrapper", func(t *testing.T) {
		wt2 := t.TempDir()
		makeWrapper(t, wt2)
		req, err := Resolve(wt2, Request{Root: ".", Args: []string{":help"}})
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		c2, _ := filepath.EvalSymlinks(wt2)
		if req.Wrapper != filepath.Join(c2, "gradlew") {
			t.Errorf("Wrapper = %q", req.Wrapper)
		}
	})

	t.Run("traversal escapes rejected", func(t *testing.T) {
		// ../ resolves above the worktree root.
		_, err := Resolve(wt, Request{Root: "../backend", Args: nil})
		if err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("err = %v, want outside-worktree rejection", err)
		}
		// Deep traversal that lands back inside is allowed
		// (canonical containment check, not textual).
		inner := filepath.Join(wt, "a", "b")
		makeWrapper(t, inner)
		if _, err := Resolve(wt, Request{Root: "a/b/../b", Args: nil}); err != nil {
			t.Errorf("in-worktree traversal should be allowed, got: %v", err)
		}
	})

	t.Run("absolute root outside worktree rejected", func(t *testing.T) {
		_, err := Resolve(wt, Request{Root: t.TempDir(), Args: nil})
		if err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("err = %v, want outside-worktree rejection", err)
		}
	})

	t.Run("symlink root escape rejected", func(t *testing.T) {
		outside := t.TempDir()
		makeWrapper(t, outside)
		link := filepath.Join(wt, "evil-link")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve(wt, Request{Root: "evil-link", Args: nil})
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("err = %v, want symlink-escape rejection", err)
		}
	})

	t.Run("symlink wrapper pointing outside rejected", func(t *testing.T) {
		outside := t.TempDir()
		target := makeWrapper(t, outside)
		realRoot := filepath.Join(wt, "realroot")
		if err := os.MkdirAll(realRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(realRoot, "gradlew")); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve(wt, Request{Root: "realroot", Args: nil})
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("err = %v, want wrapper symlink-escape rejection", err)
		}
	})

	t.Run("missing wrapper rejected", func(t *testing.T) {
		empty := filepath.Join(wt, "empty")
		if err := os.MkdirAll(empty, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve(wt, Request{Root: "empty", Args: nil})
		if err == nil || !strings.Contains(err.Error(), "gradlew") {
			t.Errorf("err = %v, want missing-gradlew rejection", err)
		}
	})

	t.Run("non-executable wrapper rejected", func(t *testing.T) {
		root := filepath.Join(wt, "noexec")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "gradlew"), []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve(wt, Request{Root: "noexec", Args: nil})
		if err == nil || !strings.Contains(err.Error(), "executable") {
			t.Errorf("err = %v, want not-executable rejection", err)
		}
	})

	t.Run("wrapper that is a directory rejected", func(t *testing.T) {
		root := filepath.Join(wt, "dirwrap")
		if err := os.MkdirAll(filepath.Join(root, "gradlew"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := Resolve(wt, Request{Root: "dirwrap", Args: nil})
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Errorf("err = %v, want not-regular-file rejection", err)
		}
	})
}
