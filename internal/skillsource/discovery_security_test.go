package skillsource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecuritySkillNameValidatedAtDiscovery asserts that Discover is the
// trust boundary for skill directory names: a directory whose name is not a
// valid skill identifier must not be surfaced to any downstream consumer
// (registry, manifest, refusal reports). Discovery runs over workdir roots
// the confined agent can write to, so accepting any on-disk name here would
// publish attacker-chosen bytes into every sink that consumes the result.
func TestSecuritySkillNameValidatedAtDiscovery(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	root := filepath.Join(wd, ".opencode", "skills")

	stageSkill(t, root, "real-skill")
	// Legal on-disk directory names that are not valid skill identifiers.
	invalid := []string{
		"bad\x1bname",
		"Bad Name",
		"a.b",
	}
	for _, n := range invalid {
		stageSkill(t, root, n)
	}

	got, err := Discover(wd, ocHarness(t))
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	seen := map[string]bool{}
	for _, e := range got {
		seen[e.Name] = true
	}
	if !seen["real-skill"] {
		t.Fatalf("control: a valid skill was not discovered; fixture is broken, not the security property: %+v", got)
	}
	for _, n := range invalid {
		if seen[n] {
			t.Errorf("Discover surfaced a directory name that is not a valid skill identifier: %q", n)
		}
	}
}

// TestDiscoverWithSkippedReportsInvalidNames checks that a skill folder
// with an invalid name is reported instead of vanishing silently, and that
// a folder without omac.yaml (not a skill) is not reported. The same
// folder name in a second skill root is reported only once.
func TestDiscoverWithSkippedReportsInvalidNames(t *testing.T) {
	withFakeHome(t)
	wd := t.TempDir()
	root := filepath.Join(wd, ".opencode", "skills")
	stageSkill(t, root, "real-skill")
	stageSkill(t, root, "My_Skill")
	stageSkill(t, filepath.Join(wd, ".agents", "skills"), "My_Skill")
	if err := os.MkdirAll(filepath.Join(root, "Notes_Not_A_Skill"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, skipped, err := DiscoverWithSkipped(wd, ocHarness(t))
	if err != nil {
		t.Fatalf("DiscoverWithSkipped: %v", err)
	}
	if len(got) != 1 || got[0].Name != "real-skill" {
		t.Errorf("entries = %+v, want only real-skill", got)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %+v, want exactly My_Skill", skipped)
	}
	s := skipped[0]
	if s.Name != "My_Skill" || s.Kind != "workdir" || s.Dir != filepath.Join(root, "My_Skill") || !strings.Contains(s.Reason, "lowercase letters") {
		t.Errorf("skipped[0] = %+v", s)
	}
}
