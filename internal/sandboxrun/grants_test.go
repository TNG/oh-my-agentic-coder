package sandboxrun

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestResolveGrantsWorkdirLevels(t *testing.T) {
	wd := t.TempDir()
	cases := map[string]func(*Grants) []string{
		sandboxprofile.AccessRead:      func(g *Grants) []string { return g.ReadPaths },
		sandboxprofile.AccessWrite:     func(g *Grants) []string { return g.WritePaths },
		sandboxprofile.AccessReadWrite: func(g *Grants) []string { return g.AllowPaths },
	}
	for access, pick := range cases {
		p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: access}}
		g, err := ResolveGrants(p, wd, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(pick(g), wd) {
			t.Errorf("access=%s: workdir not in expected grant list", access)
		}
	}
	// none: workdir in no list.
	p := &sandboxprofile.Profile{}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range [][]string{g.ReadPaths, g.WritePaths, g.AllowPaths} {
		if slices.Contains(list, wd) {
			t.Error("access=none: workdir must not be granted")
		}
	}
}

func TestResolveGrantsBaselineIncluded(t *testing.T) {
	g, err := ResolveGrants(&sandboxprofile.Profile{}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// /tmp (or /private/tmp on macOS) must be writable from the baseline.
	hasTmp := false
	for _, p := range g.WritePaths {
		if p == "/tmp" || p == "/private/tmp" {
			hasTmp = true
		}
	}
	if !hasTmp {
		t.Errorf("baseline temp write missing: %v", g.WritePaths)
	}
	// Protected paths populated even with an empty profile.
	hasSSH := false
	for _, p := range g.ProtectedPaths {
		if strings.HasSuffix(p, "/.ssh") {
			hasSSH = true
		}
	}
	if !hasSSH {
		t.Errorf("protected paths missing ~/.ssh: %d entries", len(g.ProtectedPaths))
	}
}

func TestResolveGrantsOverrideDeny(t *testing.T) {
	p := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{
			OverrideDeny: []string{"~/.git-credentials"},
		},
	}
	g, err := ResolveGrants(p, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, prot := range g.ProtectedPaths {
		if strings.HasSuffix(prot, "/.git-credentials") {
			t.Error("override_deny hole not punched")
		}
	}
	// Other protected entries survive.
	found := false
	for _, prot := range g.ProtectedPaths {
		if strings.HasSuffix(prot, "/.netrc") {
			found = true
		}
	}
	if !found {
		t.Error("unrelated protected paths must remain")
	}
}

func TestResolveGrantsDetectsUnixSockets(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "bridge.sock")
	// Create a real unix socket.
	l, err := listenUnix(sock)
	if err != nil {
		t.Skipf("cannot create unix socket: %v", err)
	}
	defer l.Close()

	p := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{Allow: []string{sock}},
	}
	g, err := ResolveGrants(p, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.UnixSockets, sock) {
		t.Errorf("unix socket not detected: %v", g.UnixSockets)
	}
}

func TestResolveGrantsUnixSocketDir(t *testing.T) {
	dir := t.TempDir() // stands in for the daemon socket dir
	p := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{AllowUnixDir: []string{dir}},
	}
	g, err := ResolveGrants(p, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.UnixSocketDirs, dir) {
		t.Errorf("AllowUnixDir not resolved into UnixSocketDirs: %v", g.UnixSocketDirs)
	}
	// File access must come along so the socket files can be opened.
	if !slices.Contains(g.AllowPaths, dir) {
		t.Errorf("AllowUnixDir must also grant file access: %v", g.AllowPaths)
	}
}

// The grant must survive even when the dir does not exist at launch: the
// daemon may create it later and Seatbelt matches subpaths at syscall time.
// (Contrast --allow / --read, which existence-filter their paths.)
func TestResolveGrantsUnixSocketDirNonexistent(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "cc-daemon-502")
	p := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{AllowUnixDir: []string{missing}},
	}
	g, err := ResolveGrants(p, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.UnixSocketDirs, missing) {
		t.Errorf("nonexistent unix dir must still be granted: %v", g.UnixSocketDirs)
	}
}

