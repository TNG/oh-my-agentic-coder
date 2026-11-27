package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitReadOnlyMountOverridesNestedWrite(t *testing.T) {
	wd := t.TempDir()
	wd, _ = filepath.EvalSymlinks(wd)
	gitdir := filepath.Join(wd, ".git")
	if err := os.MkdirAll(filepath.Join(gitdir, "objects"), 0700); err != nil {
		t.Fatal(err)
	}
	g := &Grants{Workdir: wd, AllowPaths: []string{wd, filepath.Join(gitdir, "objects")}, GitReadOnly: true}
	argv, err := BuildBwrapArgv(g, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	ro := strings.LastIndex(joined, "--ro-bind "+gitdir+" "+gitdir)
	rw := strings.LastIndex(joined, "--bind "+filepath.Join(gitdir, "objects"))
	if ro < 0 || ro < rw {
		t.Fatalf("read-only override missing or ordered before write: %s", joined)
	}
	if g.withUnrestrictedFilesystem().GitReadOnly {
		t.Fatal("unrestricted mode reports a Git restriction")
	}
}

func TestGitReadOnlyRejectsBroaderOrAliasedAuthority(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	wd := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(wd, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(wd, alias); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{"/", root, alias, "/usr/bin"} {
		g := &Grants{Workdir: wd, AllowPaths: []string{wd, extra}, GitReadOnly: true}
		if _, err := BuildBwrapArgv(g, []string{"true"}); err == nil {
			t.Errorf("accepted writable authority %s", extra)
		}
	}
}
