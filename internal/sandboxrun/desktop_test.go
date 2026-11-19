package sandboxrun

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/opencodestate"
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

// In a chain of links (~/.config -> <tmp>/a -> <data>/config), the agent
// could re-point the middle link if it can write the folder that holds it.
func TestProtectDesktopStoresChecksEveryLinkInAChain(t *testing.T) {
	dirs := make([]string, 4)
	for i := range dirs {
		d, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		dirs[i] = d
	}
	home, tmp, data, other := dirs[0], dirs[1], dirs[2], dirs[3]
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	if err := os.MkdirAll(filepath.Join(data, "config", "ai.opencode.desktop"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(data, "config"), filepath.Join(tmp, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(tmp, "a"), filepath.Join(home, ".config")); err != nil {
		t.Fatal(err)
	}

	g := &Grants{AllowPaths: []string{other}}
	if err := protectDesktopStores(g, false); err != nil {
		t.Fatalf("chain without a grant on its links: %v", err)
	}
	for _, dir := range []string{filepath.Join(home, ".config"), filepath.Join(data, "config")} {
		if p := filepath.Join(dir, "ai.opencode.desktop", "opencode.global.dat"); !slices.Contains(g.ProtectedPaths, p) {
			t.Errorf("%s not in ProtectedPaths", p)
		}
	}
	err := protectDesktopStores(&Grants{AllowPaths: []string{tmp}}, false)
	if err == nil || !strings.Contains(err.Error(), "could change") {
		t.Errorf("write grant on the folder of the middle link: error = %v, want a refusal", err)
	}
}

// A loop of links never resolves; omac stops after the kernel's limit.
func TestProtectDesktopStoresRefusesLinkLoop(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	if err := os.Symlink(filepath.Join(home, "b"), filepath.Join(home, ".config")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, ".config"), filepath.Join(home, "b")); err != nil {
		t.Fatal(err)
	}
	if err := protectDesktopStores(&Grants{}, false); err == nil || !strings.Contains(err.Error(), "more than 40 symlinks") {
		t.Errorf("link loop: error = %v, want a refusal", err)
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

// Project discovery skips a Desktop project exactly when the sandbox
// would refuse it as a write grant, so a kept project never blocks the
// start. Each setup lists the folders it builds below a temp root as
// Desktop projects; links are written "link -> target", relative to the
// root.
func TestDesktopProjectSkipsMatchSandboxRefusals(t *testing.T) {
	const (
		macDesktop   = "home/Library/Application Support/ai.opencode.desktop"
		linuxDesktop = "home/.config/ai.opencode.desktop"
	)
	for _, tc := range []struct {
		name        string
		dirs, links []string
		desktopDir  string
		wantSkipped []string
	}{
		{
			name:        "plain home",
			dirs:        []string{linuxDesktop, "home/.config/nvim", "home/work/app"},
			desktopDir:  linuxDesktop,
			wantSkipped: []string{"home/.config"},
		},
		{
			name:        "linked ~/.config without a Desktop folder",
			dirs:        []string{macDesktop, "home/dotfiles/config", "home/work/app"},
			links:       []string{"home/.config -> home/dotfiles/config"},
			desktopDir:  macDesktop,
			wantSkipped: []string{"home/dotfiles", "home/dotfiles/config"},
		},
		{
			name:        "linked ~/.local/share",
			dirs:        []string{linuxDesktop, "home/dotfiles/share", "home/.local", "home/work/app"},
			links:       []string{"home/.local/share -> home/dotfiles/share"},
			desktopDir:  linuxDesktop,
			wantSkipped: []string{"home/dotfiles", "home/.local"},
		},
		{
			name:        "link to a missing target",
			dirs:        []string{macDesktop, "home/unmounted", "home/work/app"},
			links:       []string{"home/.config -> home/unmounted/config"},
			desktopDir:  macDesktop,
			wantSkipped: []string{"home/unmounted"},
		},
		{
			name:        "chain of links",
			dirs:        []string{"data/config/ai.opencode.desktop", "tmp", "home/work/app"},
			links:       []string{"tmp/a -> data/config", "home/.config -> tmp/a"},
			desktopDir:  "data/config/ai.opencode.desktop",
			wantSkipped: []string{"tmp", "data", "data/config"},
		},
		{
			name:        "project folder that is a link",
			dirs:        []string{linuxDesktop, "home/work/app"},
			links:       []string{"home/dots -> home/.config", "home/app -> home/work/app"},
			desktopDir:  linuxDesktop,
			wantSkipped: []string{"home/dots"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("HOME", filepath.Join(root, "home"))
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("XDG_DATA_HOME", "")
			t.Setenv("XDG_STATE_HOME", "")
			for _, d := range tc.dirs {
				if err := os.MkdirAll(filepath.Join(root, d), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, l := range tc.links {
				link, target, _ := strings.Cut(l, " -> ")
				if err := os.Symlink(filepath.Join(root, target), filepath.Join(root, link)); err != nil {
					t.Fatal(err)
				}
			}
			var projects []map[string]string
			err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err == nil && (d.IsDir() || d.Type()&os.ModeSymlink != 0) {
					projects = append(projects, map[string]string{"worktree": path})
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			list, err := json.Marshal(map[string]any{"value": projects})
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(map[string]string{"globalSync.project": string(list)})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, tc.desktopDir, "opencode.global.dat"), data, 0o600); err != nil {
				t.Fatal(err)
			}

			kept, skipped, err := opencodestate.Worktrees()
			if err != nil {
				t.Fatal(err)
			}
			for _, dir := range kept {
				if err := protectDesktopStores(&Grants{AllowPaths: []string{dir}}, false); err != nil {
					t.Errorf("kept project %s is refused: %v", dir, err)
				}
			}
			var skippedForStore []string
			for _, s := range skipped {
				if s.Reason != opencodestate.SkipDesktopStore {
					continue
				}
				skippedForStore = append(skippedForStore, s.Path)
				if err := protectDesktopStores(&Grants{AllowPaths: []string{s.Path}}, false); err == nil {
					t.Errorf("skipped project %s is accepted", s.Path)
				}
			}
			for _, want := range tc.wantSkipped {
				if !slices.Contains(skippedForStore, filepath.Join(root, want)) {
					t.Errorf("%s not skipped: %v", want, skippedForStore)
				}
			}
		})
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