func TestResolveGrantsSkipsMissingProfilePaths(t *testing.T) {
	var notices strings.Builder
	p := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{
			Read: []string{filepath.Join(t.TempDir(), "missing")},
		},
	}
	if _, err := ResolveGrants(p, t.TempDir(), &notices); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notices.String(), "skipping nonexistent path") {
		t.Errorf("notice missing: %q", notices.String())
	}
}

func TestResolveGrantsDeduplicates(t *testing.T) {
	dir := t.TempDir()
	p := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{Read: []string{dir, dir}},
	}
	g, err := ResolveGrants(p, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, r := range g.ReadPaths {
		if r == dir {
			count++
		}
	}
	if count != 1 {
		t.Errorf("dir appears %d times", count)
	}
}

func listenUnix(path string) (interface{ Close() error }, error) {
	return net.Listen("unix", path)
}

// makeLinkedWorktree builds a minimal linked-worktree layout mirroring
// `git worktree add`: .git is a "gitdir: <admin>" file, and <admin>/commondir
// points back at the shared common dir.
func makeLinkedWorktree(t *testing.T) (workdir, common string) {
	t.Helper()
	base := t.TempDir()
	common = filepath.Join(base, "repo", ".git")
	admin := filepath.Join(common, "worktrees", "wt")
	for _, d := range []string{
		filepath.Join(common, "objects"),
		filepath.Join(common, "refs"),
		filepath.Join(common, "logs"),
		filepath.Join(common, "info"),
		filepath.Join(common, "hooks"),
		admin,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(common, "config"))
	writeFile(t, filepath.Join(common, "packed-refs"))
	// commondir is relative to the admin dir, as git writes it.
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	workdir = filepath.Join(base, "wt")
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, ".git"), []byte("gitdir: "+admin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return workdir, common
}

func TestResolveGrantsLinkedWorktreeReadWrite(t *testing.T) {
	wd, common := makeLinkedWorktree(t)
	p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The subdirs git add/commit actually write are read+write.
	for _, sub := range []string{"objects", "refs", "logs", filepath.Join("worktrees", "wt")} {
		want := filepath.Join(common, sub)
		if !slices.Contains(g.AllowPaths, want) {
			t.Errorf("allow missing %s: %v", want, g.AllowPaths)
		}
	}
	// config/info/packed-refs are read-only (blocks core.hooksPath/credential.helper
	// mutation; add/commit only writes loose refs, never packed-refs).
	for _, sub := range []string{"config", "info", "packed-refs"} {
		want := filepath.Join(common, sub)
		if !slices.Contains(g.ReadPaths, want) {
			t.Errorf("read missing %s: %v", want, g.ReadPaths)
		}
		if slices.Contains(g.AllowPaths, want) {
			t.Errorf("%s must not be writable", want)
		}
	}
	// hooks: readable+runnable, never writable, never hard-denied.
	hooks := filepath.Join(common, "hooks")
	if !slices.Contains(g.ReadPaths, hooks) {
		t.Errorf("hooks must be readable: %v", g.ReadPaths)
	}
	if slices.Contains(g.AllowPaths, hooks) || slices.Contains(g.WritePaths, hooks) {
		t.Errorf("hooks must not be writable: allow=%v write=%v", g.AllowPaths, g.WritePaths)
	}
	if slices.Contains(g.ProtectedPaths, hooks) {
		t.Errorf("hooks must not be hard-denied: %v", g.ProtectedPaths)
	}
}

func TestResolveGrantsLinkedWorktreeReadOnly(t *testing.T) {
	wd, common := makeLinkedWorktree(t)
	p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessRead}}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Read-only workdir: git read ops still need the common dir.
	for _, sub := range []string{"objects", "refs", "logs", "config", "info", "packed-refs", filepath.Join("worktrees", "wt")} {
		want := filepath.Join(common, sub)
		if !slices.Contains(g.ReadPaths, want) {
			t.Errorf("read missing %s: %v", want, g.ReadPaths)
		}
	}
	for _, ap := range g.AllowPaths {
		if strings.HasPrefix(ap, common) {
			t.Errorf("read-only workdir must not grant writes under common: %s", ap)
		}
	}
}

