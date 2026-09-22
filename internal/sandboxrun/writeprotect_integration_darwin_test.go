//go:build darwin

package sandboxrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestIntegrationWriteProtectedProfileReadOnly is the real-Seatbelt proof of
// #267: inside a read-write workdir, the profile, its pages sibling, and the
// launcher config stay readable but reject every write form, including
// unlink, rename, and replace. The file-write* denies sit after every allow
// in the SBPL, so they win over the writable workdir and TMPDIR baseline.
func TestIntegrationWriteProtectedProfileReadOnly(t *testing.T) {
	workdir := t.TempDir()
	profile := filepath.Join(workdir, "sandbox.json")
	if err := os.WriteFile(profile, []byte(`{"meta":{"name":"t"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pages := filepath.Join(workdir, "sandbox.pages.json")
	if err := os.WriteFile(pages, []byte(`{"schema":1,"entries":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := config.ProjectLauncherConfigPath(workdir)
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("sandbox:\n  profile_path: ./sandbox.json\n"), 0o644); err != nil {
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
	// Mirror what Run assembles (writeProtectProfilePaths + the launcher
	// config append).
	g.WriteProtectedPaths = []string{profile, pages, cfgPath}

	// All three files stay readable.
	if out, code := runSandboxed(t, g, "/bin/sh", "-c",
		"cat "+profile+" >/dev/null && cat "+pages+" >/dev/null && cat "+cfgPath+" >/dev/null && echo READ-OK"); code != 0 || out == "" {
		t.Fatalf("profile, pages, and launcher config must stay readable (exit %d):\n%s", code, out)
	}

	// None accepts writes.
	for _, target := range []string{profile, pages, cfgPath} {
		if out, code := runSandboxed(t, g, "/bin/sh", "-c", "echo tampered >> "+target); code == 0 {
			t.Fatalf("write to %s must fail — a sandboxed session could rewrite the next launch's grants\n%s", target, out)
		}
	}

	// Remove, rename, and replace are writes too: a session must not be
	// able to swap the protected file for its own.
	for _, sh := range []string{
		"rm " + profile,
		"mv " + profile + " " + profile + ".moved",
		"echo own > " + profile + ".own && mv " + profile + ".own " + profile,
		"cp /etc/hosts " + profile,
		"ln " + profile + " " + profile + ".hardlink",
	} {
		if out, code := runSandboxed(t, g, "/bin/sh", "-c", sh); code == 0 {
			t.Fatalf("must fail inside the sandbox (a way to remove, rename, or replace the profile): %q\n%s", sh, out)
		}
	}

	// Control: the workdir itself stays writable, so the protection is
	// per-path, not a blanket deny.
	if out, code := runSandboxed(t, g, "/bin/sh", "-c",
		"echo ok > "+filepath.Join(workdir, "agentfile")); code != 0 {
		t.Fatalf("workdir must stay writable (exit %d):\n%s", code, out)
	}

	// Nothing leaked through: the host profile is byte-identical.
	data, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"meta":{"name":"t"}}` {
		t.Fatalf("profile was tampered with: %s", data)
	}
}
