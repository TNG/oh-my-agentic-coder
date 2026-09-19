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

	// Build a granted workdir large enough that the launch-time protection
	// scan's enumeration bound is reached. The tree holds a real protected
	// basename file so the scan has a concrete target it is responsible for.
	if err := os.Mkdir(filepath.Join(workdir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "configs", ".env"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	populateLargeTree(t, workdir)

	prof := &sandboxprofile.Profile{
		Workdir:  sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network:  sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}

	// Control: a small workdir resolves without error, proving any refusal
	// below is caused by the unbounded tree, not by the test setup.
	controlProf := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	if _, err := ResolveGrants(controlProf, t.TempDir(), nil); err != nil {
		t.Fatalf("control: small workdir should resolve, got %v", err)
	}

	g, err := ResolveGrants(prof, workdir, nil)
	if err == nil {
		// The scan could not complete, yet launch proceeded. Confirm the
		// concrete protected target was left out of the protected set, then
		// fail: an incomplete scan must refuse to start.
		want := filepath.Join(workdir, "configs", ".env")
		for _, p := range g.ProtectedPaths {
			if p == want {
				t.Fatalf("launch proceeded after an incomplete protection scan; expected refusal")
			}
		}
		t.Errorf("launch proceeded after an incomplete protection scan (protected target %s unmasked); expected ResolveGrants to refuse", want)
	}
}

// populateLargeTree fills root with more entries than the protection scan
// can enumerate, so the scan is guaranteed to be incomplete. It is laid out
// as many small directories to keep directory reads cheap.
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
