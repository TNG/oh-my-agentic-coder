package sandboxrun

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecurityDenyScanCapFailsClosed asserts that when the launch-time
// protection scan cannot enumerate a granted tree completely, the sandbox
// refuses to start instead of proceeding with an incomplete protected set.
// A protection scan that cannot finish must not degrade to "unprotected":
// any file the configuration promises is blocked must remain blocked, so
// the only safe response to an incomplete enumeration is to fail closed.
func TestSecurityDenyScanCapFailsClosed(t *testing.T) {
	workdir := t.TempDir()

	// A protected baseline basename file placed at a lexically late path.
	// The protection scan walks granted roots in lexical order and stops
	// after maxDenyScanEntries, so a target sorted after the padding tree
	// is never visited and would be left unmasked by an implementation
	// that fails open. Placing it late makes the incompleteness concrete:
	// under the broken behavior the file is genuinely absent from the
	// protected set, not merely unconfirmed.
	protectedDir := filepath.Join(workdir, "zzz-protected")
	if err := os.Mkdir(protectedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	protectedFile := filepath.Join(protectedDir, ".env")
	if err := os.WriteFile(protectedFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	populateLargeTree(t, workdir)

	prof := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}

	// Control: a small workdir resolves without error, proving any
	// refusal below is caused by the unbounded tree, not by the setup.
	controlProf := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	if _, err := ResolveGrants(controlProf, t.TempDir(), nil); err != nil {
		t.Fatalf("control: small workdir should resolve, got %v", err)
	}

	// Fixed state: an incomplete protection scan refuses to launch
	// rather than proceed with a partial protected set. ResolveGrants is
	// the real launch-time entry point, so the refusal must surface here.
	g, err := ResolveGrants(prof, workdir, nil)
	if err == nil {
		// Diagnose: confirm the scan was actually incomplete (the late
		// protected target is absent from the protected set), so the
		// failure message reports the concrete gap rather than a bare
		// "expected refusal".
		for _, p := range g.ProtectedPaths {
			if p == protectedFile {
				t.Fatalf("protection scan completed for %s but launch was not refused; an incomplete scan must fail closed", protectedFile)
			}
		}
		t.Fatalf("launch proceeded after an incomplete protection scan (%s unmasked); expected refusal", protectedFile)
	}
}

// populateLargeTree fills root with more entries than the protection scan
// can enumerate, so the scan is guaranteed to be incomplete. It is laid out
// as many small directories whose names sort before "zzz-*", keeping the
// protected target past the enumeration bound. Directory reads are kept
// cheap by spreading entries across many dirs.
func populateLargeTree(t *testing.T, root string) {
	t.Helper()
	const dirs = 400
	const perDir = 505

	workers := 16
	jobs := make(chan int, workers*2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				dir := filepath.Join(root, fmt.Sprintf("g%04d", d))
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Errorf("mkdir %s: %v", dir, err)
					return
				}
				for f := 0; f < perDir; f++ {
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("n%05d", f)), nil, 0o644); err != nil {
						t.Errorf("write %s: %v", dir, err)
						return
					}
				}
			}
		}()
	}
	for d := 0; d < dirs; d++ {
		jobs <- d
	}
	close(jobs)
	wg.Wait()
}
