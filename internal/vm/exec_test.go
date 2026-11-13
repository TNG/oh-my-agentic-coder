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

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
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
	if slashed := WorktreeSlug("/Users/a/Code/My Repo/"); slashed != s {
		t.Fatalf("trailing slash changes the slug: %q vs %q", slashed, s)
	}
	if RemoteDir(s) != "omac/"+s+"/" {
		t.Fatalf("RemoteDir = %q", RemoteDir(s))
	}
}

// git runs a real git command in dir and fails the test on error.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	var stderr bytes.Buffer
	args = append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "protocol.file.allow=always"}, args...)
	if err := ExecRunner(context.Background(), "git", args, nil, io.Discard, &stderr); err != nil {
		t.Fatalf("git %q: %v: %s", args, err, stderr.String())
	}
}

// newRepo creates a git repository with files (name → content) and returns
// its path with symlinks resolved, as git reports it.
func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	writeFiles(t, dir, files)
	return dir
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRepoRoot(t *testing.T) {
	ctx := context.Background()
	top := newRepo(t, map[string]string{"backend/src/a.go": "package a"})
	for dir, wantPrefix := range map[string]string{
		top:                                  "",
		filepath.Join(top, "backend"):        "backend",
		filepath.Join(top, "backend", "src"): "backend/src",
	} {
		got, prefix, ok, err := RepoRoot(ctx, ExecRunner, dir)
		if err != nil || !ok || got != top || prefix != wantPrefix {
			t.Errorf("RepoRoot(%s) = %q, %q, %v, %v; want %q, %q, true", dir, got, prefix, ok, err, top, wantPrefix)
		}
	}

	if _, _, ok, err := RepoRoot(ctx, ExecRunner, t.TempDir()); err != nil || ok {
		t.Errorf("non-repo: ok = %v, err = %v; want false, nil", ok, err)
	}
}

func TestGitRunsWithCLocale(t *testing.T) {
	var envs [][]string
	run := func(ctx context.Context, _ string, _ []string, _ io.Reader, stdout, _ io.Writer) error {
		envs = append(envs, commandEnv(ctx))
		_, _ = io.WriteString(stdout, "/w/repo\n\n")
		return nil
	}
	if _, _, _, err := RepoRoot(context.Background(), run, "/w/repo"); err != nil {
		t.Fatal(err)
	}
	if _, err := IgnoredPaths(context.Background(), run, "/w/repo"); err != nil {
		t.Fatal(err)
	}
	for i, env := range envs {
		if !slices.Contains(env, "LC_ALL=C") {
			t.Errorf("git call %d env = %q, want LC_ALL=C", i, env)
		}
	}
}

func TestExecRunnerAppliesCommandEnv(t *testing.T) {
	var out bytes.Buffer
	ctx := withCommandEnv(context.Background(), "LC_ALL=C")
	if err := ExecRunner(ctx, "sh", []string{"-c", "printf %s \"$LC_ALL\""}, nil, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if out.String() != "C" {
		t.Fatalf("LC_ALL in the child = %q, want C", out.String())
	}
}

func TestRepoRootFailsOnOtherGitErrors(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, _ io.Reader, _, stderr io.Writer) error {
		_, _ = io.WriteString(stderr, "fatal: detected dubious ownership in repository at '/w/repo'\n")
		return exitErr(t, 128)
	}
	_, _, _, err := RepoRoot(context.Background(), run, "/w/repo")
	if err == nil || !strings.Contains(err.Error(), "dubious ownership") {
		t.Fatalf("err = %v, want the git error instead of copying everything", err)
	}
}

func TestIgnoredPathsUsesGit(t *testing.T) {
	top := newRepo(t, map[string]string{
		".gitignore":   "build/\n*.log\ngen/\n",
		"build/out.o":  "x",
		"x.log":        "x",
		"src/main.go":  "package main",
		"src/keep.txt": "x",
		"gen/a/b.txt":  "x", // a subdirectory whose content is all ignored
	})
	got, err := IgnoredPaths(context.Background(), ExecRunner, top)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"build/", "gen/", "x.log"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ignored = %q, want %q", got, want)
	}
}

