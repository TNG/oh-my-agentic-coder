package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// WorktreeSlug names the copy of absWorkdir in the VM: the sanitized
// basename plus a hash of the full path, so two checkouts with the same
// name never share a directory. Example: "/Users/a/My Repo" → "my-repo-1a2b3c4d".
func WorktreeSlug(absWorkdir string) string {
	absWorkdir = filepath.Clean(absWorkdir)
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		}
		return '-'
	}, filepath.Base(absWorkdir))
	base = strings.Trim(base, "-.")
	if base == "" {
		base = "worktree"
	}
	sum := sha256.Sum256([]byte(absWorkdir))
	return base + "-" + hex.EncodeToString(sum[:4])
}

// rsyncPattern turns name into one line of an rsync exclude list that
// matches name literally. rsync honors backslash escapes only in patterns
// that contain a wildcard, so a name without one stays as is: "a\b" is
// literal, "foo[1]" → `foo\[1\]`. A line cannot hold a line break, so
// "?" stands in for it and also matches names that have any other
// character there: "a\nb" → "a?b".
func rsyncPattern(name string) string {
	if !strings.ContainsAny(name, "*?[\r\n") {
		return name
	}
	var b strings.Builder
	for i := range len(name) { // bytes: file names need not be UTF-8
		c := name[i]
		switch {
		case c == '\r' || c == '\n':
			c = '?'
		case strings.IndexByte(`*?[]\`, c) >= 0:
			b.WriteByte('\\')
		}
		b.WriteByte(c)
	}
	return b.String()
}

// RemoteDir is the copy's directory, relative to the agent user's home.
func RemoteDir(slug string) string {
	return "omac/" + slug + "/"
}

// RepoRoot finds the git top level of dir and dir's path relative to it
// ("" at the top level). ok is false when dir is not inside a git work
// tree. Any other git failure, such as "detected dubious ownership", is an
// error: copying everything could leak what .gitignore keeps out.
func RepoRoot(ctx context.Context, run CommandRunner, dir string) (top, prefix string, ok bool, err error) {
	var out, stderr bytes.Buffer
	err = runGit(ctx, run, dir, &out, &stderr, "rev-parse", "--show-toplevel", "--show-prefix")
	var ee *exec.ExitError
	if errors.As(err, &ee) && strings.Contains(stderr.String(), "not a git repository") {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, gitError("rev-parse", err, &stderr)
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) != 3 || lines[2] != "" || !filepath.IsAbs(lines[0]) {
		return "", "", false, fmt.Errorf("omac vm exec: unexpected git rev-parse output %q", out.String())
	}
	top = lines[0]
	if err := checkTopLevel(dir, top); err != nil {
		return "", "", false, err
	}
	return top, strings.TrimSuffix(lines[1], "/"), true, nil
}

// checkTopLevel refuses a top level that is not the directory holding the
// .git entry nearest to dir: core.worktree in a config the sandbox can
// write would otherwise make omac copy another directory. Without any
// .git entry, the user's own environment (GIT_DIR) chose the repository.
func checkTopLevel(dir, top string) error {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("omac vm exec: %w", err)
	}
	for d := dir; ; d = filepath.Dir(d) {
		_, err := os.Lstat(filepath.Join(d, ".git"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("omac vm exec: %w", err)
		}
		if err == nil {
			a, errA := os.Stat(d)
			b, errB := os.Stat(top)
			if errA != nil || errB != nil || !os.SameFile(a, b) {
				return fmt.Errorf("omac vm exec: git reports %s as the top level, but the repository is at %s (core.worktree?)", top, d)
			}
			return nil
		}
		if filepath.Dir(d) == d {
			return nil
		}
	}
}

// runGit runs every git command of exec, in dir. The sandbox can write git
// config in the worktree, so settings that make git run a command are off:
// core.fsmonitor, and every transport, which a planted partial-clone
// config uses to fetch a missing object through core.sshCommand
// (GIT_NO_LAZY_FETCH needs git 2.45; protocol.allow=never also stops older
// git). Repository-local keys that run a command without a transport need
// their own entry here. Messages are in English, which RepoRoot matches
// on, and pathspecs are literal, so a directory named "sv*" matches only
// itself.
func runGit(ctx context.Context, run CommandRunner, dir string, stdout, stderr io.Writer, args ...string) error {
	ctx = withCommandEnv(ctx, "LC_ALL=C", "GIT_LITERAL_PATHSPECS=1", "GIT_NO_LAZY_FETCH=1")
	hardening := []string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "protocol.allow=never"}
	return run(ctx, "git", append(hardening, args...), nil, stdout, stderr)
}

// IgnoredPaths lists the paths git ignores in the repository at top and in
// its checked-out submodules, relative to top; ignored directories are
// listed once, with a trailing slash. Git does not run in an untracked
// clone inside top, so the clone's own ignore rules are not applied.
func IgnoredPaths(ctx context.Context, run CommandRunner, top string) ([]string, error) {
	var paths []string
	roots := []string{""} // repositories to read, relative to top: "", "libs/lib/"
	for len(roots) > 0 {
		root := roots[0]
		roots = roots[1:]
		dir := filepath.Join(top, root)
		ignored, err := gitOutput(ctx, run, dir, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(ignored, "\x00") {
			// git prints "./" when everything under dir is ignored: the
			// whole nested repository is excluded, the top level is not.
			if p == "./" && root != "" {
				paths = append(paths, root)
			} else if p != "" && p != "./" {
				paths = append(paths, root+p)
			}
		}
		subs, err := gitOutput(ctx, run, dir, "submodule", "--quiet", "foreach", `printf '%s\0' "$displaypath"`)
		if err != nil {
			return nil, err
		}
		for _, sub := range strings.Split(subs, "\x00") {
			if sub != "" {
				roots = append(roots, root+sub+"/")
			}
		}
	}
	return paths, nil
}

func gitOutput(ctx context.Context, run CommandRunner, dir string, args ...string) (string, error) {
	var out, stderr bytes.Buffer
	if err := runGit(ctx, run, dir, &out, &stderr, args...); err != nil {
		return "", gitError(args[0], err, &stderr)
	}
	return out.String(), nil
}

func gitError(step string, err error, stderr *bytes.Buffer) error {
	return fmt.Errorf("omac vm exec: git %s: %w: %s", step, err, strings.TrimSpace(stderr.String()))
}

// ExcludeList is the rsync exclude list, one pattern per line: ".git" at
// any depth (a directory, or the file a `git worktree` checkout has), the
// files the sandbox hides in every workdir (such as .env) at any depth,
// then each ignored path anchored at the transfer root.
func ExcludeList(ignored []string) string {
	var b strings.Builder
	b.WriteString(".git\n")
	for _, name := range sandboxprofile.PlatformBaseline().WorkdirProtected {
		b.WriteString(rsyncPattern(name) + "\n")
	}
	for _, p := range ignored {
		b.WriteString("/" + rsyncPattern(p) + "\n")
	}
	return b.String()
}

// RsyncArgs are the rsync arguments that copy workdir's contents into
// remoteDir in the VM through the ssh binary at sshPath, reading the
// ExcludeList from stdin: a file in the temp directory, which the sandbox
// can write, could be emptied before rsync reads it. They never delete on
// the remote side and work with both Apple's openrsync and rsync 3.x.
func RsyncArgs(sshPath, workdir string, t SSHTarget, remoteDir string) []string {
	t.TTY = false // a terminal would garble rsync's protocol stream
	return []string{
		"-rlpt", "--no-owner", "--no-group", "-z",
		"--exclude-from=-",
		"-e", ShellJoin(append([]string{sshPath}, SSHArgs(t)...)),
		strings.TrimRight(workdir, "/") + "/",
		t.User + "@127.0.0.1:" + remoteDir,
	}
}

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateCwd accepts a working directory relative to the copy that does
// not leave it.
func ValidateCwd(cwd string) error {
	if strings.HasPrefix(cwd, "/") {
		return fmt.Errorf("omac vm exec: working directory %q must be relative to the worktree", cwd)
	}
	for _, part := range strings.Split(cwd, "/") {
		if part == ".." {
			return fmt.Errorf("omac vm exec: working directory %q leaves the worktree", cwd)
		}
	}
	return nil
}

// RemoteCommand is the shell line that runs cmd in remoteDir/cwd with envs
// ("NAME=VALUE") set. The directory and the values are single-quoted,
// command words are quoted where needed, and names must be shell
// identifiers.
func RemoteCommand(remoteDir, cwd string, envs []string, cmd []string) (string, error) {
	if len(cmd) == 0 {
		return "", errors.New("omac vm exec: no command given")
	}
	if err := ValidateCwd(cwd); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("cd " + ShellQuote(path.Join(remoteDir, cwd)) + " && exec ")
	if len(envs) > 0 {
		b.WriteString("env ")
		for _, e := range envs {
			name, value, ok := strings.Cut(e, "=")
			if !ok || !envNameRE.MatchString(name) {
				return "", fmt.Errorf("omac vm exec: invalid environment entry %q, want NAME=VALUE", e)
			}
			b.WriteString(name + "=" + ShellQuote(value) + " ")
		}
	}
	b.WriteString(ShellJoin(cmd))
	return b.String(), nil
}

// ResolveEnv expands each spec: "NAME" takes the value from lookupEnv, such
// as os.LookupEnv, and must be set, possibly to ""; "NAME=VALUE" is passed
// through.
func ResolveEnv(specs []string, lookupEnv func(string) (string, bool)) ([]string, error) {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		name, _, hasValue := strings.Cut(s, "=")
		if !envNameRE.MatchString(name) {
			return nil, fmt.Errorf("omac vm exec: invalid environment variable name %q", name)
		}
		if hasValue {
			out = append(out, s)
			continue
		}
		v, ok := lookupEnv(name)
		if !ok {
			return nil, fmt.Errorf("omac vm exec: environment variable %s is not set", name)
		}
		out = append(out, name+"="+v)
	}
	return out, nil
}

// ErrInterrupted reports that Exec stopped because its context was
// cancelled, typically by Ctrl-C.
var ErrInterrupted = errors.New("omac vm exec: interrupted")

// ExecOpts configures Exec. Workdir must be absolute; Cwd is relative to it.
type ExecOpts struct {
	Workdir string
	Cwd     string
	Env     []string // NAME=VALUE
	Cmd     []string
	Conn    ConnInfo
	Run     CommandRunner
	SSH     SSHRunner
	TTY     bool // run the command with a terminal in the VM
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Log     func(string, ...any)
}

// Exec records the copy in ~/omac/.worktrees, copies the git top level of
// Workdir (or Workdir outside a repository) into the VM (one way, never
// deleting there), and runs Cmd in it. It refuses a copy that overlaps a
// path the sandbox protects, and a Workdir below the top level in which
// git tracks no file. While rsync runs, the process's working
// directory is the copy root. It returns the remote command's exit code;
// err is set only when omac itself failed.
func Exec(ctx context.Context, o ExecOpts) (int, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if !filepath.IsAbs(o.Workdir) || strings.ContainsAny(o.Workdir, "\r\n") {
		return 1, fmt.Errorf("omac vm exec: workdir %q must be an absolute path without newlines", o.Workdir)
	}
	if len(o.Cmd) == 0 {
		return 1, errors.New("omac vm exec: no command given")
	}
	if err := ValidateCwd(o.Cwd); err != nil {
		return 1, err
	}
	// rsync runs in the copy root, where the agent picks the files, and
	// would find a bare "ssh" through a relative PATH entry such as
	// node_modules/.bin.
	sshPath, err := exec.LookPath("ssh")
	if err != nil || !filepath.IsAbs(sshPath) {
		return 1, fmt.Errorf("omac vm exec: no ssh at an absolute path in PATH (%q, %v)", sshPath, err)
	}
	for _, p := range []string{sshPath, o.Conn.Target.KeyPath, o.Conn.Target.KnownHosts} {
		// rsync's -e parser does not understand the '\'' that ShellQuote emits.
		if strings.Contains(p, "'") {
			return 1, fmt.Errorf("omac vm exec: %q contains a single quote, which rsync cannot pass to ssh", p)
		}
		// rsync's ssh starts in the copy root, where the agent picks the files.
		if p != "" && !filepath.IsAbs(p) {
			return 1, fmt.Errorf("omac vm exec: ssh file %q must be an absolute path", p)
		}
	}
	// "repo/" and "repo" get the same slug and registry line.
	root := filepath.Clean(o.Workdir)
	workdirInfo, err := os.Stat(root)
	if err != nil {
		return 1, fmt.Errorf("omac vm exec: %w", err)
	}
	cwd := o.Cwd
	top, prefix, isRepo, err := RepoRoot(ctx, o.Run, root)
	if err != nil {
		return 1, err
	}
	var ignored []string
	if isRepo {
		root, cwd = top, path.Join(prefix, cwd)
		if err := refuseUntrackedPrefix(ctx, o.Run, top, prefix); err != nil {
			return 1, err
		}
		if ignored, err = IgnoredPaths(ctx, o.Run, top); err != nil {
			return 1, err
		}
	}
	if strings.ContainsAny(root, "\r\n") {
		return 1, fmt.Errorf("omac vm exec: copy root %q contains a newline", root)
	}
	rootDir, err := openCopyRoot(root)
	if err != nil {
		return 1, err
	}
	defer rootDir.Close()
	// git read the workdir by path; the agent may have swapped it since.
	err = inDir(rootDir, func() error {
		if fi, err := os.Stat(path.Join(".", prefix)); err != nil || !os.SameFile(fi, workdirInfo) {
			return fmt.Errorf("omac vm exec: %s changed while omac read it", o.Workdir)
		}
		return nil
	})
	if err != nil {
		return 1, err
	}
	if !isRepo {
		fmt.Fprintf(o.Stderr, "omac vm exec: %s is not a git work tree; copying everything except .git and the files the sandbox hides, such as .env\n", root)
	}
	slug := WorktreeSlug(root)
	remoteDir := RemoteDir(slug)
	remote, err := RemoteCommand(remoteDir, cwd, o.Env, o.Cmd)
	if err != nil {
		return 1, err
	}

	target := o.Conn.Target
	target.TTY = false
	if err := o.SSH(ctx, target, nil, io.Discard, o.Stderr, registerCmd(slug, root)); err != nil {
		return failure(ctx, sshError("register worktree", err))
	}
	logf("omac vm exec: copying %s to the VM (%s)", root, remoteDir)
	excludes := strings.NewReader(ExcludeList(ignored))
	err = inDir(rootDir, func() error {
		return o.Run(ctx, "rsync", RsyncArgs(sshPath, ".", target, remoteDir), excludes, o.Stderr, o.Stderr)
	})
	if err != nil {
		if exitCode(err) == 255 || exitCode(err) == 12 { // ssh failed, or the stream broke
			err = fmt.Errorf("%w; %s", err, connectionHint)
		}
		return failure(ctx, fmt.Errorf("omac vm exec: rsync to the VM failed: %w", err))
	}

	target.TTY = o.TTY
	err = o.SSH(ctx, target, o.Stdin, o.Stdout, o.Stderr, remote)
	if ctx.Err() != nil {
		return 130, ErrInterrupted
	}
	if code := exitCode(err); code > 0 && code != 255 {
		return code, nil
	}
	if err != nil {
		return 1, sshError("run command", err)
	}
	return 0, nil
}

// refuseUntrackedPrefix fails when the repository at top tracks no file in
// prefix, the workdir's path below top. That usually means a linked
// worktree lost its .git file and git found the enclosing checkout, whose
// index the sandbox cannot write.
func refuseUntrackedPrefix(ctx context.Context, run CommandRunner, top, prefix string) error {
	if prefix == "" {
		return nil
	}
	tracked, err := gitOutput(ctx, run, top, "ls-files", "-z", "--", prefix)
	if err != nil {
		return err
	}
	if tracked == "" {
		return fmt.Errorf("omac vm exec: git in %s tracks nothing in %s; if it is a linked worktree, restore its .git file; if it is a new directory, run from %s with --cwd %s", top, prefix, top, prefix)
	}
	return nil
}

// openCopyRoot opens root for rsync to copy from and applies
// refuseProtectedOverlap to the opened directory, so swapping root for a
// symlink afterwards does not change what is copied. Directories below
// root are read by path and stay open to such a swap.
func openCopyRoot(root string) (*os.File, error) {
	protected := sandboxprofile.EffectiveProtectedPaths(sandboxprofile.PlatformBaseline(), nil)
	for _, p := range protected {
		if !filepath.IsAbs(p) { // "~/.ssh" left unexpanded: no home directory
			return nil, fmt.Errorf("omac vm exec: cannot tell which paths the sandbox protects: %q does not expand", p)
		}
	}
	dir, err := os.Open(root)
	if err != nil {
		return nil, fmt.Errorf("omac vm exec: %w", err)
	}
	if err := refuseProtectedOverlap(root, dir, protected); err != nil {
		dir.Close()
		return nil, err
	}
	return dir, nil
}

// refuseProtectedOverlap fails when dir, opened from root, contains or lies
// in a protected path, such as ~/.ssh or the VM's admin key: every --vm
// session can read the copy. Paths are compared by file identity, so a
// symlink or another spelling of the same directory counts too. A
// protected path that cannot be stat'ed, as inside the sandbox, is still
// found through its parents, unless it is a symlink.
func refuseProtectedOverlap(root string, dir *os.File, protected []string) error {
	rootInfo, err := dir.Stat()
	if err != nil {
		return fmt.Errorf("omac vm exec: %w", err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("omac vm exec: %s is not a directory", root)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("omac vm exec: %w", err)
	}
	if fi, err := os.Stat(realRoot); err != nil || !os.SameFile(fi, rootInfo) {
		return fmt.Errorf("omac vm exec: %s changed while omac checked it", root)
	}
	for _, p := range protected {
		forms := []string{p}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			forms = append(forms, real)
		}
		overlaps := false
		for _, f := range forms {
			overlaps = overlaps || hasAncestor(f, rootInfo)
		}
		if pInfo, err := os.Stat(p); err == nil {
			overlaps = overlaps || hasAncestor(root, pInfo) || hasAncestor(realRoot, pInfo)
		}
		if overlaps {
			return fmt.Errorf("omac vm exec: the copy root %s overlaps %s, which the sandbox protects; run it from a project that is its own git repository", root, p)
		}
	}
	return nil
}

// inDir runs fn with the process's working directory set to dir, so that
// a command fn starts reads "." from dir however its path changes. The
// working directory is shared by all goroutines, so nothing may resolve a
// relative path while fn runs.
func inDir(dir *os.File, fn func() error) (err error) {
	restore, err := saveWorkingDir()
	if err != nil {
		return fmt.Errorf("omac vm exec: %w", err)
	}
	if err := dir.Chdir(); err != nil {
		return errors.Join(fmt.Errorf("omac vm exec: %w", err), restore())
	}
	defer func() {
		if rerr := restore(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("omac vm exec: %w", rerr))
		}
	}()
	return fn()
}

// saveWorkingDir returns a func that changes back to the current working
// directory: through a handle, which also works after the directory was
// renamed or deleted, or by path when the directory cannot be opened.
func saveWorkingDir() (func() error, error) {
	if h, err := os.Open("."); err == nil {
		return func() error {
			defer h.Close()
			return h.Chdir()
		}, nil
	}
	prev, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return func() error { return os.Chdir(prev) }, nil
}

// hasAncestor reports whether path or one of its parents is dir.
func hasAncestor(path string, dir os.FileInfo) bool {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, dir) {
			return true
		}
		if filepath.Dir(p) == p {
			return false
		}
	}
}

const connectionHint = "connection lost or remote command killed (is the VM running? `omac vm status`)"

// failure reports err, or an interruption when the context was cancelled.
func failure(ctx context.Context, err error) (int, error) {
	if ctx.Err() != nil {
		return 130, ErrInterrupted
	}
	return 1, err
}

// exitCode is the exit status of a process that ran and exited, else -1.
func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// registerCmd creates the copy's directory, which the VM's rsync cannot
// do for a missing parent, and appends "<slug> <workdir>" to the registry
// that `omac vm status` lists, unless the line is there already.
func registerCmd(slug, workdir string) string {
	entry := ShellQuote(slug + " " + workdir)
	return "mkdir -p -- " + ShellQuote("omac/"+slug) +
		" && { grep -qxF -- " + entry + " omac/.worktrees 2>/dev/null || printf '%s\\n' " + entry + " >> omac/.worktrees; }"
}

// sshError maps ssh's exit status 255 to a hint. ssh exits 255 when the
// connection fails and also when the remote command dies from a signal.
func sshError(step string, err error) error {
	if exitCode(err) == 255 {
		return errors.New("omac vm exec: " + connectionHint)
	}
	return fmt.Errorf("omac vm exec: %s: %w", step, err)
}
