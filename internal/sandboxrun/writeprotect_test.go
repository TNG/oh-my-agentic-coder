package sandboxrun

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/netprompt"
)

func TestWriteProtectProfilePaths(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "sandbox.json")
	if err := os.WriteFile(profile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Both the profile and its pages sibling come back; the pages file is
	// created empty if missing.
	got, err := writeProtectProfilePaths(profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{profile, filepath.Join(dir, "sandbox.pages.json")}
	if !slices.Equal(got, want) {
		t.Errorf("writeProtectProfilePaths = %v, want %v", got, want)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sandbox.pages.json"))
	if err != nil {
		t.Fatalf("pages file not created: %v", err)
	}
	if len(data) == 0 {
		t.Error("pages file must be created with a valid empty store, not 0 bytes")
	}

	// A second call leaves the existing pages file untouched (idempotent).
	if err := os.WriteFile(filepath.Join(dir, "sandbox.pages.json"), []byte("{\"schema\":99}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeProtectProfilePaths(profile, nil); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(dir, "sandbox.pages.json"))
	if string(data) != "{\"schema\":99}" {
		t.Errorf("existing pages file must not be overwritten, got %q", data)
	}

	// A path covered by a deny is dropped: deny is stricter.
	got, err = writeProtectProfilePaths(profile, []string{profile})
	if err != nil {
		t.Fatal(err)
	}
	want = []string{filepath.Join(dir, "sandbox.pages.json")}
	if !slices.Equal(got, want) {
		t.Errorf("denied profile must be dropped, got %v, want %v", got, want)
	}
}

// The empty store EnsureLearnedPolicyFile writes must load back cleanly,
// otherwise every first launch would warn about an unparseable page policy.
func TestWriteProtectProfilePathsPagesLoads(t *testing.T) {
	dir := t.TempDir()
	profile := filepath.Join(dir, "p.json")
	if err := os.WriteFile(profile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeProtectProfilePaths(profile, nil); err != nil {
		t.Fatal(err)
	}
	lp, err := netprompt.LoadLearnedPolicy(filepath.Join(dir, "p.pages.json"))
	if err != nil {
		t.Fatalf("created pages file does not load: %v", err)
	}
	if entries := lp.Entries(); len(entries) != 0 {
		t.Errorf("created pages file must be empty, got %v", entries)
	}
}
