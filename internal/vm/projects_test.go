package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir,
		"-c", "user.email=t@example.com", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repoWithWorktree returns a repository and a worktree of it under
// .worktrees/, the layout the project map has to see through.
func repoWithWorktree(t *testing.T) (repo, worktree string) {
	t.Helper()
	repo = canonicalPath(t.TempDir())
	gitIn(t, repo, "init", "-q")
	gitIn(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	worktree = filepath.Join(repo, ".worktrees", "feat")
	gitIn(t, repo, "worktree", "add", "-q", worktree, "-b", "feat")
	return repo, worktree
}

func writeProjectMap(t *testing.T, content string) string {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	path := filepath.Join(cfg, "omac", "vm-projects.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestProjectKeyWorktreeMapsToMainRepo(t *testing.T) {
	repo, worktree := repoWithWorktree(t)
	if got := ProjectKey(repo); got != repo {
		t.Errorf("ProjectKey(repo) = %q, want %q", got, repo)
	}
	if got := ProjectKey(worktree); got != repo {
		t.Errorf("ProjectKey(worktree) = %q, want %q", got, repo)
	}
	sub := filepath.Join(worktree, "internal")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ProjectKey(sub); got != repo {
		t.Errorf("ProjectKey(subdir of worktree) = %q, want %q", got, repo)
	}
}

func TestProjectKeyOutsideRepoIsTheDirectory(t *testing.T) {
	dir := canonicalPath(t.TempDir())
	if got := ProjectKey(dir); got != dir {
		t.Errorf("ProjectKey = %q, want %q", got, dir)
	}
}

func TestMappedInstanceFindsEntryFromWorktree(t *testing.T) {
	repo, worktree := repoWithWorktree(t)
	writeProjectMap(t, repo+": yarp3\n")
	name, ok, err := MappedInstance(worktree)
	if err != nil || !ok || name != "yarp3" {
		t.Fatalf("MappedInstance(worktree) = (%q, %v, %v), want (yarp3, true, nil)", name, ok, err)
	}
}

func TestMappedInstanceResolvesSymlinkedKey(t *testing.T) {
	repo, _ := repoWithWorktree(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	writeProjectMap(t, link+": yarp3\n")
	name, ok, err := MappedInstance(repo)
	if err != nil || !ok || name != "yarp3" {
		t.Fatalf("MappedInstance = (%q, %v, %v), want (yarp3, true, nil)", name, ok, err)
	}
}

func TestMappedInstanceNoEntry(t *testing.T) {
	repo, _ := repoWithWorktree(t)
	writeProjectMap(t, "/somewhere/else: other\n")
	if name, ok, err := MappedInstance(repo); err != nil || ok {
		t.Fatalf("MappedInstance = (%q, %v, %v), want no entry", name, ok, err)
	}
}

func TestMappedInstanceMissingFileIsEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if name, ok, err := MappedInstance(t.TempDir()); err != nil || ok {
		t.Fatalf("MappedInstance = (%q, %v, %v), want no entry", name, ok, err)
	}
}

func TestLoadProjectMapRejectsBadFiles(t *testing.T) {
	cases := map[string]string{
		"invalid yaml":      "{{{",
		"list not map":      "- a\n- b\n",
		"relative key":      "src/yarp3: yarp3\n",
		"tilde key":         "~/src/yarp3: yarp3\n",
		"invalid name":      "/abs/path: Not_Valid\n",
		"non-string value":  "/abs/path: [a]\n",
		"overlong instance": "/abs/path: abcdefghijklmnopq\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeProjectMap(t, content)
			_, err := LoadProjectMap(path)
			if err == nil {
				t.Fatal("LoadProjectMap accepted the file")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name %s", err, path)
			}
		})
	}
}