func TestResolveGrantsPlainCloneNoWorktreeGrants(t *testing.T) {
	wd := t.TempDir()
	// Plain clone: .git is a directory inside the workdir, already covered.
	if err := os.MkdirAll(filepath.Join(wd, ".git", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ap := range g.AllowPaths {
		if ap != wd {
			t.Errorf("plain clone must not add extra allow grants, got %s", ap)
		}
	}
}

// TestResolveGrantsRealGitWorktreePipeline drives resolve -> backend-generation
// against a real `git worktree add`, catching any drift from git's actual
// .git/commondir format. (A live sandbox-exec/bwrap launch can't run here, so
// this is the end-to-end check below the process boundary.)
func TestResolveGrantsRealGitWorktreePipeline(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	mainRepo := filepath.Join(base, "main")
	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t.t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t.t")
		if out, cerr := cmd.CombinedOutput(); cerr != nil {
			t.Fatalf("git %v: %v\n%s", args, cerr, out)
		}
	}
	run(base, "init", "-q", mainRepo)
	writeFile(t, filepath.Join(mainRepo, "a.txt"))
	run(mainRepo, "add", "a.txt")
	run(mainRepo, "commit", "-qm", "init")
	wt := filepath.Join(base, "wt")
	run(mainRepo, "worktree", "add", "-q", wt, "-b", "feature")

	// git writes canonical paths; ResolveGrants canonicalizes grants too,
	// so compare against the resolved common dir (/var -> /private/var on macOS).
	common := filepath.Join(mainRepo, ".git")
	if resolved, rerr := filepath.EvalSymlinks(common); rerr == nil {
		common = resolved
	}
	p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}
	g, err := ResolveGrants(p, wt, nil)
	if err != nil {
		t.Fatal(err)
	}

	admin := filepath.Join(common, "worktrees", "wt")
	for _, want := range []string{
		filepath.Join(common, "objects"),
		filepath.Join(common, "refs"),
		filepath.Join(common, "logs"),
		admin,
	} {
		if !slices.Contains(g.AllowPaths, want) {
			t.Errorf("allow missing %s: %v", want, g.AllowPaths)
		}
	}
	if !slices.Contains(g.ReadPaths, filepath.Join(common, "config")) {
		t.Errorf("read missing config: %v", g.ReadPaths)
	}
	if !slices.Contains(g.ReadPaths, filepath.Join(common, "hooks")) {
		t.Errorf("hooks must be readable: %v", g.ReadPaths)
	}
	if slices.Contains(g.ProtectedPaths, filepath.Join(common, "hooks")) {
		t.Errorf("hooks must not be hard-denied: %v", g.ProtectedPaths)
	}

	// macOS backend: objects rw, config read-only, hooks read-only.
	sbpl := GenerateSBPL(g)
	objects := filepath.Join(common, "objects")
	if !strings.Contains(sbpl, "(allow file-write* (subpath \""+objects+"\"))") {
		t.Errorf("SBPL missing write allow for objects:\n%s", sbpl)
	}
	config := filepath.Join(common, "config")
	if strings.Contains(sbpl, "(allow file-write* (subpath \""+config+"\"))") {
		t.Errorf("SBPL must not allow writing config")
	}
	hooks := filepath.Join(common, "hooks")
	if !strings.Contains(sbpl, "(allow file-read* (subpath \""+hooks+"\"))") {
		t.Errorf("SBPL missing hooks read allow:\n%s", sbpl)
	}
	if strings.Contains(sbpl, "(allow file-write* (subpath \""+hooks+"\"))") {
		t.Errorf("SBPL must not allow writing hooks:\n%s", sbpl)
	}
	if strings.Contains(sbpl, "(deny file-read* (subpath \""+hooks+"\"))") {
		t.Errorf("SBPL must not deny reading hooks (read-only, not hard-deny):\n%s", sbpl)
	}

	// Linux backend: objects bound rw (--bind), config read-only (--ro-bind).
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--bind "+objects+" "+objects) {
		t.Errorf("bwrap argv missing rw bind for objects:\n%s", joined)
	}
	if !strings.Contains(joined, "--ro-bind "+config+" "+config) {
		t.Errorf("bwrap argv missing ro bind for config:\n%s", joined)
	}
	if !strings.Contains(joined, "--ro-bind "+hooks+" "+hooks) {
		t.Errorf("bwrap argv missing ro bind for hooks:\n%s", joined)
	}
}

