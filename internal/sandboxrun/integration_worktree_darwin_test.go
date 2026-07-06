//go:build darwin

package sandboxrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// gitInDir runs git in dir during test setup (outside the sandbox), with a
// hermetic identity and no host config.
func gitInDir(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestIntegrationWorktreeHooksRunButNotWritable is the real-Seatbelt proof of
// the linked-worktree hooks policy: a host-authored prepare-commit-msg hook RUNS
// during a sandboxed commit (the #30 bug was that it couldn't) and the commit
// succeeds, yet the shared hooks dir is NOT writable — so the agent can't plant
// a hook that runs un-sandboxed on the host's next commit.
//
// Repo under $HOME on purpose: t.TempDir() lives under $TMPDIR, which the
// sandbox baseline grants WRITE — that blanket temp-write would mask the
// read-only hooks grant and make the write-denied assertion a false pass. $HOME
// is read-only baseline, so only the explicit git grants apply.
func TestIntegrationWorktreeHooksRunButNotWritable(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	base, err := os.MkdirTemp(home, ".omac-worktree-e2e-")
	if err != nil {
		t.Skipf("cannot create test dir under home: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitInDir(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "seed"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitInDir(t, repo, "add", "seed")
	gitInDir(t, repo, "commit", "-q", "-m", "seed")

	// A host hook that appends a marker to the commit message — the marker
	// lands in the commit only if the hook ran.
	hooksDir := filepath.Join(repo, ".git", "hooks")
	hook := filepath.Join(hooksDir, "prepare-commit-msg")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'HOOK-RAN' >> \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Linked worktree: .git is a file, admin dir sits outside the workdir.
	wt := filepath.Join(base, "wt")
	gitInDir(t, repo, "worktree", "add", "-q", wt)

	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wt, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}

	// Commit inside the sandbox: hook must run and the commit must succeed.
	commit := "cd " + wt + " && date > f.txt && " +
		"GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null git -c user.name=t -c user.email=t@t add f.txt && " +
		"GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null git -c user.name=t -c user.email=t@t commit -m base"
	if out, code := runSandboxed(t, g, "/bin/sh", "-c", commit); code != 0 {
		t.Fatalf("sandboxed commit failed (exit %d) — #30 regressed:\n%s", code, out)
	}
	c := exec.Command("git", "log", "-1", "--format=%B")
	c.Dir = wt
	msg, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, msg)
	}
	if !strings.Contains(string(msg), "HOOK-RAN") {
		t.Fatalf("prepare-commit-msg hook did not run inside the sandbox; msg=%q", msg)
	}

	// The shared hooks dir must NOT be writable from inside the sandbox.
	evil := filepath.Join(hooksDir, "evil")
	if _, code := runSandboxed(t, g, "/bin/sh", "-c", "echo pwn > "+evil); code == 0 {
		t.Errorf("SECURITY: agent wrote %s inside the sandbox — persistence vector open", evil)
	}
	if _, err := os.Stat(evil); err == nil {
		t.Errorf("SECURITY: %s exists — hooks dir was writable", evil)
	}
}
