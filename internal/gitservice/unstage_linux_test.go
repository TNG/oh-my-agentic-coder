//go:build linux

package gitservice

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnstagePreservesWorktreeAndOtherIndexEntries(t *testing.T) {
	for _, unborn := range []bool{true, false} {
		t.Run(fmt.Sprintf("unborn=%t", unborn), func(t *testing.T) {
			dir := gitFixture(t)
			writeFile(t, filepath.Join(dir, "source"), "base")
			writeFile(t, filepath.Join(dir, "deleted"), "base")
			if !unborn {
				fixtureGit(t, dir, "add", ".")
				fixtureGit(t, dir, "commit", "-qm", "base")
			}
			s := newTestService(t, dir)
			writeFile(t, filepath.Join(dir, "source"), "edited")
			for _, name := range []string{"added", "-literal", "keep"} {
				writeFile(t, filepath.Join(dir, name), "new")
			}
			if err := os.Symlink("source", filepath.Join(dir, "link")); err != nil {
				t.Fatal(err)
			}
			if !unborn {
				if err := os.Remove(filepath.Join(dir, "deleted")); err != nil {
					t.Fatal(err)
				}
			}
			paths := []string{"source", "added", "deleted", "link", "-literal"}
			mutate(t, s, "stage", Request{Paths: append(append([]string{}, paths...), "keep")})
			if unborn {
				if err := os.Remove(filepath.Join(dir, "deleted")); err != nil {
					t.Fatal(err)
				}
			}
			before := status(t, s)
			req := Request{ID: fmt.Sprintf("%s:%032x", before.Session, 100), Expected: &before, Paths: paths}
			code, result := call(t, s, "unstage", &req)
			if code != 200 || result.Unknown || result.State == nil {
				t.Fatalf("unstage: %d %+v", code, result)
			}
			after := status(t, s)
			if after.Head != before.Head || after.Branch != before.Branch || !after.Dirty {
				t.Fatalf("bad state: %+v", after)
			}
			if data, err := os.ReadFile(filepath.Join(dir, "source")); err != nil || string(data) != "edited" {
				t.Fatalf("worktree modified: %q %v", data, err)
			}
			for _, name := range []string{"added", "-literal", "keep"} {
				if data, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(data) != "new" {
					t.Fatalf("worktree modified: %s %q %v", name, data, err)
				}
			}
			if _, err := os.Lstat(filepath.Join(dir, "deleted")); !os.IsNotExist(err) {
				t.Fatalf("deleted file restored: %v", err)
			}
			if target, err := os.Readlink(filepath.Join(dir, "link")); err != nil || target != "source" {
				t.Fatalf("symlink modified: %s %v", target, err)
			}
			if got := fixtureGit(t, dir, "diff", "--cached", "--name-only"); got != "keep" {
				t.Fatalf("wrong staged paths: %q", got)
			}
			code, replay := call(t, s, "unstage", &req)
			if code != 200 || replay.State.Index != result.State.Index || status(t, s).Index != after.Index {
				t.Fatal("unstage replay changed index")
			}
			if !unborn {
				mutate(t, s, "branches", Request{Branch: "topic", Target: before.Head})
				state := status(t, s)
				code, result := call(t, s, "switch", &Request{ID: fmt.Sprintf("%s:%032x", s.session, 101), Expected: &state, Branch: "topic", Target: state.Head})
				if code != 409 || result.Unknown || !strings.Contains(result.Error, "clean working tree") {
					t.Fatalf("dirty switch: %d %+v", code, result)
				}
			}
		})
	}
}

func TestIndexOperationsRejectDirectoriesEvenWhenDeleted(t *testing.T) {
	for _, action := range []string{"stage", "unstage"} {
		t.Run(action, func(t *testing.T) {
			dir := gitFixture(t)
			writeFile(t, filepath.Join(dir, "nested", "leaf"), "base")
			fixtureGit(t, dir, "add", ".")
			fixtureGit(t, dir, "commit", "-qm", "base")
			s := newTestService(t, dir)
			if err := os.RemoveAll(filepath.Join(dir, "nested")); err != nil {
				t.Fatal(err)
			}
			if action == "unstage" {
				mutate(t, s, "stage", Request{Paths: []string{"nested/leaf"}})
			}
			before := status(t, s)
			code, result := call(t, s, action, &Request{ID: fmt.Sprintf("%s:%032x", s.session, 100), Expected: &before, Paths: []string{"nested"}})
			if code != 409 || result.Unknown || !strings.Contains(result.Error, "explicit files") || status(t, s).Index != before.Index {
				t.Fatalf("directory accepted: %d %+v", code, result)
			}
		})
	}
}

func TestUnstageRejectsProtectedAndUnsupportedPaths(t *testing.T) {
	dir := gitFixture(t)
	writeFile(t, filepath.Join(dir, ".env"), "secret")
	writeFile(t, filepath.Join(dir, "source"), "base")
	fixtureGit(t, dir, "add", ".")
	fixtureGit(t, dir, "commit", "-qm", "base")
	writeFile(t, filepath.Join(dir, ".env"), "changed-secret")
	fixtureGit(t, dir, "add", ".env")
	s := newTestService(t, dir, filepath.Join(dir, ".env"))
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "outside")); err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{".env", "../escape", ".git/config", ".gitmodules", "outside/file", ".", "missing"} {
		before := status(t, s)
		code, result := call(t, s, "unstage", &Request{ID: fmt.Sprintf("%s:%032x", s.session, i+1), Expected: &before, Paths: []string{path}})
		if code != 409 || result.Unknown || status(t, s).Index != before.Index {
			t.Fatalf("path %q: %d %+v", path, code, result)
		}
	}
	writeFile(t, filepath.Join(dir, "source"), "edited")
	mutate(t, s, "stage", Request{Paths: []string{"source"}})
}

func TestUnstageRequestFields(t *testing.T) {
	s := newTestService(t, gitFixture(t))
	state := status(t, s)
	for _, change := range []func(*Request){
		func(r *Request) { r.Paths = nil },
		func(r *Request) { r.Paths = make([]string, 257) },
		func(r *Request) { r.Message = "message" },
		func(r *Request) { r.Signoff = true },
		func(r *Request) { r.Branch = "main" },
		func(r *Request) { r.Target = strings.Repeat("a", 40) },
	} {
		req := Request{ID: fmt.Sprintf("%s:%032x", s.session, 1), Expected: &state, Paths: []string{"source"}}
		change(&req)
		if code, _ := call(t, s, "unstage", &req); code != 400 {
			t.Fatalf("invalid request accepted: %+v", req)
		}
	}
}
