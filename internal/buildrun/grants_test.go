package buildrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxrun"
)

func TestGrantsFor(t *testing.T) {
	wt := t.TempDir()
	canonical, err := filepath.EvalSymlinks(wt)
	if err != nil {
		t.Fatal(err)
	}
	backend := filepath.Join(wt, "backend")
	makeWrapper(t, backend)
	cacheDir := filepath.Join(t.TempDir(), "cache")

	g, err := GrantsFor(canonical, cacheDir)
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}

	contains := func(list []string, want string) bool {
		for _, p := range list {
			if p == want {
				return true
			}
		}
		return false
	}

	t.Run("grant set is worktree + cache leaf + private temp only", func(t *testing.T) {
		if !contains(g.AllowPaths, canonical) {
			t.Errorf("AllowPaths missing worktree %s: %v", canonical, g.AllowPaths)
		}
		// The cache SCOPE dir itself must not be rw: only the resolved
		// gradle leaf below it is granted. Sibling tool caches created by
		// `omac start`/`serve` (go/npm/pip leaves) would otherwise be
		// writable by the build executor (cache over-grant).
		if contains(g.AllowPaths, cacheDir) {
			t.Errorf("AllowPaths must not contain the cache scope dir %s: %v", cacheDir, g.AllowPaths)
		}
		wantLeaf := filepath.Join(cacheDir, "gradle")
		if !contains(g.AllowPaths, wantLeaf) {
			t.Errorf("AllowPaths missing GRADLE_USER_HOME leaf %s: %v", wantLeaf, g.AllowPaths)
		}
		// The bare cache pre-leaf lock dir must not widen the write
		// surface beyond the scoped cache dir itself.
		if !contains(g.AllowPaths, filepath.Join(cacheDir, "gradle", ".omac-pre-leaf-locks")) {
			t.Errorf(
				"AllowPaths missing pre-leaf lock dir: %v", g.AllowPaths)
		}
	})

	t.Run("sibling tool caches are not writable", func(t *testing.T) {
		// Simulate sibling tool leaves laid down by `omac start` (go,
		// npm, pip per the cache-isolation probes). Seatbelt/bwrap
		// subpath grants are prefix-based: granting the scope dir rw
		// would silently grant these too, so the scope dir must be
		// absent from every grant list entirely.
		if err := os.MkdirAll(filepath.Join(cacheDir, "go"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, sibling := range []string{"go", "npm", "pip"} {
			leaf := filepath.Join(cacheDir, sibling)
			if contains(g.AllowPaths, leaf) || contains(g.ReadPaths, leaf) || contains(g.WritePaths, leaf) {
				t.Errorf("sibling tool cache %s must not be granted: allow=%v read=%v write=%v",
					leaf, g.AllowPaths, g.ReadPaths, g.WritePaths)
			}
		}
		// The scope dir may appear only in the ancestor read rule that
		// makes descendants traversable at all (the same existence-path
		// leak the toolcache layout already relies on between scopes) —
		// never in a write rule. Subpath write rules carry the
		// "(require-not" canonicalization marker (see sbpl.go).
		sbpl := sandboxrun.GenerateSBPL(g.Grants)
		for _, line := range strings.Split(sbpl, "\n") {
			if strings.Contains(line, cacheDir) && strings.Contains(line, "require-not") {
				t.Errorf("SBPL must never make the cache scope dir writable:\n%s", line)
			}
		}
	})

	t.Run("network blocked, kernel enforcement", func(t *testing.T) {
		if g.NetworkMode != "blocked" {
			t.Errorf("NetworkMode = %q, want blocked", g.NetworkMode)
		}
		if g.Enforcement != "kernel" {
			t.Errorf("Enforcement = %q, want kernel", g.Enforcement)
		}
	})

	t.Run("host home is not granted", func(t *testing.T) {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Skip("no home")
		}
		if contains(g.AllowPaths, home) || contains(g.ReadPaths, home) || contains(g.WritePaths, home) {
			t.Errorf("home dir must never be granted")
		}
		hostGradle := filepath.Join(home, ".gradle")
		if contains(g.AllowPaths, hostGradle) || contains(g.ReadPaths, hostGradle) {
			t.Errorf("host ~/.gradle must never be granted: %v", g.AllowPaths)
		}
	})

	t.Run("environment redirects gradle into cache leaf", func(t *testing.T) {
		env := ChildEnv(g)
		m := map[string]string{}
		for _, kv := range env {
			if i := strings.IndexByte(kv, '='); i > 0 {
				m[kv[:i]] = kv[i+1:]
			}
		}
		if got := m["GRADLE_USER_HOME"]; got != filepath.Join(cacheDir, "gradle") {
			t.Errorf("GRADLE_USER_HOME = %q, want %s", got, filepath.Join(cacheDir, "gradle"))
		}
		if m["HOME"] != "" {
			t.Errorf("HOME must not pass through (host gradle init scripts): got %q", m["HOME"])
		}
		for _, leaked := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "GITHUB_TOKEN", "OMAC_SOCKET", "OMAC_BASE"} {
			if _, ok := m[leaked]; ok {
				t.Errorf("env must not contain %s", leaked)
			}
		}
		if m["PATH"] == "" {
			t.Error("PATH must be present (java discovery)")
		}
		if m["TMPDIR"] != g.TmpDir() {
			t.Errorf("TMPDIR = %q, want private temp %q", m["TMPDIR"], g.TmpDir())
		}
	})

	t.Run("sbpl denies host secret fixture", func(t *testing.T) {
		// The unit-level kernel-proof: a host secret path outside the
		// grant set must not appear in any allow rule, so (deny default)
		// covers it.
		secret := filepath.Join(t.TempDir(), "host-secret")
		if err := os.WriteFile(secret, []byte("s3cr3t"), 0o600); err != nil {
			t.Fatal(err)
		}
		sbpl := sandboxrun.GenerateSBPL(g.Grants)
		if strings.Contains(sbpl, secret) {
			t.Errorf("SBPL must not reference ungranted secret path %s", secret)
		}
		if !strings.Contains(sbpl, "(deny default)") {
			t.Error("SBPL must start from (deny default)")
		}
		// Sanity: the granted gradle leaf and worktree DO appear.
		if !strings.Contains(sbpl, canonical) {
			t.Errorf("SBPL must grant the worktree %s", canonical)
		}
	})
}

