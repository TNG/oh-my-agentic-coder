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

// A sandboxed agent can set core.worktree in the workdir's git config;
// the copy must not then come from another directory.
func TestRepoRootRefusesRedirectedWorktree(t *testing.T) {
	other := newRepo(t, map[string]string{"secret.txt": "other client"})
	top := newRepo(t, map[string]string{"a.go": "package a", "src/b.go": "package b"})
	git(t, top, "config", "core.worktree", other)
	link := filepath.Join(t.TempDir(), "link") // a symlink into the repository
	if err := os.Symlink(filepath.Join(top, "src"), link); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{top, link} {
		if got, _, _, err := RepoRoot(context.Background(), ExecRunner, dir); err == nil {
			t.Errorf("RepoRoot(%s) = %q, nil; want an error for a top level outside the workdir's repository", dir, got)
		}
	}
}

func TestRepoRootRejectsExtraLines(t *testing.T) {
	run := func(_ context.Context, _ string, _ []string, _ io.Reader, stdout, _ io.Writer) error {
		_, _ = io.WriteString(stdout, "/w/re\npo\n\n")
		return nil
	}
	if _, _, _, err := RepoRoot(context.Background(), run, "/w/re\npo"); err == nil {
		t.Fatal("RepoRoot accepted a top level split by a newline")
	}
}

