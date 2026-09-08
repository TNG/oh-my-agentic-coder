//go:build linux

package sandboxrun

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestIntegrationWriteProtectedProfileReadOnly: inside a read-write workdir,
// the profile and its pages sibling stay readable but reject writes (#267).
func TestIntegrationWriteProtectedProfileReadOnly(t *testing.T) {
	requireBwrap(t)
	workdir := t.TempDir()
	profile := filepath.Join(workdir, "sandbox.json")
	if err := os.WriteFile(profile, []byte(`{"meta":{"name":"t"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pages := filepath.Join(workdir, "sandbox.pages.json")
	if err := os.WriteFile(pages, []byte(`{"schema":1,"entries":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	p := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	g, err := ResolveGrants(p, workdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Mirror what Run does via writeProtectProfilePaths.
	g.WriteProtectedPaths = []string{profile, pages}

	// Both files stay readable.
	if out, code := runBwrapped(t, g, "/bin/sh", "-c",
		"cat "+profile+" >/dev/null && cat "+pages+" >/dev/null && echo READ-OK"); code != 0 || out == "" {
		t.Fatalf("profile and pages must stay readable (exit %d):\n%s", code, out)
	}

	// Neither accepts writes.
	for _, target := range []string{profile, pages} {
		if out, code := runBwrapped(t, g, "/bin/sh", "-c", "echo tampered >> "+target); code == 0 {
			t.Fatalf("write to %s must fail — a sandboxed session could rewrite the next launch's grants\n%s", target, out)
		}
	}

	// Control: the workdir itself stays writable, so the protection is
	// per-path, not a blanket deny.
	if out, code := runBwrapped(t, g, "/bin/sh", "-c",
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
