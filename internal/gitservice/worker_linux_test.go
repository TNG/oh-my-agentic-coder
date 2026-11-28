//go:build linux

package gitservice

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fixtureGit(t, dir, "init", "-q", "-b", "main")
	return dir
}

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("/usr/bin/git", args...)
	c.Dir = dir
	c.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid"}
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func requireWorker(t *testing.T) {
	t.Helper()
	c := exec.Command("/usr/bin/bwrap", "--unshare-net", "--unshare-pid", "--ro-bind", "/usr", "/usr", "--ro-bind", "/lib", "/lib", "--ro-bind", "/lib64", "/lib64", "--proc", "/proc", "--dev", "/dev", "--", "/usr/bin/git", "--version")
	if out, err := c.CombinedOutput(); err != nil {
		if os.Getenv("GITHUB_ACTIONS") == "true" || os.Getenv("OMAC_REQUIRE_GIT_WORKER_TESTS") == "1" {
			t.Fatalf("worker unavailable: %v: %s", err, out)
		}
		t.Skipf("worker requires functional network namespaces: %v: %s", err, out)
	}
}

func newTestService(t *testing.T, dir string, denied ...string) *Service {
	t.Helper()
	requireWorker(t)
	s, err := New(Options{Workdir: dir, Name: "Test", Email: "test@example.invalid"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ConfigureDenies(denied); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWorkerIgnoresRepositoryExecutionConfig(t *testing.T) {
	dir := gitFixture(t)
	canary := filepath.Join(dir, ".git", "executed")
	script := "#!/bin/sh\necho executed > /repo/.git/executed\n"
	hook := filepath.Join(dir, ".git", "hooks", "pre-commit")
	writeFile(t, hook, script)
	if err := os.Chmod(hook, 0700); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"core.fsmonitor", "core.sshCommand", "core.pager", "gpg.program", "filter.attack.clean", "diff.external"} {
		fixtureGit(t, dir, "config", key, "/repo/.git/hooks/pre-commit")
	}
	fixtureGit(t, dir, "config", "commit.gpgsign", "true")
	before, _ := os.ReadFile(filepath.Join(dir, ".git", "config"))
	s := newTestService(t, dir)
	writeFile(t, filepath.Join(dir, "source"), "hello\n")
	if _, err := s.run(context.Background(), true, false, nil, "add", "--", "source"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.run(context.Background(), true, false, nil, "commit", "-m", "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(canary); !os.IsNotExist(err) {
		t.Fatalf("repository code executed: %v", err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if string(before) != string(after) {
		t.Fatal("host config changed")
	}
	if got := fixtureGit(t, dir, "-c", "core.fsmonitor=false", "show", "HEAD:source"); got != "hello" {
		t.Fatal(got)
	}
	// Positive control: this hook can write its canary in the worker when enabled.
	if _, err := s.run(context.Background(), true, false, nil, "-c", "core.hooksPath=/repo/.git/hooks", "commit", "--allow-empty", "-m", "control"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(canary); err != nil {
		t.Fatalf("hook control did not execute: %v", err)
	}
}

func TestWorkerRejectsMetadataAliases(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := gitFixture(t)
			config := filepath.Join(dir, ".git", "config")
			if kind == "hardlink" {
				if err := os.Link(config, filepath.Join(dir, "alias")); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Symlink("../../source", filepath.Join(dir, ".git", "hooks", "pre-commit")); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := New(Options{Workdir: dir, Name: "Test", Email: "test@example.invalid"}); err == nil {
				t.Fatal("accepted metadata alias")
			}
		})
	}
}
