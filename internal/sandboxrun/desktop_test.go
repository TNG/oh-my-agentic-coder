package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestProtectDesktopStores(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	state := filepath.Join(home, ".config", "ai.opencode.desktop")
	t.Setenv("XDG_STATE_HOME", state)
	runtime := filepath.Join(state, "opencode")
	if err := os.MkdirAll(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, write, writeOnly, read string
		learn, wantErr               bool
	}{
		{name: "runtime subtree", write: runtime},
		{name: "whole desktop dir", write: state, wantErr: true},
		{name: "ancestor", write: filepath.Dir(state), wantErr: true},
		{name: "absent store", write: filepath.Join(state, "drafts.sqlite"), wantErr: true},
		{name: "write-only grant", writeOnly: state, wantErr: true},
		{name: "learn mode", learn: true, wantErr: true},
		// Desktop keeps cookies and browser storage next to its stores.
		{name: "read on desktop dir", read: state, wantErr: true},
		{name: "read on ancestor", read: home, wantErr: true},
		{name: "read on runtime subtree", read: runtime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &Grants{}
			if tc.write != "" {
				g.AllowPaths = []string{tc.write}
			}
			if tc.writeOnly != "" {
				g.WritePaths = []string{tc.writeOnly}
			}
			if tc.read != "" {
				g.ReadPaths = []string{tc.read}
			}
			err := protectDesktopStores(g, tc.learn)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if err == nil {
				for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
					if !slices.Contains(g.ProtectedPaths, filepath.Join(state, "drafts.sqlite")+suffix) {
						t.Fatal("store/companion not protected")
					}
				}
				if !slices.Contains(g.ProtectedPaths, filepath.Join(state, "opencode.global.dat")) {
					t.Fatal("legacy store not protected")
				}
			}
		})
	}
}

// The refusal names only what a user of omac serve can change: the
// folder it was started in, or the sandbox profile.
func TestProtectDesktopStoresAdvice(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	for _, tc := range []struct {
		name string
		g    *Grants
		want string
	}{
		{"write on workdir", &Grants{Workdir: home, AllowPaths: []string{home}}, "the folder omac serve was started in. Start omac serve from a project folder"},
		{"read on workdir", &Grants{Workdir: home, ReadPaths: []string{home}}, "the folder omac serve was started in. Start omac serve from a project folder"},
		{"write from profile", &Grants{Workdir: t.TempDir(), AllowPaths: []string{home}}, `"filesystem" section of your sandbox profile`},
	} {
		err := protectDesktopStores(tc.g, false)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to contain %q", tc.name, err, tc.want)
		}
		for _, flag := range []string{"--allow", "--write", "--read"} {
			if err != nil && strings.Contains(err.Error(), flag) {
				t.Errorf("%s: error %q names %s, which omac serve doesn't have", tc.name, err, flag)
			}
		}
	}
}