func TestGitRunsWithCLocale(t *testing.T) {
	var envs [][]string
	run := func(ctx context.Context, _ string, args []string, _ io.Reader, stdout, _ io.Writer) error {
		envs = append(envs, commandEnv(ctx))
		if slices.Contains(args, "rev-parse") {
			_, _ = io.WriteString(stdout, "/w/repo\n\n")
		}
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

func TestWithCommandEnvKeepsEarlierEntries(t *testing.T) {
	parent := withCommandEnv(context.Background(), "LC_ALL=C")
	a, b := withCommandEnv(parent, "A=1"), withCommandEnv(parent, "B=2")
	if got, want := commandEnv(a), []string{"LC_ALL=C", "A=1"}; !slices.Equal(got, want) {
		t.Errorf("commandEnv(a) = %q, want %q", got, want)
	}
	if got, want := commandEnv(b), []string{"LC_ALL=C", "B=2"}; !slices.Equal(got, want) {
		t.Errorf("commandEnv(b) = %q, want %q", got, want)
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

func TestIgnoredPathsIncludesNestedSubmodules(t *testing.T) {
	nested := newRepo(t, map[string]string{".gitignore": "gen/\n", "n.go": "package n"})
	git(t, nested, "add", ".")
	git(t, nested, "commit", "-qm", "init")
	lib := newRepo(t, map[string]string{".gitignore": "out/\n", "lib.go": "package lib"})
	git(t, lib, "submodule", "add", "-q", nested, "deps/nested")
	git(t, lib, "add", ".")
	git(t, lib, "commit", "-qm", "init")
	top := newRepo(t, map[string]string{".gitignore": "build/\n", "build/x": "x", "src/a.go": "package a"})
	git(t, top, "submodule", "add", "-q", lib, "libs/lib")
	git(t, top, "submodule", "update", "-q", "--init", "--recursive")
	writeFiles(t, top, map[string]string{
		"libs/lib/out/o.class":          "x",
		"libs/lib/deps/nested/gen/g.go": "x",
	})

	got, err := IgnoredPaths(context.Background(), ExecRunner, top)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	if want := []string{"build/", "libs/lib/deps/nested/gen/", "libs/lib/out/"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ignored = %q, want %q", got, want)
	}
}

// A sandboxed agent can edit git config inside the worktree; the host's git
// calls must not run the command it plants.
func TestGitIgnoresPlantedFSMonitor(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	plant := func(t *testing.T, repo string) {
		git(t, repo, "config", "core.fsmonitor", "touch "+ShellQuote(marker)+"; false")
	}
	lib := newRepo(t, map[string]string{"lib.go": "package lib"})
	git(t, lib, "add", ".")
	git(t, lib, "commit", "-qm", "init")
	for name, setup := range map[string]func(t *testing.T, top string){
		"top level": func(t *testing.T, top string) { plant(t, top) },
		"submodule": func(t *testing.T, top string) {
			git(t, top, "submodule", "add", "-q", lib, "libs/lib")
			plant(t, filepath.Join(top, "libs/lib"))
		},
		"untracked clone": func(t *testing.T, top string) {
			clone := filepath.Join(top, "vendor/x")
			writeFiles(t, clone, map[string]string{"v.c": "x"})
			git(t, clone, "init", "-q")
			plant(t, clone)
		},
	} {
		t.Run(name, func(t *testing.T) {
			_ = os.Remove(marker)
			top := newRepo(t, map[string]string{"src/a.go": "package a"})
			setup(t, top)
			if _, _, _, err := RepoRoot(context.Background(), ExecRunner, top); err != nil {
				t.Fatal(err)
			}
			if _, err := IgnoredPaths(context.Background(), ExecRunner, top); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("git ran the planted core.fsmonitor command on the host")
			}
		})
	}
}

func TestIgnoredPathsFailsOnGitError(t *testing.T) {
	for _, failing := range []string{"ls-files", "submodule"} {
		run := func(_ context.Context, _ string, args []string, _ io.Reader, _, _ io.Writer) error {
			if slices.Contains(args, failing) {
				return exitErr(t, 128)
			}
			return nil
		}
		if _, err := IgnoredPaths(context.Background(), run, "/w/repo"); err == nil {
			t.Errorf("IgnoredPaths with a failing git %s = nil error", failing)
		}
	}
}

// subRunner fakes git for a repository at /w/repo with one submodule at
// libs/lib; lsFiles answers the ignored listing in the submodule.
func subRunner(lsFiles func(stdout io.Writer) error) CommandRunner {
	return func(_ context.Context, _ string, args []string, _ io.Reader, stdout, _ io.Writer) error {
		inSub := args[1] == "/w/repo/libs/lib"
		switch {
		case slices.Contains(args, "submodule") && !inSub:
			_, _ = io.WriteString(stdout, "libs/lib\n")
		case slices.Contains(args, "--ignored") && inSub:
			return lsFiles(stdout)
		}
		return nil
	}
}

func TestIgnoredPathsExcludesFullyIgnoredSubmodule(t *testing.T) {
	run := subRunner(func(stdout io.Writer) error {
		_, _ = io.WriteString(stdout, "./\x00")
		return nil
	})
	got, err := IgnoredPaths(context.Background(), run, "/w/repo")
	if err != nil || !reflect.DeepEqual(got, []string{"libs/lib/"}) {
		t.Fatalf("IgnoredPaths = %q, %v; want [libs/lib/]", got, err)
	}
}

func TestIgnoredPathsFailsOnGitErrorInSubmodule(t *testing.T) {
	run := subRunner(func(io.Writer) error { return exitErr(t, 128) })
	if _, err := IgnoredPaths(context.Background(), run, "/w/repo"); err == nil {
		t.Fatal("IgnoredPaths with a failing git call in a submodule = nil error")
	}
}

func TestIgnoredPathsDropsBareDot(t *testing.T) {
	run := func(_ context.Context, _ string, args []string, _ io.Reader, stdout, _ io.Writer) error {
		if slices.Contains(args, "--ignored") {
			_, _ = io.WriteString(stdout, "./\x00build/\x00")
		}
		return nil
	}
	got, err := IgnoredPaths(context.Background(), run, "/w/repo")
	if err != nil || !reflect.DeepEqual(got, []string{"build/"}) {
		t.Fatalf("IgnoredPaths = %q, %v; want [build/]", got, err)
	}
}

func excludeLines(ignored []string) []string {
	return strings.Split(strings.TrimSuffix(ExcludeList(ignored), "\n"), "\n")
}

func TestExcludeListAlwaysExcludesProtectedFiles(t *testing.T) {
	protected := sandboxprofile.PlatformBaseline().WorkdirProtected
	if !slices.Contains(protected, ".env") {
		t.Fatalf("baseline lacks .env: %q", protected)
	}
	lines := excludeLines(nil)
	if want := append([]string{".git"}, protected...); !slices.Equal(lines, want) {
		t.Fatalf("exclude file = %q, want %q", lines, want)
	}
}

func TestExcludeListAnchorsIgnoredPaths(t *testing.T) {
	for _, c := range []struct{ name, ignored, want string }{
		{"directory", "build/", "/build/"},
		{"plain file", "x.log", "/x.log"},
		{"brackets escaped", "foo[1]", `/foo\[1\]`},
		{"wildcards and backslash escaped", `a*b?\c`, `/a\*b\?\\c`},
		{"backslash kept without wildcard", `plain\name`, `/plain\name`},
		{"line feed", "a\nb", "/a?b"},
		{"carriage return", "a\rb[1]", `/a?b\[1\]`},
		{"bytes that are not UTF-8", "out\xff[1]", "/out\xff\\[1\\]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines := excludeLines([]string{c.ignored})
			if got := lines[len(lines)-1]; got != c.want {
				t.Fatalf("pattern = %q, want %q", got, c.want)
			}
		})
	}
	if lines := excludeLines([]string{"a\nb"}); slices.Contains(lines, "/a") {
		t.Fatalf("a name with a newline split into patterns: %q", lines)
	}
}

var testTarget = SSHTarget{User: "alice", Port: 31022, KeyPath: "/k/agent key", KnownHosts: "/k/known_hosts"}

func TestRsyncArgs(t *testing.T) {
	args := RsyncArgs("/w/repo", testTarget, "omac/slug/")
	want := []string{
		"-rlpt", "--no-owner", "--no-group", "-z",
		"--exclude-from=-",
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
	if got := RsyncArgs("/w/repo/", testTarget, "omac/slug/"); got[7] != "/w/repo/" {
		t.Fatalf("source = %q", got[7])
	}
	// rsync's own ssh never gets a terminal.
	tty := testTarget
	tty.TTY = true
	if got := RsyncArgs("/w/repo", tty, "omac/slug/"); !reflect.DeepEqual(got, want) {
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
	env := map[string]string{"A": "1", "EMPTY": ""}
	getenv := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	got, err := ResolveEnv([]string{"A", "B=2", "EMPTY"}, getenv)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A=1", "B=2", "EMPTY="}; !reflect.DeepEqual(got, want) {
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

// testDir creates rel inside a new temporary directory and returns its
// path without symlinks.
func testDir(t *testing.T, rel string) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

type execFakes struct {
	order    []string // "rsync" and "ssh" in call order
	runCalls []string
	rsync    []string
	rsyncWd  string // the working directory rsync started in
	excludes string
	rsyncErr error
	remotes  []string
	cmdErr   error
	regErr   error // returned by the registration, the first ssh call
	notRepo  error // returned by `git rev-parse`, with git's "not a git repository" message
	// top and prefix are what `git rev-parse --show-toplevel --show-prefix`
	// prints; by default the workdir is the top level.
	top, prefix string
	ttys        []bool // SSHTarget.TTY per ssh call
	onRegister  func() // runs when the registration's ssh call starts
	onCmd       func() // runs when the command's ssh call starts
}

func (f *execFakes) run(_ context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
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
		case slices.Contains(args, "--ignored"):
			// One ignored directory, in the top level only.
			if args[1] == f.top || f.top == "" {
				_, _ = io.WriteString(stdout, "build/\x00")
			}
		}
		return nil
	case "rsync":
		f.order = append(f.order, "rsync")
		f.rsync = args
		f.rsyncWd, _ = os.Getwd()
		if slices.Contains(args, "--exclude-from=-") && stdin != nil {
			raw, _ := io.ReadAll(stdin)
			f.excludes = string(raw)
		}
		return f.rsyncErr
	}
	return errors.New("unexpected " + name)
}

func (f *execFakes) ssh(_ context.Context, t SSHTarget, _ io.Reader, stdout, _ io.Writer, remote string) error {
	f.order = append(f.order, "ssh")
	f.remotes = append(f.remotes, remote)
	f.ttys = append(f.ttys, t.TTY)
	if len(f.remotes) == 1 {
		if f.onRegister != nil {
			f.onRegister()
		}
		return f.regErr
	}
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
	workdir := testDir(t, "My Repo")
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
	if n := len(f.rsync); n < 2 || f.rsync[n-2] != "./" || f.rsync[n-1] != "alice@127.0.0.1:omac/"+slug+"/" {
		t.Fatalf("rsync args = %q", f.rsync)
	}
	if f.rsyncWd != workdir {
		t.Fatalf("rsync ran in %s, want %s", f.rsyncWd, workdir)
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
	repo := testDir(t, "repo")
	if _, err := Exec(context.Background(), f.opts(repo+"/", io.Discard)); err != nil {
		t.Fatal(err)
	}
	slug := WorktreeSlug(repo)
	if !strings.Contains(f.remotes[0], ShellQuote(slug+" "+repo)) {
		t.Fatalf("registration = %s, want the cleaned path", f.remotes[0])
	}
	if n := len(f.rsync); f.rsync[n-1] != "alice@127.0.0.1:"+RemoteDir(slug) {
		t.Fatalf("rsync target = %q", f.rsync[n-1])
	}
}

func TestExecCopiesTopLevelFromSubdir(t *testing.T) {
	// Run from <repo>/backend with --cwd sub: the copy is the whole
	// repository, and the command runs in backend/sub of it.
	top := filepath.Dir(testDir(t, "repo/backend"))
	f := &execFakes{top: top, prefix: "backend/"}
	if _, err := Exec(context.Background(), f.opts(filepath.Join(top, "backend"), io.Discard)); err != nil {
		t.Fatal(err)
	}
	slug := WorktreeSlug(top)
	if n := len(f.rsync); f.rsyncWd != top || f.rsync[n-1] != "alice@127.0.0.1:"+RemoteDir(slug) {
		t.Fatalf("rsync = %q in %s, want the top level copied to %s", f.rsync, f.rsyncWd, RemoteDir(slug))
	}
	if f.remotes[0] != registerCmd(slug, top) {
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
		if f.rsyncWd != top {
			t.Errorf("from %s: rsync ran in %s, want %s", c.workdir, f.rsyncWd, top)
		}
		if want, _ := RemoteCommand(RemoteDir(slug), "backend", []string{"A=b c"}, []string{"make", "test"}); f.remotes[1] != want {
			t.Errorf("from %s: command = %s, want %s", c.workdir, f.remotes[1], want)
		}
		if !strings.HasSuffix(f.excludes, "\n/build/\n") {
			t.Errorf("from %s: exclude file = %q", c.workdir, f.excludes)
		}
	}
}

func TestExecRefusesToCopyProtectedPaths(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	writeFiles(t, home, map[string]string{".ssh/id_ed25519": "x", ".ssh/keys/k": "x", "proj/a.go": "package a"})
	t.Setenv("HOME", home)
	link := filepath.Join(base, "link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	keysLink := filepath.Join(base, "keys")
	if err := os.Symlink(filepath.Join(home, ".ssh", "keys"), keysLink); err != nil {
		t.Fatal(err)
	}
	type copyRoot struct {
		workdir string
		repo    bool
	}
	cases := map[string]copyRoot{
		"home":                   {home, false},
		"home as a repository":   {home, true},
		"parent of home":         {base, false},
		"protected directory":    {filepath.Join(home, ".ssh"), false},
		"inside protected":       {filepath.Join(home, ".ssh", "keys"), false},
		"symlink to home":        {link, false},
		"symlink into protected": {keysLink, false},
	}
	if upper := strings.ToUpper(home); upper != home {
		if a, err1 := os.Stat(upper); err1 == nil {
			if b, err2 := os.Stat(home); err2 == nil && os.SameFile(a, b) {
				cases["home in other case"] = copyRoot{upper, false} // case-insensitive file system
			}
		}
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := &execFakes{}
			if !c.repo {
				f.notRepo = exitErr(t, 128)
			}
			_, err := Exec(context.Background(), f.opts(c.workdir, io.Discard))
			if err == nil || !strings.Contains(err.Error(), "protects") {
				t.Errorf("Exec from %s = %v, want a refusal", c.workdir, err)
			}
			if f.rsync != nil || len(f.remotes) != 0 {
				t.Errorf("Exec from %s copied: rsync=%q ssh=%q", c.workdir, f.rsync, f.remotes)
			}
		})
	}
	f := &execFakes{notRepo: exitErr(t, 128)}
	if _, err := Exec(context.Background(), f.opts(filepath.Join(home, "proj"), io.Discard)); err != nil {
		t.Errorf("Exec from a project under home = %v, want nil", err)
	}
}

func TestExecRefusesProtectedPathLinkedIntoRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, dotfiles := filepath.Join(base, "home"), filepath.Join(base, "dotfiles")
	writeFiles(t, dotfiles, map[string]string{"ssh/id_ed25519": "x"})
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dotfiles, "ssh"), filepath.Join(home, ".ssh")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	f := &execFakes{notRepo: exitErr(t, 128)}
	if _, err := Exec(context.Background(), f.opts(dotfiles, io.Discard)); err == nil || f.rsync != nil {
		t.Fatalf("Exec from the target of ~/.ssh = %v, rsync %q; want a refusal", err, f.rsync)
	}
}

// Inside the sandbox, stat on a protected path fails; the check must still
// see that the copy root contains it.
func TestExecRefusesUnreadableProtectedPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	writeFiles(t, home, map[string]string{".ssh/id_ed25519": "x"})
	if err := os.Chmod(home, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })
	t.Setenv("HOME", home)
	f := &execFakes{notRepo: exitErr(t, 128)}
	if _, err := Exec(context.Background(), f.opts(home, io.Discard)); err == nil || f.rsync != nil {
		t.Fatalf("Exec from a home whose ~/.ssh cannot be read = %v, rsync %q; want a refusal", err, f.rsync)
	}
}

func TestExecRefusesUnreadableCopyRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	parent := t.TempDir()
	root := filepath.Join(parent, "proj")
	writeFiles(t, root, map[string]string{"a.go": "package a"})
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	f := &execFakes{notRepo: exitErr(t, 128)}
	if _, err := Exec(context.Background(), f.opts(root, io.Discard)); err == nil || f.rsync != nil {
		t.Fatalf("Exec from a root it cannot stat = %v, rsync %q; want a refusal", err, f.rsync)
	}
}

// The agent can replace the copy root with a link to ~/.ssh while the
// registration runs; rsync must still read the directory that was checked.
func TestExecCopiesTheCheckedRootAfterASwap(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, root := filepath.Join(base, "home"), filepath.Join(base, "repo", "svc")
	writeFiles(t, home, map[string]string{".ssh/id_ed25519": "x"})
	writeFiles(t, root, map[string]string{"a.go": "package a"})
	t.Setenv("HOME", home)
	moved := root + ".old"
	f := &execFakes{notRepo: exitErr(t, 128), onRegister: func() {
		if err := os.Rename(root, moved); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(filepath.Join(home, ".ssh"), root); err != nil {
			t.Error(err)
		}
	}}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Exec(context.Background(), f.opts(root, io.Discard)); err != nil {
		t.Fatal(err)
	}
	if f.rsyncWd != moved || f.rsync[len(f.rsync)-2] != "./" {
		t.Fatalf("rsync copied %q from %s, want ./ from the checked directory %s", f.rsync, f.rsyncWd, moved)
	}
	if after, _ := os.Getwd(); after != wd {
		t.Fatalf("working directory after Exec = %s, want %s", after, wd)
	}
}

func TestExecRefusesWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	f := &execFakes{notRepo: exitErr(t, 128)}
	if _, err := Exec(context.Background(), f.opts(testDir(t, "plain"), io.Discard)); err == nil || !strings.Contains(err.Error(), "protects") {
		t.Fatalf("Exec without a home directory = %v, want a refusal about protected paths", err)
	}
	if f.rsync != nil {
		t.Fatalf("copied without knowing the protected paths: %q", f.rsync)
	}
}

// A linked worktree whose .git file is gone resolves to the enclosing
// checkout, which ignores the worktree directory.
func TestExecRefusesDirectoryGitIgnores(t *testing.T) {
	for _, c := range []struct{ prefix, cwd string }{
		{"build/w/", ""},
		{"build/", ""},
		{"", "build/x"},
	} {
		f := &execFakes{top: "/w/repo", prefix: c.prefix}
		o := f.opts("/w/repo/"+c.prefix, io.Discard)
		o.Cwd = c.cwd
		_, err := Exec(context.Background(), o)
		if err == nil || !strings.Contains(err.Error(), "ignores") {
			t.Errorf("Exec in %s%s = %v, want a refusal", c.prefix, c.cwd, err)
		}
		if f.rsync != nil || len(f.remotes) != 0 {
			t.Errorf("Exec in %s%s copied: rsync=%q ssh=%q", c.prefix, c.cwd, f.rsync, f.remotes)
		}
	}
}

