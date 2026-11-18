package vm

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestRsyncFlagsCopyLocally runs the local rsync with the production flags
// and exclude list, copying src/ to dst/ on this machine instead of the VM.
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
		"secret\nkey":       "ignored",
		"x[1]\ny":           "ignored",
	})
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	excludes := ExcludeList([]string{"build/", "foo[1]", "secret\nkey", "x[1]\ny"})

	// The production arguments and directory, minus the remote shell and with
	// a local target.
	prod := RsyncArgs(".", testTarget, "omac/slug/")
	var args []string
	for i := 0; i < len(prod)-1; i++ {
		if prod[i] == "-e" {
			i++
			continue
		}
		args = append(args, prod[i])
	}
	args = append(args, dst+"/")
	srcDir, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer srcDir.Close()
	var stderr bytes.Buffer
	err = inDir(srcDir, func() error {
		return ExecRunner(context.Background(), "rsync", args, strings.NewReader(excludes), io.Discard, &stderr)
	})
	if err != nil {
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

// stubSSH records its arguments, one per line, then runs the remote
// command locally the way sshd would hand it to a shell.
const stubSSH = `#!/bin/sh
for a in "$@"; do printf '%s\n' "$a" >> "$SSH_ARGS_FILE"; done
while [ $# -gt 0 ]; do
	case "$1" in
	-i|-p|-o|-l) shift 2 ;;
	-*) shift ;;
	*) break ;;
	esac
done
shift
exec sh -c "$*"
`

// TestRsyncRemoteShellKeepsSSHArgs runs the local rsync with the production
// arguments, including -e, against a stub ssh, so the -e parser of this
// machine's rsync must keep each ssh argument intact.
func TestRsyncRemoteShellKeepsSSHArgs(t *testing.T) {
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync not installed")
	}
	base := t.TempDir()
	bin, src, dst := filepath.Join(base, "bin"), filepath.Join(base, "src"), filepath.Join(base, "dst")
	writeFiles(t, src, map[string]string{"keep.txt": "k", ".env": "SECRET=1"})
	writeFiles(t, bin, map[string]string{"ssh": stubSSH})
	if err := os.Chmod(filepath.Join(bin, "ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	argsFile := filepath.Join(base, "ssh-args")
	ctx := withCommandEnv(context.Background(), "PATH="+bin+":"+os.Getenv("PATH"), "SSH_ARGS_FILE="+argsFile)
	args := RsyncArgs(src, testTarget, dst+"/")
	var stderr bytes.Buffer
	if err := ExecRunner(ctx, "rsync", args, strings.NewReader(ExcludeList(nil)), io.Discard, &stderr); err != nil {
		t.Fatalf("rsync %q: %v: %s", args, err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dst, "keep.txt")); err != nil {
		t.Fatalf("not copied through the stub: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".env")); err == nil {
		t.Fatal(".env copied: the exclude list on stdin did not reach rsync")
	}
	raw, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(string(raw), "\n")
	want := SSHArgs(testTarget)
	if len(got) < len(want) || !reflect.DeepEqual(got[:len(want)], want) {
		t.Fatalf("ssh got %q, want it to start with %q", got, want)
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
