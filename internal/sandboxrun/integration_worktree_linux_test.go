//go:build linux

package sandboxrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestIntegrationWorktreeHooksRunButNotWritable is the Linux bwrap
// counterpart of the darwin test: a host-authored prepare-commit-msg hook
// RUNS during a sandboxed commit (the #30 bug was that it couldn't) and the
// commit succeeds, yet the shared hooks dir is NOT writable — so the agent
// can't plant a hook that runs un-sandboxed on the host's next commit.
//
// Repo under $HOME on purpose: t.TempDir() lives under $TMPDIR, which the
// sandbox baseline grants WRITE — that blanket temp-write would mask the
// read-only hooks grant and make the write-denied assertion a false pass.
// $HOME is read-only baseline, so only the explicit git grants apply.
func TestIntegrationWorktreeHooksRunButNotWritable(t *testing.T) {
	requireBwrap(t)
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
	if out, code := runBwrapped(t, g, "/bin/sh", "-c", commit); code != 0 {
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
	if _, code := runBwrapped(t, g, "/bin/sh", "-c", "echo pwn > "+evil); code == 0 {
		t.Errorf("SECURITY: agent wrote %s inside the sandbox — persistence vector open", evil)
	}
	if _, err := os.Stat(evil); err == nil {
		t.Errorf("SECURITY: %s exists — hooks dir was writable", evil)
	}
}

// TestIntegrationWorktreeSymlinkEscape proves the containment claim
// end-to-end under bwrap: a planted `objects -> secret` symlink under the
// common dir must NOT widen a write grant to the secret dir.
func TestIntegrationWorktreeSymlinkEscape(t *testing.T) {
	requireBwrap(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	base, err := os.MkdirTemp(home, ".omac-worktree-esc-")
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

	wt := filepath.Join(base, "wt")
	gitInDir(t, repo, "worktree", "add", "-q", wt)

	secret := filepath.Join(base, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(secret, "pwned")

	// Plant the symlink: objects -> secret. The real objects dir is moved
	// aside so git's structural shape is preserved but the grant target
	// would escape if containment failed.
	// NOTE: after this point the repo is structurally invalid (objects is a
	// symlink to an empty dir); no further git ops are run against it. The
	// test only exercises grant resolution + bwrap containment, not git.
	common := filepath.Join(repo, ".git")
	realObjects := filepath.Join(common, "objects")
	if err := os.Rename(realObjects, filepath.Join(base, "real-objects")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(common, "objects")); err != nil {
		t.Fatal(err)
	}

	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wt, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}

	if _, code := runBwrapped(t, g, "/bin/sh", "-c", "echo x > "+target); code == 0 {
		if _, serr := os.Stat(target); serr == nil {
			t.Fatalf("ESCAPE: wrote %s via objects symlink — sandbox boundary broken", target)
		}
		t.Fatalf("ESCAPE: exit 0 writing %s (file may not exist but exit code wrong)", target)
	}
	if _, serr := os.Stat(target); serr == nil {
		t.Fatalf("ESCAPE: %s exists — objects symlink widened the grant", target)
	}
}

// TestIntegrationWorktreeKnownLimitations verifies the PR body's "Known
// limitation" claims under real bwrap: branch -d fails cleanly, the
// packed-refs.lock EPERM is non-fatal (commit succeeds), gc no-ops, and
// fsck stays clean after all operations.
func TestIntegrationWorktreeKnownLimitations(t *testing.T) {
	requireBwrap(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	base, err := os.MkdirTemp(home, ".omac-worktree-lim-")
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
	gitInDir(t, repo, "pack-refs", "--all") // force packed-refs to exist

	wt := filepath.Join(base, "wt")
	gitInDir(t, repo, "worktree", "add", "-q", wt, "-b", "feature")

	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, wt, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}

	sandboxGit := func(args ...string) (string, int) {
		env := "GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null"
		full := "cd " + wt + " && " + env + " git -c user.name=t -c user.email=t@t " + strings.Join(args, " ")
		return runBwrapped(t, g, "/bin/sh", "-c", full)
	}

	// 1. Commit must succeed even with packed-refs present (non-fatal EPERM).
	out, code := sandboxGit("commit", "--allow-empty", "-m", "c1")
	if code != 0 {
		t.Fatalf("commit failed (exit %d) — packed-refs.lock EPERM must be non-fatal:\n%s", code, out)
	}
	if strings.Contains(strings.ToLower(out), "fatal") {
		t.Errorf("commit produced fatal output:\n%s", out)
	}

	// 2. git gc should no-op or succeed, not corrupt.
	// gc may emit warnings (non-zero exit without "fatal"); only flag
	// fatal failures — a non-zero exit with mere warnings is acceptable.
	out, code = sandboxGit("gc", "--quiet")
	if code != 0 && strings.Contains(strings.ToLower(out), "fatal") {
		t.Errorf("gc produced fatal output (warnings OK, fatals not):\n%s", out)
	}

	// 3. branch -d should fail cleanly (exit non-zero), ref preserved.
	_, code = sandboxGit("branch", "-d", "feature")
	if code == 0 {
		t.Errorf("branch -d succeeded inside sandbox — PR says it should fail (needs common-root lock)")
	}
	c := exec.Command("git", "rev-parse", "--verify", "feature")
	c.Dir = repo
	if err := c.Run(); err != nil {
		t.Errorf("feature ref missing after failed branch -d — corruption: %v", err)
	}

	// 4. fsck clean after all sandboxed ops.
	c = exec.Command("git", "fsck", "--full")
	c.Dir = repo
	if fsckOut, ferr := c.CombinedOutput(); ferr != nil {
		t.Errorf("fsck failed after sandboxed ops: %v\n%s", ferr, fsckOut)
	}
}

// TestIntegrationWorktreeLandlockCombined exercises the combined bwrap +
// Landlock path: a linked worktree's commit-relevant dirs are bound via
// bwrap while stage2 applies Landlock network rules. Proves the two
// enforcement layers compose — commit succeeds (fs grants work) AND
// network stays blocked (Landlock holds) in the same sandbox.
func TestIntegrationWorktreeLandlockCombined(t *testing.T) {
	requireBwrap(t)
	if !LandlockNetSupported() {
		t.Skipf("Landlock ABI %d < 4", LandlockABI())
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	base, err := os.MkdirTemp(home, ".omac-worktree-ll-")
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

	wt := filepath.Join(base, "wt")
	gitInDir(t, repo, "worktree", "add", "-q", wt, "-b", "feature")

	// Build the omac binary: stage2 must be a real `omac sandbox stage2`
	// invocation (the test binary cannot stand in for it).
	omac := filepath.Join(t.TempDir(), "omac")
	build := exec.Command("go", "build", "-o", omac, "github.com/tngtech/oh-my-agentic-coder/cmd/omac")
	build.Env = os.Environ()
	if out, berr := build.CombinedOutput(); berr != nil {
		t.Fatalf("build omac: %v\n%s", berr, out)
	}

	// Filtered mode with no open ports = full TCP block via Landlock.
	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered},
	}
	g, err := ResolveGrants(p, wt, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	g.ReadPaths = append(g.ReadPaths, omac)

	stage2 := []string{omac, "sandbox", "stage2"}
	stage2 = append(stage2, Stage2Args(g)...)

	// Commit inside the sandbox: must succeed (worktree binds work) AND
	// network must stay blocked (Landlock holds) in the same process.
	commitCmd := "cd " + wt + " && date > f.txt && " +
		"GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null git -c user.name=t -c user.email=t@t add f.txt && " +
		"GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null git -c user.name=t -c user.email=t@t commit -m base"
	argvTail := append(append([]string{}, stage2...), "--", "/bin/sh", "-c", commitCmd)
	argv, err := BuildBwrapArgv(g, argvTail)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("sandboxed commit failed (exit %d) — combined bwrap+Landlock path broken:\n%s", ee.ExitCode(), out)
		}
		t.Fatalf("exec: %v (%s)", err, out)
	}

	// Verify the commit landed.
	c := exec.Command("git", "log", "-1", "--format=%s")
	c.Dir = wt
	msg, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git log: %v\n%s", err, msg)
	}
	if strings.TrimSpace(string(msg)) != "base" {
		t.Errorf("commit message mismatch: got %q", string(msg))
	}

	// Network must be blocked: curl to a local server should fail.
	srvOut := filepath.Join(base, "srv")
	if err := os.MkdirAll(srvOut, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not installed")
	}
	netCmd := "curl -sS --max-time 3 http://127.0.0.1:1/ 2>&1; exit $?"
	argvTail = append(append([]string{}, stage2...), "--", "/bin/sh", "-c", netCmd)
	argv, err = BuildBwrapArgv(g, argvTail)
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(argv[0], argv[1:]...)
	if netOut, netErr := cmd.CombinedOutput(); netErr == nil {
		t.Errorf("network must be blocked under Landlock, curl succeeded: %s", netOut)
	}
}
