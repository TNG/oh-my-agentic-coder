package skillsource

import (
	"path/filepath"
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