// TestResolveGrantsWorktreeHooksRunnableNotWritable pins the hooks policy
// (readable+runnable, never writable, never hard-denied) at both access levels.
func TestResolveGrantsWorktreeHooksRunnableNotWritable(t *testing.T) {
	wd, common := makeLinkedWorktree(t)
	hooks := filepath.Join(common, "hooks")
	for _, access := range []string{sandboxprofile.AccessReadWrite, sandboxprofile.AccessRead} {
		p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: access}}
		g, err := ResolveGrants(p, wd, nil)
		if err != nil {
			t.Fatalf("access %s: %v", access, err)
		}
		if !slices.Contains(g.ReadPaths, hooks) {
			t.Errorf("access %s: hooks must be readable: %v", access, g.ReadPaths)
		}
		if slices.Contains(g.AllowPaths, hooks) || slices.Contains(g.WritePaths, hooks) {
			t.Errorf("access %s: hooks must not be writable (allow=%v write=%v)", access, g.AllowPaths, g.WritePaths)
		}
		if slices.Contains(g.ProtectedPaths, hooks) {
			t.Errorf("access %s: hooks must not be hard-denied: %v", access, g.ProtectedPaths)
		}
	}
}

// TestResolveGrantsWorktreeHooksSymlinkEscape guards the hooks READ grant:
// a planted `hooks -> <secret>` symlink must be dropped by containment,
// not canonicalized into a read of the target.
func TestResolveGrantsWorktreeHooksSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	secret := filepath.Join(base, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	secretResolved, err := filepath.EvalSymlinks(secret)
	if err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(base, "repo", ".git")
	admin := filepath.Join(common, "worktrees", "wt")
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, real := range []string{"objects", "refs", "logs", "info"} {
		if err := os.MkdirAll(filepath.Join(common, real), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(common, "config"))
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The poisoned entry: hooks symlinked at the secret dir.
	if err := os.Symlink(secret, filepath.Join(common, "hooks")); err != nil {
		t.Fatal(err)
	}
	wd := filepath.Join(base, "wt")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, ".git"), []byte("gitdir: "+admin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range [][]string{g.ReadPaths, g.WritePaths, g.AllowPaths} {
		for _, gp := range list {
			resolved, rerr := filepath.EvalSymlinks(gp)
			if rerr != nil {
				continue
			}
			if resolved == secretResolved || strings.HasPrefix(resolved, secretResolved+string(filepath.Separator)) {
				t.Errorf("ESCAPE: grant %s resolves into the secret dir %s via hooks symlink", gp, secretResolved)
			}
		}
	}
}

// TestResolveGrantsWorktreeRejectsSymlinkEscape guards the sandbox boundary:
// grant paths derive from in-workdir files a prior sandboxed session could
// tamper with, and backends symlink-canonicalize every grant. A planted
// symlink under the common dir (e.g. objects -> secret) must NOT widen any
// grant to the target.
func TestResolveGrantsWorktreeRejectsSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	secret := filepath.Join(base, "secret")
	if err := os.MkdirAll(secret, 0o755); err != nil {
		t.Fatal(err)
	}
	secretResolved, err := filepath.EvalSymlinks(secret)
	if err != nil {
		t.Fatal(err)
	}

	// Attacker-controlled common dir with git's structural shape.
	common := filepath.Join(base, "evil", ".git")
	admin := filepath.Join(common, "worktrees", "wt")
	if err := os.MkdirAll(admin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, real := range []string{"logs", "info"} {
		if err := os.MkdirAll(filepath.Join(common, real), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(common, "config"))
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("../..\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The poisoned entries: objects/refs symlinked at the secret dir.
	if err := os.Symlink(secret, filepath.Join(common, "objects")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(common, "refs")); err != nil {
		t.Fatal(err)
	}

	wd := filepath.Join(base, "wt")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, ".git"), []byte("gitdir: "+admin+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}

	// No grant — nor its symlink-resolved form (what kernel rules use) —
	// may reach the secret dir.
	for _, list := range [][]string{g.ReadPaths, g.WritePaths, g.AllowPaths} {
		for _, gp := range list {
			resolved, rerr := filepath.EvalSymlinks(gp)
			if rerr != nil {
				continue
			}
			if resolved == secretResolved || strings.HasPrefix(resolved, secretResolved+string(filepath.Separator)) {
				t.Errorf("ESCAPE: grant %s resolves into the secret dir %s", gp, secretResolved)
			}
		}
	}
}

// TestResolveGrantsWorktreeRejectsSpoofedCommondir: a commondir violating
// git's <common>/worktrees/<name> invariant yields no worktree grants.
func TestResolveGrantsWorktreeRejectsSpoofedCommondir(t *testing.T) {
	wd, _ := makeLinkedWorktree(t)
	// Repoint commondir at an unrelated absolute dir.
	elsewhere := t.TempDir()
	admin := readAdminDir(t, wd)
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte(elsewhere+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range [][]string{g.ReadPaths, g.AllowPaths} {
		for _, gp := range list {
			if strings.HasPrefix(gp, elsewhere) {
				t.Errorf("spoofed commondir leaked a grant under %s: %s", elsewhere, gp)
			}
		}
	}
	// Only the workdir itself should be allow-granted.
	for _, ap := range g.AllowPaths {
		if ap != wd {
			t.Errorf("unexpected allow grant for spoofed worktree: %s", ap)
		}
	}
}

// readAdminDir parses the admin-dir path out of a linked worktree's .git.
func readAdminDir(t *testing.T, workdir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(workdir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir:"))
}

func TestResolveGrantsWorktreeAccessNone(t *testing.T) {
	wd, common := makeLinkedWorktree(t)
	p := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessNone}}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range [][]string{g.ReadPaths, g.AllowPaths} {
		for _, x := range list {
			if strings.HasPrefix(x, common) {
				t.Errorf("access=none must not grant common paths: %s", x)
			}
		}
	}
}

// writeFile is a tiny helper for the deny tests.
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDenyFullCLIPipeline exercises the exact path `omac sandbox run`
// takes — ParseFlags -> Merge -> ResolveGrants -> backend generation —
// to prove a `--deny .env` on the command line reaches the actual mask.
// (A live sandbox-exec/bwrap launch cannot run in this dev environment,
// so this is the end-to-end check below the process boundary.)
func TestDenyFullCLIPipeline(t *testing.T) {
	wd := t.TempDir()
	env := filepath.Join(wd, ".env")
	writeFile(t, env)
	keep := filepath.Join(wd, "app.py")
	writeFile(t, keep)

	// Args exactly as a user would type after `omac sandbox run`.
	flags, err := sandboxprofile.ParseFlags([]string{
		"--deny", ".env", "--block-net", "--", "cat", ".env",
	})
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}

	// Start from a profile that grants the cwd read+write (the default
	// posture) and merge the flags, as run.go does.
	base := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
	}
	merged, _ := sandboxprofile.Merge(base, flags)
	if err := merged.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	g, err := ResolveGrants(merged, wd, nil)
	if err != nil {
		t.Fatalf("ResolveGrants: %v", err)
	}

	// .env masked, app.py not.
	if !slices.Contains(g.ProtectedPaths, env) {
		t.Errorf("--deny .env did not protect %s: %v", env, g.ProtectedPaths)
	}
	if slices.Contains(g.ProtectedPaths, keep) {
		t.Error("app.py must remain accessible")
	}

	// Linux backend masks the file with --ro-bind /dev/null.
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, " "), "--ro-bind /dev/null "+env) {
		t.Errorf("bwrap argv missing deny mask for %s", env)
	}
	// macOS backend denies read+write on the file.
	sbpl := GenerateSBPL(g)
	if !strings.Contains(sbpl, "(deny file-read* (subpath \""+env+"\"))") ||
		!strings.Contains(sbpl, "(deny file-write* (subpath \""+env+"\"))") {
		t.Errorf("SBPL missing deny rules for %s:\n%s", env, sbpl)
	}
}

