package vm

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// TestRsyncFlagsCopyLocally runs the local rsync with the production flags
// and exclude file, copying src/ to dst/ on this machine instead of the VM.
func TestRsyncFlagsCopyLocally(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	base := t.TempDir()
	src, dst := filepath.Join(base, "src"), filepath.Join(base, "dst")
	writeFiles(t, src, map[string]string{
		"keep.txt":          "k",
		"foo1":              "copied: foo[1] must not act as a glob",
		"foo[1]":            "ignored",
		"build/out.o":       "ignored",
		"sub/main.go":       "package main",
		"sub/build/x":       "copied: only /build/ at the top is ignored",
		".git":              "gitdir: /elsewhere/.git/worktrees/x", // a `git worktree` checkout
		"sub/.git/HEAD":     "ref: refs/heads/main",
		".env":              "SECRET=1",
		"sub/.env.local":    "SECRET=2",
		"sub/.envrc":        "export SECRET=3",
		"-leading-dash.txt": "copied",
	})
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	excl, err := WriteExcludeFile(base, []string{"build/", "foo[1]"})
	if err != nil {
		t.Fatal(err)
	}

	// The production arguments, minus the remote shell and with a local target.
	prod := RsyncArgs(src, excl, testTarget, "omac/slug/")
	var args []string
	for i := 0; i < len(prod)-1; i++ {
		if prod[i] == "-e" {
			i++
			continue
		}
		args = append(args, prod[i])
	}
	args = append(args, dst+"/")
	var stderr bytes.Buffer
	if err := ExecRunner(context.Background(), "rsync", args, nil, io.Discard, &stderr); err != nil {
		t.Fatalf("rsync %q: %v: %s", args, err, stderr.String())
	}

	var got []string
	err = filepath.WalkDir(dst, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dst, p)
		got = append(got, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-leading-dash.txt", "foo1", "keep.txt", "sub/build/x", "sub/main.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("copied %q, want %q", got, want)
	}
}

// TestRegisterCmdInRealShell runs the registration line twice in sh, the
// way ssh runs it in the agent user's home, and reads the result back with
// the status parser.
func TestRegisterCmdInRealShell(t *testing.T) {
	home := t.TempDir()
	workdir := "/w/it's a -dir"
	slug := WorktreeSlug(workdir)
	for range 2 {
		cmd := exec.Command("sh", "-c", registerCmd(slug, workdir))
		cmd.Dir = home
		cmd.Env = append(os.Environ(), "HOME="+home)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("registration: %v: %s", err, out)
		}
	}
	if fi, err := os.Stat(filepath.Join(home, "omac", slug)); err != nil || !fi.IsDir() {
		t.Fatalf("copy directory not created: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "omac", ".worktrees"))
	if err != nil {
		t.Fatal(err)
	}
	if want := slug + " " + workdir + "\n"; string(raw) != want {
		t.Fatalf("registry = %q, want one line %q", raw, want)
	}
	if got, want := parseWorktreeRegistry(string(raw)), []string{workdir + " (" + slug + ")"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parsed = %q, want %q", got, want)
	}
}
