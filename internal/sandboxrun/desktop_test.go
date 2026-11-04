package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
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
		name, write    string
		learn, wantErr bool
	}{
		{name: "runtime subtree", write: runtime},
		{name: "whole desktop dir", write: state, wantErr: true},
		{name: "ancestor", write: filepath.Dir(state), wantErr: true},
		{name: "absent store", write: filepath.Join(state, "drafts.sqlite"), wantErr: true},
		{name: "learn mode", learn: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &Grants{}
			if tc.write != "" {
				g.AllowPaths = []string{tc.write}
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
	// A consulted path must not be reroutable via an agent-writable link.
	link := filepath.Join(home, "link")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", link)
	if err := protectDesktopStores(&Grants{}, false); err == nil {
		t.Fatal("symlink routing accepted")
	}
	if len(opencodestate.DesktopStorePaths()) == 0 {
		t.Fatal("no discovery sources protected")
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