func TestResolveGrantsDenyBasenameGlobInWorkdir(t *testing.T) {
	wd := t.TempDir()
	env := filepath.Join(wd, ".env")
	writeFile(t, env)
	nested := filepath.Join(wd, "sub")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	nestedEnv := filepath.Join(nested, ".env")
	writeFile(t, nestedEnv)
	keep := filepath.Join(wd, "main.go")
	writeFile(t, keep)

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Deny: []string{".env"}},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.ProtectedPaths, env) {
		t.Errorf("cwd .env not protected: %v", g.ProtectedPaths)
	}
	if !slices.Contains(g.ProtectedPaths, nestedEnv) {
		t.Errorf("nested .env not protected: %v", g.ProtectedPaths)
	}
	if slices.Contains(g.ProtectedPaths, keep) {
		t.Error("non-matching file must not be protected")
	}
}

func TestResolveGrantsDenyExplicitPath(t *testing.T) {
	wd := t.TempDir()
	secret := filepath.Join(wd, "config", "prod.env")
	if err := os.MkdirAll(filepath.Dir(secret), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, secret)

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Deny: []string{secret}}, // absolute path form
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.ProtectedPaths, secret) {
		t.Errorf("explicit deny path not protected: %v", g.ProtectedPaths)
	}
}

func TestResolveGrantsDenyDirGlobMatchesDirectory(t *testing.T) {
	wd := t.TempDir()
	secretsDir := filepath.Join(wd, ".secrets")
	if err := os.Mkdir(secretsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(secretsDir, "token")
	writeFile(t, inside)

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Deny: []string{".secrets"}},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.ProtectedPaths, secretsDir) {
		t.Errorf("matched directory not protected: %v", g.ProtectedPaths)
	}
	// The walk must not descend into the matched dir (the dir mask
	// covers everything inside it), so the inner file is not listed.
	if slices.Contains(g.ProtectedPaths, inside) {
		t.Error("walk should not descend into a matched directory")
	}
}