func TestExecAllowsNilWriters(t *testing.T) {
	f := &execFakes{notRepo: exitErr(t, 128)}
	o := f.opts(testDir(t, "plain"), nil)
	o.Stderr = nil
	if _, err := Exec(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

func TestExecWarnsOutsideRepo(t *testing.T) {
	f := &execFakes{notRepo: exitErr(t, 128)}
	var stderr bytes.Buffer
	o := f.opts(testDir(t, "plain"), io.Discard)
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
	code, err := Exec(context.Background(), f.opts(testDir(t, "repo"), io.Discard))
	if err != nil || code != 0 {
		t.Fatalf("Exec = %d, %v", code, err)
	}
}

func TestExecRsyncFailureStopsBeforeCommand(t *testing.T) {
	f := &execFakes{rsyncErr: errors.New("boom")}
	_, err := Exec(context.Background(), f.opts(testDir(t, "repo"), io.Discard))
	if err == nil || !strings.Contains(err.Error(), "rsync") {
		t.Fatalf("err = %v, want rsync error", err)
	}
	if len(f.remotes) != 1 {
		t.Fatalf("ssh calls after rsync failure = %q, want only the registration", f.remotes)
	}
}

func TestExecRegistrationFailureStopsBeforeCopy(t *testing.T) {
	f := &execFakes{regErr: exitErr(t, 1)}
	_, err := Exec(context.Background(), f.opts(testDir(t, "repo"), io.Discard))
	if err == nil || !strings.Contains(err.Error(), "register worktree") {
		t.Fatalf("err = %v, want the registration error", err)
	}
	if f.rsync != nil || len(f.remotes) != 1 {
		t.Fatalf("after a failed registration: rsync=%q ssh=%q, want neither copy nor command", f.rsync, f.remotes)
	}
}

func TestExecSSHConnectionFailure(t *testing.T) {
	f := &execFakes{cmdErr: exitErr(t, 255)}
	_, err := Exec(context.Background(), f.opts(testDir(t, "repo"), io.Discard))
	if err == nil || !strings.Contains(err.Error(), "connection lost or remote command killed (is the VM running? `omac vm status`)") {
		t.Fatalf("err = %v, want connection error naming omac vm status", err)
	}
}

func TestExecRsyncConnectionFailure(t *testing.T) {
	for _, code := range []int{255, 12} {
		f := &execFakes{rsyncErr: exitErr(t, code)}
		_, err := Exec(context.Background(), f.opts(testDir(t, "repo"), io.Discard))
		if err == nil || !strings.Contains(err.Error(), "omac vm status") {
			t.Errorf("rsync exit %d: err = %v, want the connection hint", code, err)
		}
	}
}

func TestExecInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Ctrl-C cancels the context; the stopped ssh then has no exit code.
	f := &execFakes{onCmd: cancel, cmdErr: errors.New("signal: killed")}
	code, err := Exec(ctx, f.opts(testDir(t, "repo"), io.Discard))
	if code != 130 || !errors.Is(err, ErrInterrupted) {
		t.Fatalf("Exec = %d, %v; want 130, ErrInterrupted", code, err)
	}
}

func TestExecTTYOnlyForTheCommand(t *testing.T) {
	f := &execFakes{}
	o := f.opts(testDir(t, "repo"), io.Discard)
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
			o := f.opts(testDir(t, "repo"), io.Discard)
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
