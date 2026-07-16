package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestProfileFixturesParseAndResolve runs the real Parse -> Validate ->
// ResolveGrants pipeline (the exact path `omac sandbox run` takes,
// mirroring TestDenyFullCLIPipeline above) against a set of checked-in
// profile fixtures representing real shapes users write today: a
// minimal profile, a workdir+network-allow profile, the
// override_deny+allow_unix_dir Docker-socket recipe, and a deny-glob
// profile. A change to the JSON schema, Parse, Validate, or
// ResolveGrants that would silently break one of these existing user
// configs fails here — fast, with no live agent required — instead of
// only surfacing in a live e2e run or a user bug report.
func TestProfileFixturesParseAndResolve(t *testing.T) {
	cases := []struct {
		file  string
		check func(t *testing.T, p *sandboxprofile.Profile, g *Grants)
	}{
		{
			file: "minimal.json",
			check: func(t *testing.T, p *sandboxprofile.Profile, g *Grants) {
				if !slices.Contains(g.AllowPaths, g.Workdir) {
					t.Errorf("minimal profile must grant the workdir readwrite: %v", g.AllowPaths)
				}
			},
		},
		{
			file: "workdir_network.json",
			check: func(t *testing.T, p *sandboxprofile.Profile, g *Grants) {
				if g.NetworkMode != sandboxprofile.ModeFiltered {
					t.Errorf("network.mode not resolved: %v", g.NetworkMode)
				}
				// AllowDomain is consumed directly from the parsed Profile by
				// the netproxy filter (run.go), not carried on Grants — check
				// it survived Parse/Validate intact.
				if !slices.Contains(p.Network.AllowDomain, "api.example.com") {
					t.Errorf("allow_domain not preserved: %v", p.Network.AllowDomain)
				}
			},
		},
		{
			file: "docker_socket_recipe.json",
			check: func(t *testing.T, p *sandboxprofile.Profile, g *Grants) {
				if !slices.Contains(g.UnixSocketDirs, "/var/run") {
					t.Errorf("allow_unix_dir not resolved into UnixSocketDirs: %v", g.UnixSocketDirs)
				}
				for _, p := range g.ProtectedPaths {
					if p == "/var/run/docker.sock" {
						t.Errorf("override_deny should have punched a hole for docker.sock: %v", g.ProtectedPaths)
					}
				}
			},
		},
		{
			file: "env_deny_glob.json",
			check: func(t *testing.T, p *sandboxprofile.Profile, g *Grants) {
				found := false
				for _, p := range g.ProtectedPaths {
					if strings.HasSuffix(p, "/.env") {
						found = true
					}
				}
				if !found {
					t.Errorf("deny glob .env not resolved into ProtectedPaths: %v", g.ProtectedPaths)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "profiles", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			p, err := sandboxprofile.Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if err := p.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			wd := t.TempDir()
			// A file for the deny-glob fixture to actually mask.
			if err := os.WriteFile(filepath.Join(wd, ".env"), []byte("SECRET=x"), 0o600); err != nil {
				t.Fatal(err)
			}
			g, err := ResolveGrants(p, wd, nil)
			if err != nil {
				t.Fatalf("ResolveGrants: %v", err)
			}
			tc.check(t, p, g)
		})
	}
}