func TestGrantsForMissingWorktree(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "cache")
	if _, err := GrantsFor(filepath.Join(t.TempDir(), "nope"), cacheDir); err == nil {
		t.Fatal("expected error for missing worktree")
	}
}

func TestGrantsForPreparesGradleLeaf(t *testing.T) {
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	g, err := GrantsFor(wt, cacheDir)
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	leaf := filepath.Join(cacheDir, "gradle")
	fi, err := os.Stat(leaf)
	if err != nil {
		t.Fatalf("gradle leaf not prepared: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("gradle leaf is not a dir")
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("gradle leaf perms = %o, want 700", got)
	}
	if g.GradleUserHome() != leaf {
		t.Errorf("GradleUserHome = %q, want %q", g.GradleUserHome(), leaf)
	}
	if g.TmpDir() == "" || g.TmpDir() == os.TempDir() {
		t.Errorf("TmpDir must be a private dir, got %q", g.TmpDir())
	}
}

func TestGrantsForNeverDeletesInsideCache(t *testing.T) {
	// v0 leaves daemon locks alone: no lock hygiene runs before launch
	// (warm-daemon reuse is a later ticket). A stale-looking daemon lock
	// must survive GrantsFor untouched.
	wt, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	lock := filepath.Join(cacheDir, "gradle", ".gradle", "daemon", "8.5", "registry.bin.lock")
	if err := os.MkdirAll(filepath.Dir(lock), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lock, []byte("lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := GrantsFor(wt, cacheDir); err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("daemon lock must not be pruned by GrantsFor: %v", err)
	}
}
