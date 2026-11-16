package sandboxrun

import (
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
		name, write, writeOnly, read, goos string
		learn, wantErr                     bool
	}{
		{name: "runtime subtree", write: runtime},
		{name: "whole desktop dir", write: state, wantErr: true},
		{name: "ancestor", write: filepath.Dir(state), wantErr: true},
		{name: "absent store", write: filepath.Join(state, "drafts.sqlite"), wantErr: true},
		{name: "write-only grant", writeOnly: state, wantErr: true},
		{name: "learn mode", learn: true, wantErr: true},
		// bwrap hides only files that exist at launch, so a read grant
		// over a store dir would expose stores Desktop creates later.
		{name: "linux read on desktop dir", read: state, goos: "linux", wantErr: true},
		{name: "linux read on ancestor", read: home, goos: "linux", wantErr: true},
		{name: "linux read on runtime subtree", read: runtime, goos: "linux"},
		{name: "darwin read on desktop dir", read: state, goos: "darwin"},
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
			goos := tc.goos
			if goos == "" {
				goos = "linux"
			}
			err := protectDesktopStores(g, tc.learn, goos)
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
	// A consulted path must not be reroutable via an agent-writable link.
	link := filepath.Join(home, "link")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", link)
	if err := protectDesktopStores(&Grants{}, false, "linux"); err == nil {
		t.Fatal("symlink routing accepted")
	}
	if len(opencodestate.DesktopStorePaths()) == 0 {
		t.Fatal("no discovery sources protected")
	}
}

// resolveRunGrants is what Run launches with, so these cases fail if Run
// stops honoring --for-opencode-desktop.
func TestResolveRunGrantsProtectsDesktopStores(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	state := filepath.Join(home, ".config", "ai.opencode.desktop")
	t.Setenv("XDG_STATE_HOME", state)
	runtimeDir := filepath.Join(state, "opencode")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	resolve := func(args ...string) (*Grants, error) {
		t.Helper()
		flags, err := sandboxprofile.ParseFlags(append(args, "--", "true"))
		if err != nil {
			t.Fatal(err)
		}
		merged, _ := sandboxprofile.Merge(&sandboxprofile.Profile{}, flags)
		return resolveRunGrants(merged, flags, workdir, nil)
	}

	if _, err := resolve("--for-opencode-desktop", "--allow", state); err == nil || !strings.Contains(err.Error(), "would be writable by the agent through the write grant") {
		t.Errorf("write grant on the Desktop dir: error = %v, want a refusal", err)
	}
	if _, err := resolve("--for-opencode-desktop", "--learn", "--allow-literal", runtimeDir); err == nil || !strings.Contains(err.Error(), "--learn") {
		t.Errorf("--learn: error = %v, want a refusal", err)
	}
	if _, err := resolve("--allow", state); err != nil {
		t.Errorf("without --for-opencode-desktop the same grant must pass: %v", err)
	}

	// The success case needs a home outside the platform's own write
	// grants (on macOS the temp dir is under /var/folders, which the
	// baseline grants writable).
	base, err := ResolveGrants(&sandboxprofile.Profile{}, workdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range append(base.WritePaths, base.AllowPaths...) {
		if isUnder(home, canonicalPath(w)) {
			t.Skipf("temp home %s is under the baseline write grant %s", home, w)
		}
	}
	g, err := resolve("--for-opencode-desktop", "--allow-literal", runtimeDir)
	if err != nil {
		t.Fatalf("grant on only the runtime dir: %v", err)
	}
	for _, name := range []string{"opencode.global.dat", "drafts.sqlite", "drafts.sqlite-wal"} {
		if !slices.Contains(g.ProtectedPaths, filepath.Join(state, name)) {
			t.Errorf("%s not in ProtectedPaths", name)
		}
	}
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
