//go:build darwin

package sandboxrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestIntegrationOmacConfigDirMasked is the real-Seatbelt proof: the
// project-local .omac directory holds the sandbox definition (launcher config,
// profile, pages). Inside a read-write workdir it must be entirely unreadable
// and unwritable, so a session can neither read the rules nor plant or rewrite
// a file a later launch would trust. The protected denies sit after every
// allow in the SBPL, so they win over the writable workdir.
func TestIntegrationOmacConfigDirMasked(t *testing.T) {
	workdir := t.TempDir()
	localDir := config.LocalConfigDir(workdir)
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.ProjectLauncherConfigPath(workdir)
	profile := filepath.Join(localDir, "profile.json")
	pages := filepath.Join(localDir, "profile.pages.json")
	for path, body := range map[string]string{
		cfg:     "sandbox:\n  profile_name: profile\n",
		profile: `{"meta":{"name":"profile"}}`,
		pages:   `{"schema":1,"entries":[]}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, workdir, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror what Run assembles (writeProtectProfilePaths + launcher config).
	g.WriteProtectedPaths = []string{profile, pages, cfg}

	// The definition files are not readable.
	for _, target := range []string{cfg, profile, pages} {
		if out, code := runSandboxed(t, g, "/bin/sh", "-c", "cat "+target); code == 0 {
			t.Fatalf("%s must not be readable inside the sandbox, got:\n%s", target, out)
		}
	}

	// Nothing inside .omac accepts writes, creations, or removal.
	for _, sh := range []string{
		"echo tampered >> " + profile,
		"rm " + profile,
		"mv " + profile + " " + profile + ".moved",
		"echo own > " + filepath.Join(localDir, "evil.json"),
		"mkdir " + filepath.Join(localDir, "evil"),
		"cp /etc/hosts " + profile,
		"rm -rf " + localDir,
	} {
		if out, code := runSandboxed(t, g, "/bin/sh", "-c", sh); code == 0 {
			t.Fatalf("must fail inside the sandbox (a way to read, plant, or replace the sandbox definition): %q\n%s", sh, out)
		}
	}

	// Control: the workdir itself stays writable, so the protection is
	// scoped to .omac, not a blanket deny.
	if out, code := runSandboxed(t, g, "/bin/sh", "-c",
		"echo ok > "+filepath.Join(workdir, "agentfile")); code != 0 {
		t.Fatalf("workdir must stay writable (exit %d):\n%s", code, out)
	}

	// Nothing leaked through: the host files are byte-identical.
	if data, err := os.ReadFile(profile); err != nil || string(data) != `{"meta":{"name":"profile"}}` {
		t.Fatalf("profile was tampered with: %s (err %v)", data, err)
	}
}