// A home reached through a symlink works, as long as no grant lets the
// agent re-point the link or change its target (ADR 0002).
func TestProtectDesktopStoresThroughSymlinks(t *testing.T) {
	realHome, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	linkDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(linkDir, "home")
	if err := os.Symlink(realHome, home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	const rel = ".config/ai.opencode.desktop"
	if err := os.MkdirAll(filepath.Join(realHome, rel), 0o700); err != nil {
		t.Fatal(err)
	}

	g := &Grants{}
	if err := protectDesktopStores(g, false); err != nil {
		t.Fatalf("symlinked home: %v", err)
	}
	for _, dir := range []string{home, realHome} {
		if p := filepath.Join(dir, rel, "opencode.global.dat"); !slices.Contains(g.ProtectedPaths, p) {
			t.Errorf("%s not in ProtectedPaths", p)
		}
	}

	for name, grant := range map[string]string{
		"folder holding the link": linkDir,
		"target of the link":      realHome,
	} {
		err := protectDesktopStores(&Grants{AllowPaths: []string{grant}}, false)
		if err == nil {
			t.Errorf("write grant on the %s: accepted", name)
		}
	}

	store := filepath.Join(realHome, rel, "opencode.global.dat")
	if err := os.Symlink(filepath.Join(realHome, "elsewhere.dat"), store); err != nil {
		t.Fatal(err)
	}
	if err := protectDesktopStores(&Grants{}, false); err == nil || !strings.Contains(err.Error(), "is a symlink") {
		t.Errorf("symlinked store: error = %v, want a refusal", err)
	}
}

// A link whose target is missing at startup (an unmounted disk) holds no
// Desktop data, so it doesn't refuse the start. The place where its
// target would appear is checked and denied like a real path, so the
// agent cannot create the target itself.
func TestProtectDesktopStoresAcceptsDanglingLink(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	if err := os.Symlink(filepath.Join("unmounted", "config"), filepath.Join(home, ".config")); err != nil {
		t.Fatal(err)
	}

	g := &Grants{}
	if err := protectDesktopStores(g, false); err != nil {
		t.Fatalf("dangling link: %v", err)
	}
	for _, dir := range []string{filepath.Join(home, ".config"), filepath.Join(home, "unmounted", "config")} {
		if p := filepath.Join(dir, "ai.opencode.desktop", "opencode.global.dat"); !slices.Contains(g.ProtectedPaths, p) {
			t.Errorf("%s not in ProtectedPaths", p)
		}
	}
	grant := &Grants{AllowPaths: []string{filepath.Join(home, "unmounted")}}
	if err := protectDesktopStores(grant, false); err == nil {
		t.Error("write grant where the link's target would appear: accepted")
	}
}

// Without HOME and XDG_STATE_HOME there is no store to protect, which
// must refuse the session instead of silently protecting nothing.
func TestProtectDesktopStoresFailsClosedWithoutStorePaths(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	err := protectDesktopStores(&Grants{}, false)
	if err == nil || !strings.Contains(err.Error(), "Set HOME, or export XDG_STATE_HOME") {
		t.Fatalf("error = %v, want a refusal naming HOME and XDG_STATE_HOME", err)
	}
}

// resolveRunGrants is what Run launches with, so these cases fail if Run
// stops honoring --for-opencode-desktop.
func TestResolveRunGrantsProtectsDesktopStores(t *testing.T) {
	setup := func(t *testing.T, home string) (state, runtimeDir string, resolve func(args ...string) (*Grants, error)) {
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("XDG_DATA_HOME", "")
		state = filepath.Join(home, ".config", "ai.opencode.desktop")
		t.Setenv("XDG_STATE_HOME", state)
		runtimeDir = filepath.Join(state, "opencode")
		if err := os.MkdirAll(runtimeDir, 0700); err != nil {
			t.Fatal(err)
		}
		workdir := t.TempDir()
		resolve = func(args ...string) (*Grants, error) {
			t.Helper()
			flags, err := sandboxprofile.ParseFlags(append(args, "--", "true"))
			if err != nil {
				t.Fatal(err)
			}
			merged, _ := sandboxprofile.Merge(&sandboxprofile.Profile{}, flags)
			return resolveRunGrants(merged, flags, workdir, nil)
		}
		return state, runtimeDir, resolve
	}

	t.Run("refusals", func(t *testing.T) {
		home, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		state, runtimeDir, resolve := setup(t, home)
		if _, err := resolve("--for-opencode-desktop", "--allow", state); err == nil || !strings.Contains(err.Error(), "could change OpenCode Desktop's file") {
			t.Errorf("write grant on the Desktop dir: error = %v, want a refusal", err)
		}
		if _, err := resolve("--for-opencode-desktop", "--learn", "--allow-literal", runtimeDir); err == nil || !strings.Contains(err.Error(), "--learn") {
			t.Errorf("--learn: error = %v, want a refusal", err)
		}
		if _, err := resolve("--allow", state); err != nil {
			t.Errorf("without --for-opencode-desktop the same grant must pass: %v", err)
		}
	})

	t.Run("success", func(t *testing.T) {
		state, runtimeDir, resolve := setup(t, homeOutsideBaselineGrants(t))
		g, err := resolve("--for-opencode-desktop", "--allow-literal", runtimeDir)
		if err != nil {
			t.Fatalf("grant on only the runtime dir: %v", err)
		}
		for _, name := range []string{"opencode.global.dat", "drafts.sqlite", "drafts.sqlite-wal"} {
			if !slices.Contains(g.ProtectedPaths, filepath.Join(state, name)) {
				t.Errorf("%s not in ProtectedPaths", name)
			}
		}
	})
}

// homeOutsideBaselineGrants returns a new folder that no baseline write
// grant covers. The temp dir doesn't qualify on macOS, where the baseline
// grants /var/folders. It tries the user cache dir, then the package dir.
func homeOutsideBaselineGrants(t *testing.T) string {
	t.Helper()
	base, err := ResolveGrants(&sandboxprofile.Profile{}, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var parents []string
	if dir, err := os.UserCacheDir(); err == nil {
		parents = append(parents, dir)
	}
	if dir, err := os.Getwd(); err == nil {
		parents = append(parents, dir)
	}
	for _, parent := range parents {
		home, err := os.MkdirTemp(parent, "omac-desktop-home-")
		if err != nil {
			continue
		}
		t.Cleanup(func() { os.RemoveAll(home) })
		if home, err = filepath.EvalSymlinks(home); err != nil {
			t.Fatal(err)
		}
		covered := false
		for _, w := range append(base.WritePaths, base.AllowPaths...) {
			covered = covered || isUnder(home, canonicalPath(w))
		}
		if !covered {
			return home
		}
	}
	t.Skipf("no writable folder outside the baseline write grants in %v", parents)
	return ""
}

func TestDesktopLiteralGrantStillScansSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "project$HOME")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, ".env")
	if err := os.WriteFile(secret, []byte("TOKEN=test"), 0600); err != nil {
		t.Fatal(err)
	}
	flags, err := sandboxprofile.ParseFlags([]string{"--allow-literal", dir, "--", "true"})
	if err != nil {
		t.Fatal(err)
	}
	p, _ := sandboxprofile.Merge(&sandboxprofile.Profile{}, flags)
	g, err := ResolveGrants(p, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.AllowPaths, dir) {
		t.Fatal("literal grant was expanded")
	}
	if !slices.Contains(g.ProtectedPaths, secret) {
		t.Fatal("literal grant bypasses secret scan")
	}
}

// Only omac serve passes --for-opencode-desktop, so its refusals must
// reach the user as serve's own error, before anything is launched.
func TestRunReportsDesktopRefusalAsServeError(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	flags, err := sandboxprofile.ParseFlags([]string{"--for-opencode-desktop", "--allow", home, "--", "true"})
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	code := Run(Options{Flags: flags, Workdir: t.TempDir(), Stderr: &stderr})
	if code != 1 || !strings.Contains(stderr.String(), "omac serve: --for-opencode-desktop: the agent could change") {
		t.Errorf("Run = %d, stderr:\n%s\nwant 1 and a refusal from omac serve", code, stderr.String())
	}
}
