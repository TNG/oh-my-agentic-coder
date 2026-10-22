//go:build linux

package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestIntegrationOmacConfigDirMasked: the project-local .omac directory holds
// the sandbox definition (launcher config, profile, pages). Inside a read-write
// workdir it must be entirely unreadable and unwritable, so a session can
// neither read the rules nor plant or rewrite a file a later launch would
// trust.
func TestIntegrationOmacConfigDirMasked(t *testing.T) {
	requireBwrap(t)
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
	// The .omac directory is masked with the read-only marker dir, exactly as
	// Run does; a tmpfs fallback would be writable and defeat the test.
	g.DenialText = "denied by test"
	cleanup, err := g.prepareMarkers()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// The definition files are not readable.
	for _, target := range []string{cfg, profile, pages} {
		if out, code := runBwrapped(t, g, "/bin/sh", "-c", "cat "+target); code == 0 {
			t.Fatalf("%s must not be readable inside the sandbox, got:\n%s", target, out)
		}
	}

	// The masked .omac dir shows only the denial explanation — the agent
	// learns why the path is blocked instead of suspecting data loss.
	if out, code := runBwrapped(t, g, "/bin/sh", "-c", "ls -A "+localDir); code != 0 || !strings.Contains(out, markerDirFileName) {
		t.Fatalf("%s must list the denial notice marker %q inside the sandbox, got code %d:\n%s", localDir, markerDirFileName, code, out)
	}

	// Nothing inside .omac accepts writes, creations, or removal.
	for _, sh := range []string{
		"echo tampered >> " + profile,
		"rm " + profile,
		"mv " + profile + " " + profile + ".moved",
		"echo own > " + filepath.Join(localDir, "evil.json"),
		"mkdir " + filepath.Join(localDir, "evil"),
		"cp /etc/hostname " + profile,
		"chmod +w " + profile,
		"rm -rf " + localDir,
	} {
		if out, code := runBwrapped(t, g, "/bin/sh", "-c", sh); code == 0 {
			t.Fatalf("must fail inside the sandbox (a way to read, plant, or replace the sandbox definition): %q\n%s", sh, out)
		}
	}

	// Control: the workdir itself stays writable, so the protection is
	// scoped to .omac, not a blanket deny.
	if out, code := runBwrapped(t, g, "/bin/sh", "-c",
		"echo ok > "+filepath.Join(workdir, "agentfile")); code != 0 {
		t.Fatalf("workdir must stay writable (exit %d):\n%s", code, out)
	}

	// Nothing leaked through: the host files are byte-identical.
	if data, err := os.ReadFile(profile); err != nil || string(data) != `{"meta":{"name":"profile"}}` {
		t.Fatalf("profile was tampered with: %s (err %v)", data, err)
	}
}

// TestIntegrationPlantedOmacDirMaskedWithDenial: resolving grants must
// catch a planted .omac under a granted tree by leaf name, and mask it
// read-only with the denial explanation — a session cannot read or rewrite
// a profile that a later launch (e.g. started from that subdir) would
// trust, and the agent learns the directory is intentionally blocked.
func TestIntegrationPlantedOmacDirMaskedWithDenial(t *testing.T) {
	requireBwrap(t)
	workdir := t.TempDir()
	nested := filepath.Join(workdir, "sub")
	localDir := filepath.Join(nested, ".omac")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(localDir, "default.json")
	if err := os.WriteFile(profile, []byte(`{"meta":{"name":"evil"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, workdir, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	g.DenialText = "denied by test"
	cleanup, err := g.prepareMarkers()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// The planted dir is caught by the deny walk (leaf name .omac) and
	// therefore part of ProtectedPaths.
	if !slices.Contains(g.ProtectedPaths, localDir) {
		t.Fatalf("ProtectedPaths = %v; want %q", g.ProtectedPaths, localDir)
	}

	// Listing shows only the denial notice; reads and writes fail.
	if out, code := runBwrapped(t, g, "/bin/sh", "-c", "ls -A "+localDir); code != 0 || !strings.Contains(out, markerDirFileName) {
		t.Fatalf("a planted nested .omac must expose only the denial notice %q, got code %d:\n%s", markerDirFileName, code, out)
	}
	// Planted content is unreadable; writes and removal fail. The denial
	// notice itself is the one readable byte (positive control).
	if out, code := runBwrapped(t, g, "/bin/sh", "-c", "cat "+localDir+"/"+markerDirFileName); code != 0 {
		t.Fatalf("the denial notice must be readable inside the sandbox, got code %d:\n%s", code, out)
	}
	for _, sh := range []string{
		"cat " + profile,
		"echo own > " + filepath.Join(localDir, "planted.json"),
		"rm -rf " + localDir,
	} {
		if out, code := runBwrapped(t, g, "/bin/sh", "-c", sh); code == 0 {
			t.Fatalf("planted nested .omac must be masked by %q — got:\n%s", sh, out)
		}
	}

	// Control: a sibling dot-env keeps the explanatory marker like any
	// other protected path.
	dotenv := filepath.Join(workdir, ".env")
	if err := os.WriteFile(dotenv, []byte("SECRET=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, code := runBwrapped(t, g, "/bin/sh", "-c", "cat "+dotenv); code != 0 {
		t.Fatalf(".env must be masked with its explanatory marker (readable), got code %d:\n%s", code, out)
	}
}

// TestIntegrationLearnModeKeepsOmacMasked: learn mode lifts the profile's
// protected paths and grants "/", but the .omac directory must stay masked so a
// session cannot plant a profile a later, non-learn launch would trust.
func TestIntegrationLearnModeKeepsOmacMasked(t *testing.T) {
	requireBwrap(t)
	workdir := t.TempDir()
	localDir := config.LocalConfigDir(workdir)
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(localDir, "default.json")
	if err := os.WriteFile(profile, []byte(`{"meta":{"name":"default"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, workdir, os.Stderr)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror Run's learn-mode handling.
	g = g.withUnrestrictedFilesystem()
	g.ProtectedPaths = dedupe(append(g.ProtectedPaths, sandboxprofile.NonOverridableProtectedPaths(localDir)...))
	g.DenialText = "denied by test"
	cleanup, err := g.prepareMarkers()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// .omac stays unreadable and unwritable.
	for _, target := range []string{profile, filepath.Join(localDir, "planted.json")} {
		if out, code := runBwrapped(t, g, "/bin/sh", "-c",
			"cat "+target+" 2>/dev/null; echo own > "+target); code == 0 {
			t.Fatalf("learn mode must keep %s masked, got:\n%s", target, out)
		}
	}

	// Control: the rest of the workdir is writable, so the mask is scoped.
	if out, code := runBwrapped(t, g, "/bin/sh", "-c",
		"echo ok > "+filepath.Join(workdir, "agentfile")); code != 0 {
		t.Fatalf("workdir must stay writable in learn mode (exit %d):\n%s", code, out)
	}
}
