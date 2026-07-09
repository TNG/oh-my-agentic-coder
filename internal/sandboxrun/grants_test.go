package sandboxrun

import (
	"net"
	"os"
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

// TestResolveGrantsWorkdirEnvProtectedByDefault verifies that a
// workdir-local .env / .envrc is masked by the baseline
// WorkdirProtected set without any --deny flag, and that
// filesystem.override_deny can punch a hole through it.
func TestResolveGrantsWorkdirEnvProtectedByDefault(t *testing.T) {
	wd := t.TempDir()
	env := filepath.Join(wd, ".env")
	writeFile(t, env)
	envrc := filepath.Join(wd, ".envrc")
	writeFile(t, envrc)
	nested := filepath.Join(wd, "config")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	nestedEnv := filepath.Join(nested, ".env")
	writeFile(t, nestedEnv)
	keep := filepath.Join(wd, "main.go")
	writeFile(t, keep)

	t.Run("default protects .env and .envrc", func(t *testing.T) {
		p := &sandboxprofile.Profile{
			Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		}
		g, err := ResolveGrants(p, wd, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(g.ProtectedPaths, env) {
			t.Errorf("workdir .env not protected by default: %v", g.ProtectedPaths)
		}
		if !slices.Contains(g.ProtectedPaths, envrc) {
			t.Errorf("workdir .envrc not protected by default: %v", g.ProtectedPaths)
		}
		if !slices.Contains(g.ProtectedPaths, nestedEnv) {
			t.Errorf("nested .env not protected by default: %v", g.ProtectedPaths)
		}
		if slices.Contains(g.ProtectedPaths, keep) {
			t.Error("non-.env file must not be protected")
		}
	})

	t.Run("override_deny basename punches hole", func(t *testing.T) {
		p := &sandboxprofile.Profile{
			Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
			Filesystem: sandboxprofile.Filesystem{OverrideDeny: []string{".env"}},
		}
		g, err := ResolveGrants(p, wd, nil)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(g.ProtectedPaths, env) {
			t.Errorf(".env should be unprotected via override_deny: %v", g.ProtectedPaths)
		}
		if slices.Contains(g.ProtectedPaths, nestedEnv) {
			t.Errorf("nested .env should be unprotected via override_deny: %v", g.ProtectedPaths)
		}
		// .envrc is not overridden and stays protected.
		if !slices.Contains(g.ProtectedPaths, envrc) {
			t.Errorf(".envrc should remain protected: %v", g.ProtectedPaths)
		}
	})

	t.Run("override_deny absolute path punches hole", func(t *testing.T) {
		p := &sandboxprofile.Profile{
			Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
			Filesystem: sandboxprofile.Filesystem{OverrideDeny: []string{env}},
		}
		g, err := ResolveGrants(p, wd, nil)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(g.ProtectedPaths, env) {
			t.Errorf("absolute override_deny should unprotect %s: %v", env, g.ProtectedPaths)
		}
		// Nested .env is a different absolute path and stays protected.
		if !slices.Contains(g.ProtectedPaths, nestedEnv) {
			t.Errorf("nested .env should remain protected: %v", g.ProtectedPaths)
		}
	})
}

// TestResolveGrantsBaselineWorkdirDenyDoesNotScanBaseline mirrors
// TestResolveGrantsDenyGlobDoesNotScanBaseline but for the baseline
// WorkdirProtected walk (resolveBaselineWorkdirDeny). The invariant
// holds today (shares denyScan), but a regression that accidentally
// passed base.Read into the baseline resolver would go undetected
// without this guard.
func TestResolveGrantsBaselineWorkdirDenyDoesNotScanBaseline(t *testing.T) {
	wd := t.TempDir()
	env := filepath.Join(wd, ".env")
	writeFile(t, env)

	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Baseline .env protection is active.
	if !slices.Contains(g.ProtectedPaths, env) {
		t.Errorf("workdir .env not protected by default: %v", g.ProtectedPaths)
	}
	// No baseline system tree was scanned.
	for _, prot := range g.ProtectedPaths {
		if strings.HasPrefix(prot, "/usr/") || strings.HasPrefix(prot, "/lib") {
			t.Errorf("baseline tree was scanned for workdir deny: %s", prot)
		}
	}
}

// TestResolveGrantsUserDenyAndBaselineBothActive verifies that when
// both user deny globs and baseline workdir-protected basenames are
// active simultaneously, all matches are found and no duplicates appear.
func TestResolveGrantsUserDenyAndBaselineBothActive(t *testing.T) {
	wd := t.TempDir()
	env := filepath.Join(wd, ".env")
	writeFile(t, env)
	keyFile := filepath.Join(wd, "secret.key")
	writeFile(t, keyFile)
	keep := filepath.Join(wd, "app.go")
	writeFile(t, keep)

	p := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{Deny: []string{"*.key"}},
	}
	g, err := ResolveGrants(p, wd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.ProtectedPaths, env) {
		t.Errorf(".env (baseline) not protected: %v", g.ProtectedPaths)
	}
	if !slices.Contains(g.ProtectedPaths, keyFile) {
		t.Errorf("secret.key (user deny) not protected: %v", g.ProtectedPaths)
	}
	if slices.Contains(g.ProtectedPaths, keep) {
		t.Error("app.go must not be protected")
	}
	seen := map[string]bool{}
	for _, prot := range g.ProtectedPaths {
		if seen[prot] {
			t.Errorf("duplicate protected path: %s", prot)
		}
		seen[prot] = true
	}
}
