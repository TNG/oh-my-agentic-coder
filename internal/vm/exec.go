package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
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

// rsyncLiteral makes name match only itself as an rsync pattern. rsync
// honors backslash escapes only in patterns that contain a wildcard, so a
// name without one stays as is: "a\b" is literal, "foo[1]" → `foo\[1\]`.
func rsyncLiteral(name string) string {
	if !strings.ContainsAny(name, "*?[") {
		return name
	}
	var b strings.Builder
	for _, r := range name {
		if strings.ContainsRune(`*?[]\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
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
	err = run(ctx, "git", []string{"-C", dir, "rev-parse", "--show-toplevel", "--show-prefix"}, nil, &out, &stderr)
	var ee *exec.ExitError
	if errors.As(err, &ee) && strings.Contains(stderr.String(), "not a git repository") {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, gitError("rev-parse", err, &stderr)
	}
	lines := strings.Split(out.String(), "\n")
	if len(lines) < 2 || !filepath.IsAbs(lines[0]) {
		return "", "", false, fmt.Errorf("omac vm exec: unexpected git rev-parse output %q", out.String())
	}
	return lines[0], strings.TrimSuffix(lines[1], "/"), true, nil
}

// IgnoredPaths lists the paths git ignores in the repository at top and in
// its checked-out submodules, relative to top; ignored directories are
// listed once, with a trailing slash.
func IgnoredPaths(ctx context.Context, run CommandRunner, top string) ([]string, error) {
	var out, stderr bytes.Buffer
	foreach := []string{"-C", top, "submodule", "--quiet", "foreach", "--recursive", `echo "$displaypath"`}
	if err := run(ctx, "git", foreach, nil, &out, &stderr); err != nil {
		return nil, gitError("submodule foreach", err, &stderr)
	}
	roots := []string{""}
	for line := range strings.Lines(out.String()) {
		if sub := strings.TrimRight(line, "\r\n"); sub != "" {
			roots = append(roots, sub+"/")
		}
	}
	var paths []string
	for _, root := range roots {
		out.Reset()
		stderr.Reset()
		args := []string{"-C", filepath.Join(top, root), "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z"}
		if err := run(ctx, "git", args, nil, &out, &stderr); err != nil {
			return nil, gitError("ls-files", err, &stderr)
		}
		for _, p := range strings.Split(out.String(), "\x00") {
			// git prints "./" when everything it lists is ignored. In a
			// submodule that excludes the submodule; a bare "./" is dropped.
			if p == "./" && root != "" {
				paths = append(paths, root)
			} else if p != "" && p != "./" {
				paths = append(paths, root+p)
			}
		}
	}
	return paths, nil
}

func gitError(step string, err error, stderr *bytes.Buffer) error {
	return fmt.Errorf("omac vm exec: git %s: %w: %s", step, err, strings.TrimSpace(stderr.String()))
}

// WriteExcludeFile writes an rsync exclude list into dir: ".git" at any
// depth (a directory, or the file a `git worktree` checkout has), the files
// the sandbox hides in every workdir (such as .env) at any depth, then each
// ignored path anchored at the transfer root.
func WriteExcludeFile(dir string, ignored []string) (string, error) {
	var b strings.Builder
	b.WriteString(".git\n")
	for _, name := range sandboxprofile.PlatformBaseline().WorkdirProtected {
		b.WriteString(rsyncLiteral(name) + "\n")
	}
	for _, p := range ignored {
		// rsync reads one pattern per line; a newline in a name cannot be expressed.
		if strings.ContainsAny(p, "\r\n") {
			continue
		}
		b.WriteString("/" + rsyncLiteral(p) + "\n")
	}
	path := filepath.Join(dir, "exclude")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("omac vm exec: write exclude file: %w", err)
	}
	return path, nil
}

// RsyncArgs copies workdir's contents into remoteDir in the VM. It never
// deletes on the remote side, and the flags work with both Apple's
// openrsync and rsync 3.x.
func RsyncArgs(workdir, excludeFile string, t SSHTarget, remoteDir string) []string {
	return []string{
		"-rlpt", "--no-owner", "--no-group", "-z",
		"--exclude-from=" + excludeFile,
		"-e", "ssh " + ShellJoin(SSHArgs(t)),
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
// ("NAME=VALUE") set. Every user-controlled part is single-quoted.
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

// ResolveEnv expands each spec: "NAME" takes the value from getenv and
// must be set; "NAME=VALUE" is passed through.
func ResolveEnv(specs []string, getenv func(string) string) ([]string, error) {
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
		v := getenv(name)
		if v == "" {
			return nil, fmt.Errorf("omac vm exec: environment variable %s is not set", name)
		}
		out = append(out, name+"="+v)
	}
	return out, nil
}

// ExecOpts configures Exec. Workdir must be absolute; Cwd is relative to
// it. Inside a git repository, Exec copies the repository's top level.
type ExecOpts struct {
	Workdir string
	Cwd     string
	Env     []string // NAME=VALUE
	Cmd     []string
	Conn    ConnInfo
	Run     CommandRunner
	SSH     SSHRunner
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Log     func(string, ...any)
}

// Exec records the copy in ~/omac/.worktrees, copies the git top level of
// Workdir (or Workdir outside a repository) into the VM (one way, never
// deleting there), and runs Cmd in it. It returns the remote command's
// exit code; err is set only when omac itself failed.
func Exec(ctx context.Context, o ExecOpts) (int, error) {
	logf := o.Log
	if logf == nil {
		logf = func(string, ...any) {}
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
	// One slug and one registry line per directory, however it was spelled.
	root := filepath.Clean(o.Workdir)
	cwd := o.Cwd
	top, prefix, isRepo, err := RepoRoot(ctx, o.Run, root)
	if err != nil {
		return 1, err
	}
	var ignored []string
	if isRepo {
		root, cwd = top, path.Join(prefix, cwd)
		if ignored, err = IgnoredPaths(ctx, o.Run, top); err != nil {
			return 1, err
		}
	} else {
		fmt.Fprintf(o.Stderr, "omac vm exec: %s is not a git work tree; copying everything except .git and .env files\n", root)
	}
	if strings.ContainsAny(root, "\r\n") {
		return 1, fmt.Errorf("omac vm exec: copy root %q contains a newline", root)
	}
	slug := WorktreeSlug(root)
	remoteDir := RemoteDir(slug)
	remote, err := RemoteCommand(remoteDir, cwd, o.Env, o.Cmd)
	if err != nil {
		return 1, err
	}

	tmp, err := os.MkdirTemp("", "omac-vm-exec-")
	if err != nil {
		return 1, fmt.Errorf("omac vm exec: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)
	excludeFile, err := WriteExcludeFile(tmp, ignored)
	if err != nil {
		return 1, err
	}

	if err := o.SSH(ctx, o.Conn.Target, nil, io.Discard, o.Stderr, registerCmd(slug, root)); err != nil {
		return 1, sshError("register worktree", err)
	}
	logf("omac vm exec: copying %s to the VM (%s)", root, remoteDir)
	if err := o.Run(ctx, "rsync", RsyncArgs(root, excludeFile, o.Conn.Target, remoteDir), nil, o.Stderr, o.Stderr); err != nil {
		return 1, fmt.Errorf("omac vm exec: rsync to the VM failed: %w", err)
	}

	err = o.SSH(ctx, o.Conn.Target, o.Stdin, o.Stdout, o.Stderr, remote)
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() != 255 {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return 1, sshError("run command", err)
	}
	return 0, nil
}

// registerCmd creates the copy's directory, which the VM's rsync cannot
// do for a missing parent, and appends "<slug> <workdir>" to the registry
// that `omac vm status` lists, unless the line is there already.
func registerCmd(slug, workdir string) string {
	entry := ShellQuote(slug + " " + workdir)
	return "mkdir -p -- " + ShellQuote("omac/"+slug) +
		" && { grep -qxF -- " + entry + " omac/.worktrees 2>/dev/null || printf '%s\\n' " + entry + " >> omac/.worktrees; }"
}

// sshError maps ssh's exit status 255 (connection failure) to a hint.
func sshError(step string, err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 255 {
		return errors.New("omac vm exec: ssh connection failed (is the VM running? `omac vm status`)")
	}
	return fmt.Errorf("omac vm exec: %s: %w", step, err)
}
