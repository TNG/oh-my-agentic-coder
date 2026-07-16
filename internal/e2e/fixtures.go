//go:build e2e || e2e_fast

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildOmac compiles the omac binary into a temp dir and returns its path.
//
// Lives in the e2e_fast build set (not just the full e2e build) because the
// model-free regression tests — serve_isolation_test.go in particular —
// drive the real compiled binary as a subprocess and must be runnable at
// PR time without a live LLM harness.
func buildOmac(t *testing.T) string {
	t.Helper()
	// OMAC_BIN lets a caller supply a prebuilt binary instead of compiling from
	// source — used by the "E2E: drift" omac@release matrix leg to exercise the
	// latest RELEASED omac against the latest harnesses. Unset ⇒ build main.
	if bin := os.Getenv("OMAC_BIN"); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			t.Fatalf("OMAC_BIN=%q not usable: %v", bin, err)
		}
		t.Logf("using prebuilt omac binary from OMAC_BIN=%s", bin)
		return bin
	}
	binPath := filepath.Join(t.TempDir(), "omac")
	// Build from repo root (test CWD is internal/e2e/).
	repoRoot := filepath.Join("..", "..")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, "./cmd/omac")
	cmd.Dir = repoRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build omac: %v\n%s", err, out)
	}
	return binPath
}
