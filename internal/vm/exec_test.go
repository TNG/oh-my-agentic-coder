package vm

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestWorktreeSlug(t *testing.T) {
	s := WorktreeSlug("/Users/a/Code/My Repo")
	if !regexp.MustCompile(`^my-repo-[0-9a-f]{8}$`).MatchString(s) {
		t.Fatalf("slug = %q", s)
	}
	if again := WorktreeSlug("/Users/a/Code/My Repo"); again != s {
		t.Fatalf("not deterministic: %q vs %q", s, again)
	}
	if other := WorktreeSlug("/Users/b/My Repo"); other == s {
		t.Fatalf("same basename, different path, same slug %q", s)
	}
	if RemoteDir(s) != "omac/"+s+"/" {
		t.Fatalf("RemoteDir = %q", RemoteDir(s))
	}
}

func TestIgnoredPathsUsesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	if err := ExecRunner(ctx, "git", []string{"init", "-q", dir}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		".gitignore":   "build/\n*.log\n",
		"build/out.o":  "x",
		"x.log":        "x",
		"src/main.go":  "package main",
		"src/keep.txt": "x",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, ok, err := IgnoredPaths(ctx, ExecRunner, dir)
	if err != nil || !ok {
		t.Fatalf("IgnoredPaths = %v, %v", ok, err)
	}
	slices.Sort(got)
	if want := []string{"build/", "x.log"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ignored = %q, want %q", got, want)
	}

	plain := t.TempDir()
	got, ok, err = IgnoredPaths(ctx, ExecRunner, plain)
	if err != nil || ok || len(got) != 0 {
		t.Fatalf("non-repo: %q, %v, %v; want empty, false, nil", got, ok, err)
	}
}