func TestIgnoredPathsIncludesSubmodules(t *testing.T) {
	lib := newRepo(t, map[string]string{".gitignore": "out/\n", "lib.go": "package lib"})
	git(t, lib, "add", ".")
	git(t, lib, "commit", "-qm", "init")
	top := newRepo(t, map[string]string{".gitignore": "build/\n", "build/x": "x"})
	git(t, top, "submodule", "add", "-q", lib, "libs/lib")
	writeFiles(t, top, map[string]string{"libs/lib/out/o.class": "x"})

	got, err := IgnoredPaths(context.Background(), ExecRunner, top)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"build/", "libs/lib/out/"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ignored = %q, want %q", got, want)
	}
}

func TestIgnoredPathsDropsBareDot(t *testing.T) {
	run := func(_ context.Context, _ string, args []string, _ io.Reader, stdout, _ io.Writer) error {
		if slices.Contains(args, "ls-files") {
			_, _ = io.WriteString(stdout, "./\x00build/\x00")
		}
		return nil
	}
	got, err := IgnoredPaths(context.Background(), run, "/w/repo")
	if err != nil || !reflect.DeepEqual(got, []string{"build/"}) {
		t.Fatalf("IgnoredPaths = %q, %v; want [build/]", got, err)
	}
}

func readExcludeFile(t *testing.T, ignored []string) []string {
	t.Helper()
	p, err := WriteExcludeFile(t.TempDir(), ignored)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

func TestWriteExcludeFileAlwaysExcludesProtectedFiles(t *testing.T) {
	protected := sandboxprofile.PlatformBaseline().WorkdirProtected
	if !slices.Contains(protected, ".env") {
		t.Fatalf("baseline lacks .env: %q", protected)
	}
	lines := readExcludeFile(t, nil)
	if want := append([]string{".git"}, protected...); !slices.Equal(lines, want) {
		t.Fatalf("exclude file = %q, want %q", lines, want)
	}
}

func TestWriteExcludeFileAnchorsIgnoredPaths(t *testing.T) {
	for _, c := range []struct{ name, ignored, want string }{
		{"directory", "build/", "/build/"},
		{"plain file", "x.log", "/x.log"},
		{"brackets escaped", "foo[1]", `/foo\[1\]`},
		{"wildcards and backslash escaped", `a*b?\c`, `/a\*b\?\\c`},
		{"backslash kept without wildcard", `plain\name`, `/plain\name`},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := readExcludeFile(t, []string{c.ignored})
			if got := lines[len(lines)-1]; got != c.want {
				t.Fatalf("pattern = %q, want %q", got, c.want)
			}
		})
	}
	if lines := readExcludeFile(t, []string{"a\nb"}); slices.Contains(lines, "/a") {
		t.Fatalf("a name with a newline split into patterns: %q", lines)
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
	// rsync's own ssh never gets a terminal.
	tty := testTarget
	tty.TTY = true
	if got := RsyncArgs("/w/repo", "/tmp/ex", tty, "omac/slug/"); !reflect.DeepEqual(got, want) {
		t.Fatalf("args with TTY =\n%q\nwant\n%q", got, want)
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
	order    []string // "rsync" and "ssh" in call order
	runCalls []string
	rsync    []string
	excludes string
	rsyncErr error
	remotes  []string
	cmdErr   error
	notRepo  error // returned by `git rev-parse`, with git's "not a git repository" message
	// top and prefix are what `git rev-parse --show-toplevel --show-prefix`
	// prints; by default the workdir is the top level.
	top, prefix string
	ttys        []bool // SSHTarget.TTY per ssh call
	onCmd       func() // runs when the command's ssh call starts
}

func (f *execFakes) run(_ context.Context, name string, args []string, _ io.Reader, stdout, stderr io.Writer) error {
	f.runCalls = append(f.runCalls, name)
	switch name {
	case "git":
		if f.notRepo != nil {
			_, _ = io.WriteString(stderr, "fatal: not a git repository (or any of the parent directories): .git\n")
			return f.notRepo
		}
		switch {
		case slices.Contains(args, "rev-parse"):
			top := f.top
			if top == "" {
				top = args[1] // git -C <dir>
			}
			_, _ = io.WriteString(stdout, top+"\n"+f.prefix+"\n")
		case slices.Contains(args, "ls-files"):
			// One ignored directory, in the top level only.
			if args[1] == f.top || f.top == "" {
				_, _ = io.WriteString(stdout, "build/\x00")
			}
		}
		return nil
	case "rsync":
		f.order = append(f.order, "rsync")
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

func (f *execFakes) ssh(_ context.Context, t SSHTarget, _ io.Reader, stdout, _ io.Writer, remote string) error {
	f.order = append(f.order, "ssh")
	f.remotes = append(f.remotes, remote)
	f.ttys = append(f.ttys, t.TTY)
	if len(f.remotes) == 2 {
		if f.onCmd != nil {
			f.onCmd()
		}
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
	// The registration creates the copy's directory, so it runs before rsync.
	if want := []string{"ssh", "rsync", "ssh"}; !slices.Equal(f.order, want) {
		t.Fatalf("call order = %q, want %q", f.order, want)
	}
	if n := len(f.rsync); n < 2 || f.rsync[n-2] != "/w/My Repo/" || f.rsync[n-1] != "alice@127.0.0.1:omac/"+slug+"/" {
		t.Fatalf("rsync args = %q", f.rsync)
	}
	if !strings.HasPrefix(f.excludes, ".git\n.env\n") || !strings.HasSuffix(f.excludes, "\n/build/\n") {
		t.Fatalf("exclude file = %q", f.excludes)
	}
	if len(f.remotes) != 2 {
		t.Fatalf("ssh calls = %q", f.remotes)
	}
	entry := ShellQuote(slug + " " + workdir)
	wantReg := "mkdir -p -- " + ShellQuote("omac/"+slug) + " && { grep -qxF -- " + entry + " omac/.worktrees 2>/dev/null || printf '%s\\n' " + entry + " >> omac/.worktrees; }"
	if f.remotes[0] != wantReg {
		t.Fatalf("registration =\n%s\nwant\n%s", f.remotes[0], wantReg)
	}
	wantCmd, _ := RemoteCommand(RemoteDir(slug), "sub", []string{"A=b c"}, []string{"make", "test"})
	if f.remotes[1] != wantCmd {
		t.Fatalf("command = %s, want %s", f.remotes[1], wantCmd)
	}
}

func TestExecCleansWorkdir(t *testing.T) {
	f := &execFakes{}
	if _, err := Exec(context.Background(), f.opts("/w/repo/", io.Discard)); err != nil {
		t.Fatal(err)
	}
	slug := WorktreeSlug("/w/repo")
	if !strings.Contains(f.remotes[0], ShellQuote(slug+" /w/repo")) {
		t.Fatalf("registration = %s, want the cleaned path", f.remotes[0])
	}
	if n := len(f.rsync); f.rsync[n-1] != "alice@127.0.0.1:"+RemoteDir(slug) {
		t.Fatalf("rsync target = %q", f.rsync[n-1])
	}
}

func TestExecCopiesTopLevelFromSubdir(t *testing.T) {
	// Run from <repo>/backend with --cwd sub: the copy is the whole
	// repository, and the command runs in backend/sub of it.
	f := &execFakes{top: "/w/repo", prefix: "backend/"}
	if _, err := Exec(context.Background(), f.opts("/w/repo/backend", io.Discard)); err != nil {
		t.Fatal(err)
	}
	slug := WorktreeSlug("/w/repo")
	if n := len(f.rsync); f.rsync[n-2] != "/w/repo/" || f.rsync[n-1] != "alice@127.0.0.1:"+RemoteDir(slug) {
		t.Fatalf("rsync = %q, want the top level copied to %s", f.rsync, RemoteDir(slug))
	}
	if f.remotes[0] != registerCmd(slug, "/w/repo") {
		t.Fatalf("registration = %s", f.remotes[0])
	}
	if want, _ := RemoteCommand(RemoteDir(slug), "backend/sub", []string{"A=b c"}, []string{"make", "test"}); f.remotes[1] != want {
		t.Fatalf("command = %s, want %s", f.remotes[1], want)
	}
}

func TestExecFromSubdirOfRealRepo(t *testing.T) {
	top := newRepo(t, map[string]string{".gitignore": "build/\n", "build/x": "x", "backend/b.txt": "b"})
	f := &execFakes{}
	run := func(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
		if name == "git" {
			return ExecRunner(ctx, name, args, stdin, stdout, stderr)
		}
		return f.run(ctx, name, args, stdin, stdout, stderr)
	}
	slug := WorktreeSlug(top)
	for _, c := range []struct{ workdir, cwd string }{
		{top, "backend"},                    // omac vm exec --cwd backend, from the top level
		{filepath.Join(top, "backend"), ""}, // omac vm exec, from backend/
	} {
		f.remotes = nil
		o := f.opts(c.workdir, io.Discard)
		o.Cwd, o.Run = c.cwd, run
		if _, err := Exec(context.Background(), o); err != nil {
			t.Fatal(err)
		}
		if n := len(f.rsync); f.rsync[n-2] != top+"/" {
			t.Errorf("from %s: rsync source = %q, want %s/", c.workdir, f.rsync[n-2], top)
		}
		if want, _ := RemoteCommand(RemoteDir(slug), "backend", []string{"A=b c"}, []string{"make", "test"}); f.remotes[1] != want {
			t.Errorf("from %s: command = %s, want %s", c.workdir, f.remotes[1], want)
		}
		if !strings.HasSuffix(f.excludes, "\n/build/\n") {
			t.Errorf("from %s: exclude file = %q", c.workdir, f.excludes)
		}
	}
}

func TestExecWarnsOutsideRepoWithoutVerbose(t *testing.T) {
	f := &execFakes{notRepo: exitErr(t, 128)}
	var stderr bytes.Buffer
	o := f.opts("/w/plain", io.Discard)
	o.Stderr = &stderr
	if _, err := Exec(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "not a git work tree") {
		t.Fatalf("stderr = %q, want the non-repo warning", stderr.String())
	}
	if !strings.Contains(f.excludes, "\n.env\n") {
		t.Fatalf("exclude file outside a repo = %q, want the protected files", f.excludes)
	}
}

func TestExecZeroExit(t *testing.T) {
	f := &execFakes{}
	code, err := Exec(context.Background(), f.opts("/w/repo", io.Discard))
	if err != nil || code != 0 {
		t.Fatalf("Exec = %d, %v", code, err)
	}
}

func TestExecRsyncFailureStopsBeforeCommand(t *testing.T) {
	f := &execFakes{rsyncErr: errors.New("boom")}
	_, err := Exec(context.Background(), f.opts("/w/repo", io.Discard))
	if err == nil || !strings.Contains(err.Error(), "rsync") {
		t.Fatalf("err = %v, want rsync error", err)
	}
	if len(f.remotes) != 1 {
		t.Fatalf("ssh calls after rsync failure = %q, want only the registration", f.remotes)
	}
}

func TestExecSSHConnectionFailure(t *testing.T) {
	f := &execFakes{cmdErr: exitErr(t, 255)}
	_, err := Exec(context.Background(), f.opts("/w/repo", io.Discard))
	if err == nil || !strings.Contains(err.Error(), "connection lost or remote command killed (is the VM running? `omac vm status`)") {
		t.Fatalf("err = %v, want connection error naming omac vm status", err)
	}
}

func TestExecRsyncConnectionFailure(t *testing.T) {
	for _, code := range []int{255, 12} {
		f := &execFakes{rsyncErr: exitErr(t, code)}
		_, err := Exec(context.Background(), f.opts("/w/repo", io.Discard))
		if err == nil || !strings.Contains(err.Error(), "omac vm status") {
			t.Errorf("rsync exit %d: err = %v, want the connection hint", code, err)
		}
	}
}

func TestExecInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Ctrl-C cancels the context; the killed ssh then reports exit -1.
	f := &execFakes{onCmd: cancel, cmdErr: errors.New("signal: killed")}
	code, err := Exec(ctx, f.opts("/w/repo", io.Discard))
	if code != 130 || !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Exec = %d, %v; want 130, ErrInterrupted", code, err)
	}
}

func TestExecTTYOnlyForTheCommand(t *testing.T) {
	f := &execFakes{}
	o := f.opts("/w/repo", io.Discard)
	o.TTY = true
	if _, err := Exec(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if want := []bool{false, true}; !slices.Equal(f.ttys, want) {
		t.Fatalf("TTY per ssh call = %v, want %v", f.ttys, want)
	}
	if slices.Contains(f.rsync, "-t") || strings.Contains(f.rsync[6], " -t") {
		t.Fatalf("rsync got a terminal: %q", f.rsync)
	}
}

func TestExecRejectsBadInputBeforeCopy(t *testing.T) {
	for name, mutate := range map[string]func(*ExecOpts){
		"relative workdir": func(o *ExecOpts) { o.Workdir = "repo" },
		"escaping cwd":     func(o *ExecOpts) { o.Cwd = "../etc" },
		"empty cmd":        func(o *ExecOpts) { o.Cmd = nil },
		"quote in key":     func(o *ExecOpts) { o.Conn.Target.KeyPath = "/Users/o'brien/key" },
		"quote in hosts":   func(o *ExecOpts) { o.Conn.Target.KnownHosts = "/Users/o'brien/known_hosts" },
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
