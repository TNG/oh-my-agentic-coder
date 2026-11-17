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

// Two spellings of one directory would otherwise let map iteration order
// decide which VM a project gets.
func TestLoadProjectMapRejectsKeysForTheSameDirectory(t *testing.T) {
	repo, _ := repoWithWorktree(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}
	path := writeProjectMap(t, repo+": a\n"+link+": b\n")
	_, err := LoadProjectMap(path)
	if err == nil {
		t.Fatal("LoadProjectMap accepted two keys for one directory")
	}
	for _, want := range []string{path, repo, link} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantEntry(t *testing.T, dir, want string) {
	t.Helper()
	name, ok, err := MappedInstance(dir)
	if err != nil {
		t.Fatalf("MappedInstance(%s): %v", dir, err)
	}
	if want == "" && ok {
		t.Errorf("MappedInstance(%s) = %q, want no entry", dir, name)
	}
	if want != "" && (!ok || name != want) {
		t.Errorf("MappedInstance(%s) = (%q, %v), want %q", dir, name, ok, want)
	}
}

func mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// An entry covers its directory and everything below it; the longest key
// wins. A linked worktree elsewhere also gets its repository's entry.
func TestMappedInstanceLookup(t *testing.T) {
	repo, worktree := repoWithWorktree(t)
	outside := filepath.Join(canonicalPath(t.TempDir()), "wt")
	gitIn(t, repo, "worktree", "add", "-q", outside, "-b", "outside")
	writeProjectMap(t, repo+": a\n"+filepath.Join(repo, "special")+": b\n")

	wantEntry(t, repo, "a")
	wantEntry(t, mkdir(t, filepath.Join(worktree, "internal")), "a")
	wantEntry(t, mkdir(t, filepath.Join(outside, "src")), "a")
	wantEntry(t, mkdir(t, filepath.Join(repo, "special", "x")), "b")
	wantEntry(t, t.TempDir(), "")

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Join(repo, "special"), link); err != nil {
		t.Fatal(err)
	}
	wantEntry(t, link, "b")
}

// The sandbox can write the project, .git included. Nothing it writes there
// may lead the lookup to another repository's entry, or the next bare --vm
// session would log in to that repository's VM.
func TestMappedInstanceIgnoresPlantedGitPointers(t *testing.T) {
	other, otherWorktree := repoWithWorktree(t)
	admin := filepath.Join(other, ".git", "worktrees", filepath.Base(otherWorktree))
	writeProjectMap(t, other+": other\n")

	t.Run("commondir in a .git directory", func(t *testing.T) {
		repo, _ := repoWithWorktree(t)
		writeFile(t, filepath.Join(repo, ".git", "commondir"), filepath.Join(other, ".git")+"\n")
		wantEntry(t, repo, "")
	})
	t.Run(".git file naming another worktree's admin dir", func(t *testing.T) {
		dir := canonicalPath(t.TempDir())
		writeFile(t, filepath.Join(dir, ".git"), "gitdir: "+admin+"\n")
		wantEntry(t, dir, "")
	})
	t.Run(".git symlink to another worktree's .git", func(t *testing.T) {
		dir := canonicalPath(t.TempDir())
		if err := os.Symlink(filepath.Join(otherWorktree, ".git"), filepath.Join(dir, ".git")); err != nil {
			t.Fatal(err)
		}
		wantEntry(t, dir, "")
	})
	t.Run("forged admin dir inside the project", func(t *testing.T) {
		dir := canonicalPath(t.TempDir())
		fake := filepath.Join(dir, "fake")
		writeFile(t, filepath.Join(dir, ".git"), "gitdir: "+fake+"\n")
		writeFile(t, filepath.Join(fake, "gitdir"), filepath.Join(dir, ".git")+"\n")
		writeFile(t, filepath.Join(fake, "commondir"), filepath.Join(other, ".git")+"\n")
		wantEntry(t, dir, "")
	})
}

// Creating or deleting .git markers below a mapped directory does not
// change its VM: a submodule belongs to its superproject's entry.
func TestMappedInstanceIgnoresGitMarkersBelowAnEntry(t *testing.T) {
	repo, _ := repoWithWorktree(t)
	writeProjectMap(t, repo+": a\n")
	sub := filepath.Join(repo, "sub")
	writeFile(t, filepath.Join(sub, ".git"), "gitdir: ../.git/modules/sub\n")
	wantEntry(t, mkdir(t, filepath.Join(sub, "src")), "a")
	mkdir(t, filepath.Join(repo, "inner", ".git"))
	wantEntry(t, filepath.Join(repo, "inner"), "a")
}