func TestWriteExcludeFile(t *testing.T) {
	p, err := WriteExcludeFile(t.TempDir(), []string{"build/", "x.log"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if want := ".git/\n/build/\n/x.log\n"; string(raw) != want {
		t.Fatalf("exclude file = %q, want %q", raw, want)
	}
}

var testTarget = SSHTarget{User: "alice", Port: 31022, KeyPath: "/k/agent key", KnownHosts: "/k/known_hosts"}

func TestRsyncArgs(t *testing.T) {
	args := RsyncArgs("/w/repo", "/tmp/ex", testTarget, "omac/slug/")
	want := []string{
		"-rlpt", "--no-owner", "--no-group", "-z",
		"--exclude-from=/tmp/ex",
		"-e", "ssh " + ShellJoin(SSHArgs(testTarget)),
		"/w/repo/", "alice@127.0.0.1:omac/slug/",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args =\n%q\nwant\n%q", args, want)
	}
	if !strings.Contains(args[6], "-p 31022") || !strings.Contains(args[6], "'/k/agent key'") {
		t.Fatalf("-e value = %q", args[6])
	}
	for _, a := range args {
		if strings.Contains(a, "--delete") {
			t.Fatalf("rsync must never delete: %q", args)
		}
	}
	// A trailing slash on the workdir is not doubled.
	if got := RsyncArgs("/w/repo/", "/tmp/ex", testTarget, "omac/slug/"); got[7] != "/w/repo/" {
		t.Fatalf("source = %q", got[7])
	}
}

func TestRemoteCommand(t *testing.T) {
	got, err := RemoteCommand("omac/s/", "backend", []string{"TOKEN=a b"}, []string{"./gradlew", "test", "--offline"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `cd 'omac/s/backend' && exec env TOKEN='a b' ./gradlew test --offline`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	got, err = RemoteCommand("omac/s/", "", nil, []string{"echo", "$(id)"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `cd 'omac/s' && exec echo '$(id)'`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	got, err = RemoteCommand("omac/s/", "it's", []string{"V=x';rm -rf ~;'"}, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `cd 'omac/s/it'\''s' && exec env V='x'\'';rm -rf ~;'\''' true`; got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}

	bad := []struct {
		name string
		cwd  string
		envs []string
		cmd  []string
	}{
		{"parent cwd", "../x", nil, []string{"true"}},
		{"nested parent cwd", "a/../../x", nil, []string{"true"}},
		{"absolute cwd", "/abs", nil, []string{"true"}},
		{"bad env name", "", []string{"1X=y"}, []string{"true"}},
		{"env without =", "", []string{"X"}, []string{"true"}},
		{"injected env name", "", []string{"A;id=y"}, []string{"true"}},
		{"empty cmd", "", nil, nil},
	}
	for _, c := range bad {
		if _, err := RemoteCommand("omac/s/", c.cwd, c.envs, c.cmd); err == nil {
			t.Errorf("%s: want error", c.name)
		}
	}
}

func TestResolveEnv(t *testing.T) {
	getenv := envMap(map[string]string{"A": "1"})
	got, err := ResolveEnv([]string{"A", "B=2"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A=1", "B=2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if _, err := ResolveEnv([]string{"C"}, getenv); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("unset: err = %v", err)
	}
	if _, err := ResolveEnv([]string{"1X=2"}, getenv); err == nil {
		t.Fatal("bad name: want error")
	}
}

// exitErr returns a real *exec.ExitError carrying code.
func exitErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("sh", "-c", "exit "+strconv.Itoa(code)).Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != code {
		t.Fatalf("cannot build ExitError %d: %v", code, err)
	}
	return err
}

type execFakes struct {
	runCalls []string
	rsync    []string
	excludes string
	rsyncErr error
	remotes  []string
	cmdErr   error
}

func (f *execFakes) run(_ context.Context, name string, args []string, _ io.Reader, stdout, _ io.Writer) error {
	f.runCalls = append(f.runCalls, name)
	switch name {
	case "git":
		// Pretend the workdir is a repository with one ignored directory.
		if slices.Contains(args, "ls-files") {
			_, _ = io.WriteString(stdout, "build/\x00")
		}
		return nil
	case "rsync":
		f.rsync = args
		for _, a := range args {
			if p, ok := strings.CutPrefix(a, "--exclude-from="); ok {
				raw, _ := os.ReadFile(p)
				f.excludes = string(raw)
			}
		}
		return f.rsyncErr
	}
	return errors.New("unexpected " + name)
}

func (f *execFakes) ssh(_ context.Context, _ SSHTarget, _ io.Reader, stdout, _ io.Writer, remote string) error {
	f.remotes = append(f.remotes, remote)
	if len(f.remotes) == 2 {
		_, _ = io.WriteString(stdout, "hello\n")
		return f.cmdErr
	}
	return nil
}

func (f *execFakes) opts(workdir string, out io.Writer) ExecOpts {
	return ExecOpts{
		Workdir: workdir,
		Cwd:     "sub",
		Env:     []string{"A=b c"},
		Cmd:     []string{"make", "test"},
		Conn:    ConnInfo{Target: testTarget},
		Run:     f.run,
		SSH:     f.ssh,
		Stdout:  out,
		Stderr:  io.Discard,
	}
}

func TestExecRunsRsyncThenSSH(t *testing.T) {
	f := &execFakes{cmdErr: exitErr(t, 3)}
	workdir := "/w/My Repo"
	slug := WorktreeSlug(workdir)
	var out bytes.Buffer
	code, err := Exec(context.Background(), f.opts(workdir, &out))
	if err != nil || code != 3 {
		t.Fatalf("Exec = %d, %v; want 3, nil", code, err)
	}
	if out.String() != "hello\n" {
		t.Fatalf("stdout = %q", out.String())
	}
	if f.runCalls[len(f.runCalls)-1] != "rsync" {
		t.Fatalf("run calls = %q", f.runCalls)
	}
	if n := len(f.rsync); n < 2 || f.rsync[n-2] != "/w/My Repo/" || f.rsync[n-1] != "alice@127.0.0.1:omac/"+slug+"/" {
		t.Fatalf("rsync args = %q", f.rsync)
	}
	if f.excludes != ".git/\n/build/\n" {
		t.Fatalf("exclude file = %q", f.excludes)
	}
	if len(f.remotes) != 2 {
		t.Fatalf("ssh calls = %q", f.remotes)
	}
	entry := ShellQuote(slug + " " + workdir)
	wantReg := "mkdir -p omac && grep -qxF -- " + entry + " omac/.worktrees 2>/dev/null || printf '%s\\n' " + entry + " >> omac/.worktrees"
	if f.remotes[0] != wantReg {
		t.Fatalf("registration =\n%s\nwant\n%s", f.remotes[0], wantReg)
	}
	wantCmd, _ := RemoteCommand(RemoteDir(slug), "sub", []string{"A=b c"}, []string{"make", "test"})
	if f.remotes[1] != wantCmd {
		t.Fatalf("command = %s, want %s", f.remotes[1], wantCmd)
	}
}

func TestExecZeroExit(t *testing.T) {
	f := &execFakes{}
	code, err := Exec(context.Background(), f.opts("/w/repo", io.Discard))
	if err != nil || code != 0 {
		t.Fatalf("Exec = %d, %v", code, err)
	}
}

func TestExecRsyncFailureStopsBeforeSSH(t *testing.T) {
	f := &execFakes{rsyncErr: errors.New("boom")}
	_, err := Exec(context.Background(), f.opts("/w/repo", io.Discard))
	if err == nil || !strings.Contains(err.Error(), "rsync") {
		t.Fatalf("err = %v, want rsync error", err)
	}
	if len(f.remotes) != 0 {
		t.Fatalf("ssh ran after rsync failure: %q", f.remotes)
	}
}

func TestExecSSHConnectionFailure(t *testing.T) {
	f := &execFakes{cmdErr: exitErr(t, 255)}
	_, err := Exec(context.Background(), f.opts("/w/repo", io.Discard))
	if err == nil || !strings.Contains(err.Error(), "omac vm status") {
		t.Fatalf("err = %v, want connection error naming omac vm status", err)
	}
}

func TestExecRejectsBadInputBeforeCopy(t *testing.T) {
	for name, mutate := range map[string]func(*ExecOpts){
		"relative workdir": func(o *ExecOpts) { o.Workdir = "repo" },
		"escaping cwd":     func(o *ExecOpts) { o.Cwd = "../etc" },
		"empty cmd":        func(o *ExecOpts) { o.Cmd = nil },
	} {
		t.Run(name, func(t *testing.T) {
			f := &execFakes{}
			o := f.opts("/w/repo", io.Discard)
			mutate(&o)
			if _, err := Exec(context.Background(), o); err == nil {
				t.Fatal("want error")
			}
			if len(f.runCalls) != 0 || len(f.remotes) != 0 {
				t.Fatalf("side effects before validation: run=%q ssh=%q", f.runCalls, f.remotes)
			}
		})
	}
}
