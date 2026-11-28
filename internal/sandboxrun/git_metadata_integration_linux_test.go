//go:build linux

package sandboxrun

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIntegrationGitMetadataReadOnly(t *testing.T) {
	requireBwrap(t)
	g := linuxTestGrants(t)
	gitInDir(t, g.Workdir, "init", "-q")
	g.GitReadOnly = true
	g.AllowPaths = append(g.AllowPaths, filepath.Join(g.Workdir, ".git", "objects"))
	for _, command := range []string{
		"echo bad >> .git/config", "touch .git/commondir", "mkdir .git/rebase-merge", "touch .git/objects/injected", "mv .git moved", "rm -rf .git", "git config core.hooksPath bad",
		"ln .git/config alias",
	} {
		if out, code := runBwrapped(t, g, "/bin/sh", "-c", command); code == 0 {
			t.Errorf("metadata write succeeded: %s: %s", command, out)
		}
	}
	if out, code := runBwrapped(t, g, "/bin/sh", "-c", "printf ok > source; git status --porcelain"); code != 0 {
		t.Fatalf("normal reads/writes failed: %s", out)
	}
	if data, err := os.ReadFile(filepath.Join(g.Workdir, "source")); err != nil || string(data) != "ok" {
		t.Fatal("source file not writable")
	}
	gitInDir(t, g.Workdir, "add", "source")
	gitInDir(t, g.Workdir, "commit", "-qm", "host commit")
}

func TestGitProtectedParentCannotBeRelocated(t *testing.T) {
	requireBwrap(t)
	g := linuxTestGrants(t)
	gitInDir(t, g.Workdir, "init", "-q")
	parent := filepath.Join(g.Workdir, "deployment", "local")
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(parent, ".env")
	if err := os.WriteFile(secret, []byte("private-canary"), 0600); err != nil {
		t.Fatal(err)
	}
	g.GitReadOnly = true
	g.ProtectedPaths = append(g.ProtectedPaths, secret)
	readonly := filepath.Join(g.Workdir, "readonly", "deep")
	if err := os.MkdirAll(readonly, 0700); err != nil {
		t.Fatal(err)
	}
	g.ReadPaths = append(g.ReadPaths, filepath.Dir(readonly))
	g.ProtectedPaths = append(g.ProtectedPaths, filepath.Join(readonly, ".env"))
	for _, command := range []string{"mv deployment moved", "mv deployment/local relocated"} {
		if out, code := runBwrapped(t, g, "/bin/sh", "-c", command); code == 0 {
			t.Fatalf("protected parent moved, invalidating the service deny path: %s: %s", command, out)
		}
	}
	if out, code := runBwrapped(t, g, "/bin/sh", "-c", "printf editable > deployment/local/source; mkdir ordinary; mv ordinary renamed; rmdir renamed"); code != 0 {
		t.Fatalf("ordinary edits or unrelated directory moves failed: %s", out)
	}
	if out, code := runBwrapped(t, g, "/bin/sh", "-c", "touch readonly/deep/reopened"); code == 0 {
		t.Fatalf("pinning a deny parent reopened read-only source: %s", out)
	}
}
