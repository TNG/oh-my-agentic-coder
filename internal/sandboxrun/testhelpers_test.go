package sandboxrun

import (
	"os"
	"os/exec"
	"testing"
)

// gitInDir runs git in dir during test setup (outside the sandbox), with a
// hermetic identity and no host config. Shared by the linux and darwin
// integration_worktree test files.
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

// testGitRun runs git in dir during unit tests (outside the sandbox) with a
// hermetic identity. Used by grants_test.go tests that drive a real git
// repo to exercise resolve -> backend-generation. Unlike gitInDir it does
// not null out GIT_CONFIG_GLOBAL/SYSTEM (the grants tests historically
// didn't), preserving their original behaviour.
func testGitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
