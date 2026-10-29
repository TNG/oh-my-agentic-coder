package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/pathutil"
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
	if !ok || got != pathutil.Canonical(root) {
		t.Fatalf("resolveGitProjectRoot = %q, %v; want %q, true", got, ok, pathutil.Canonical(root))
	}
}

func TestResolveGitProjectRootIgnoresCoreWorktree(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	broadAncestor := filepath.Dir(root)
	testGit(t, root, "config", "core.worktree", broadAncestor)

	got, ok := resolveGitProjectRoot(root)
	if !ok {
		t.Fatalf("a normal checkout with core.worktree must still resolve")
	}
	if got == pathutil.Canonical(broadAncestor) {
		t.Fatalf("core.worktree scoped the project to the ancestor %q", got)
	}
	if want := pathutil.Canonical(root); got != want {
		t.Fatalf("root = %q, want the repo dir %q", got, want)
	}
}

func TestResolveGitProjectRootLinkedWorktree(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	commitRepo(t, root)
	worktree := filepath.Join(root, ".worktrees", "linked")
	testGit(t, root, "worktree", "add", "-q", worktree)
	got, ok := resolveGitProjectRoot(worktree)
	if !ok || got != pathutil.Canonical(root) {
		t.Fatalf("linked worktree root = %q, %v; want %q, true", got, ok, pathutil.Canonical(root))
	}
}

func TestResolveGitProjectRootRejectsWorktreeOutsideRepo(t *testing.T) {
	root := t.TempDir()
	initRepo(t, root)
	commitRepo(t, root)
	outside := filepath.Join(t.TempDir(), "wt")
	testGit(t, root, "worktree", "add", "-q", outside)
	if got, ok := resolveGitProjectRoot(outside); ok {
		t.Fatalf("a worktree outside the repository scoped the project to %q", got)
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
	if !ok || got != pathutil.Canonical(real) {
		t.Fatalf("symlinked workdir root = %q, %v; want %q, true", got, ok, pathutil.Canonical(real))
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
	if !ok || got != pathutil.Canonical(inner) {
		t.Fatalf("nested root = %q, %v; want %q, true", got, ok, pathutil.Canonical(inner))
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
	sub := filepath.Join(outer, "sub")
	got, ok := resolveGitProjectRoot(sub)
	if !ok || got != pathutil.Canonical(sub) {
		t.Fatalf("submodule root = %q, %v; want %q, true", got, ok, pathutil.Canonical(sub))
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

func TestResolveGitProjectRootForgedGitfileStaysAtWorkdir(t *testing.T) {
	admin := t.TempDir()
	// An agent-controlled admin dir: a bare repo with core.worktree moved to a
	// broad ancestor, exactly the shape that fooled --show-toplevel.
	testGit(t, admin, "init", "--bare", "-q")
	testGit(t, admin, "config", "core.bare", "false")
	testGit(t, admin, "config", "core.worktree", filepath.Dir(admin))

	victim := t.TempDir()
	gitfile := "gitdir: " + admin + "\n"
	if err := os.WriteFile(filepath.Join(victim, ".git"), []byte(gitfile), 0o600); err != nil {
		t.Fatal(err)
	}

	got, ok := resolveGitProjectRoot(victim)
	if !ok {
		return // no project option is also safe
	}
	if got != pathutil.Canonical(victim) {
		t.Fatalf("forged .git resolved to %q, want the workdir %q or no root", got, pathutil.Canonical(victim))
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
	got, ok := resolveGitProjectRoot(victim)
	if ok && got != pathutil.Canonical(victim) {
		t.Fatalf("forged .git scoped the project to %q, want the workdir or no root", got)
	}
}

func TestResolveGitProjectRootRejectsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	initRepo(t, home)
	if got, ok := resolveGitProjectRoot(home); ok {
		t.Fatalf("a repo at $HOME resolved to %q", got)
	}
}

func TestAcceptableProjectRootGates(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "a", "b")
	if acceptableProjectRoot(string(filepath.Separator), workdir) {
		t.Fatal("the filesystem root must be refused")
	}
	shallow := filepath.Join(string(filepath.Separator), "Users")
	if acceptableProjectRoot(shallow, filepath.Join(shallow, "x", "y")) {
		t.Fatal("a strict ancestor with one component must be refused")
	}
	if !acceptableProjectRoot(filepath.Join(shallow, "x"), filepath.Join(shallow, "x")) {
		t.Fatal("a two-component repo root must be accepted")
	}
}