func TestDenyProducesMaskInBackends(t *testing.T) {
	wd := t.TempDir()
	env := filepath.Join(wd, ".env")
	writeFile(t, env)

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Deny: []string{".env"}},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Linux: a denied file inside a granted tree is masked with
	// --ro-bind /dev/null <path>.
	argv, err := BuildBwrapArgv(g, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(argv, " "), "--ro-bind /dev/null "+env) {
		t.Errorf("bwrap argv missing deny mask for %s", env)
	}

	// macOS: a deny rule for the path appears in the SBPL.
	sbpl := GenerateSBPL(g)
	if !strings.Contains(sbpl, "(deny file-read* (subpath \""+env+"\"))") {
		t.Errorf("SBPL missing deny rule for %s:\n%s", env, sbpl)
	}
}

func TestResolveGrantsDenyGlobDoesNotScanBaseline(t *testing.T) {
	// A deny glob like "*.so" must not trigger a walk of baseline
	// system trees (e.g. /usr); only granted non-baseline roots. We
	// assert by timing/behaviour proxy: an empty workdir grant with a
	// broad glob yields no matches and returns promptly.
	wd := t.TempDir()
	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Deny: []string{"*.so"}},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, prot := range g.ProtectedPaths {
		if strings.HasPrefix(prot, "/usr/") || strings.HasPrefix(prot, "/lib") {
			t.Errorf("baseline tree was scanned for glob: %s", prot)
		}
	}
}
