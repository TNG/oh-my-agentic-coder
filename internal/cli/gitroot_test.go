package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func testGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "user.name=omac test", "-c", "user.email=omac@example.invalid", "-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	testGit(t, dir, "init", "-q")
}

func commitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("tracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "add", "tracked.txt")
	testGit(t, dir, "commit", "-q", "-m", "initial")
}

func TestResolveGitProjectRootNormalCheckout(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	got, ok := resolveGitProjectRoot(root)
	if !ok || got != canonicalPath(root) {
		t.Fatalf("resolveGitProjectRoot = %q, %v; want %q, true", got, ok, canonicalPath(root))
	}
}

func TestResolveGitProjectRootLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	commitRepo(t, root)
	worktree := filepath.Join(root, ".worktrees", "linked")
	testGit(t, root, "worktree", "add", "-q", worktree)
	got, ok := resolveGitProjectRoot(worktree)
	if !ok || got != canonicalPath(root) {
		t.Fatalf("linked worktree root = %q, %v; want %q, true", got, ok, canonicalPath(root))
	}
}

func TestResolveGitProjectRootSymlinkedWorkdir(t *testing.T) {
	real := t.TempDir()
	initRepo(t, real)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink not supported: %v", err)
	}
	got, ok := resolveGitProjectRoot(link)
	if !ok || got != canonicalPath(real) {
		t.Fatalf("symlinked workdir root = %q, %v; want %q, true", got, ok, canonicalPath(real))
	}
}

func TestResolveGitProjectRootNestedRepo(t *testing.T) {
	outer := t.TempDir()
	initRepo(t, outer)
	inner := filepath.Join(outer, "inner")
	if err := os.Mkdir(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	initRepo(t, inner)
	got, ok := resolveGitProjectRoot(inner)
	if !ok || got != canonicalPath(inner) {
		t.Fatalf("nested root = %q, %v; want %q, true", got, ok, canonicalPath(inner))
	}
}

func TestResolveGitProjectRootSubmodule(t *testing.T) {
	outer := t.TempDir()
	initRepo(t, outer)
	commitRepo(t, outer)
	inner := t.TempDir()
	initRepo(t, inner)
	commitRepo(t, inner)
	cmd := exec.Command("git", "-C", outer,
		"-c", "user.name=omac test", "-c", "user.email=omac@example.invalid",
		"-c", "protocol.file.allow=always",
		"submodule", "add", "-q", inner, "sub")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git submodule add: %v\n%s", err, out)
	}
	got, ok := resolveGitProjectRoot(filepath.Join(outer, "sub"))
	if !ok || got != canonicalPath(filepath.Join(outer, "sub")) {
		t.Fatalf("submodule root = %q, %v; want %q, true", got, ok, canonicalPath(filepath.Join(outer, "sub")))
	}
}

func TestResolveGitProjectRootNonGit(t *testing.T) {
	if root, ok := resolveGitProjectRoot(t.TempDir()); ok {
		t.Fatalf("non-git dir resolved to %q", root)
	}
}

func TestResolveGitProjectRootBare(t *testing.T) {
	root := t.TempDir()
	testGit(t, root, "init", "--bare", "-q")
	if got, ok := resolveGitProjectRoot(root); ok {
		t.Fatalf("bare repo resolved to %q", got)
	}
}

func TestResolveGitProjectRootRejectsForgedGitDir(t *testing.T) {
	other := t.TempDir()
	initRepo(t, other)
	victim := t.TempDir()
	gitfile := "gitdir: " + filepath.Join(other, ".git") + "\n"
	if err := os.WriteFile(filepath.Join(victim, ".git"), []byte(gitfile), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := resolveGitProjectRoot(victim); ok {
		t.Fatalf("forged .git scoped the project to %q", got)
	}
}

func TestResolveGitProjectRootFailsWhenGitMissing(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if got, ok := resolveGitProjectRoot(t.TempDir()); ok {
		t.Fatalf("failing git resolved to %q", got)
	}
}

func TestResolveGitProjectRootTimesOut(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\n/bin/sleep 5\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	start := time.Now()
	if got, ok := resolveGitProjectRoot(t.TempDir()); ok {
		t.Fatalf("timed-out git resolved to %q", got)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("resolver waited %v, want the %v timeout", elapsed, gitRootTimeout)
	}
}

func TestPathUnderBoundary(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	if !pathUnder(root, filepath.Join(root, "a", ".env")) {
		t.Fatal("a path below the root must match")
	}
	if pathUnder(root, root+"-other") {
		t.Fatal("a sibling with the same prefix must not match")
	}
	if pathUnder(root, filepath.Dir(root)) {
		t.Fatal("the parent must not match")
	}
}
