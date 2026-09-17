//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxrun"
)

// The per-test harness HOME (a temp-dir sibling of the workdir) must stay
// reachable inside the sandbox: after the baseline tmp hardening removed the
// Linux /tmp grants, node-based harnesses (pi, codex) otherwise fail to
// resolve their nested node_modules at launch.
func TestE2EProfileGrantsHarnessHome(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "001")
	work := filepath.Join(base, "002")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	h, ok := harnessByName("pi")
	if !ok {
		t.Fatal("pi harness not found")
	}
	writeSandboxProfile(t, home, h, nil)

	data, err := os.ReadFile(filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json"))
	if err != nil {
		t.Fatal(err)
	}
	var profile sandboxprofile.Profile
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	grants, err := sandboxrun.ResolveGrants(&profile, work, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	argv, err := sandboxrun.BuildBwrapArgv(grants, []string{"true"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range argv {
		if a == home {
			return
		}
	}
	t.Fatalf("harness home %s not mounted in bwrap argv:\n%s", home, argv)
}
