package sandboxrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecurityMarkerDirNameCannotEscape asserts that a hostile
// Denial.marker_dir_name value cannot make prepareMarkers write outside the
// throwaway marker directory it just created.
//
// dirFileName comes straight from the profile's denial.marker_dir_name field
// (Profile.Validate never inspects it) and is joined onto markerDir with a
// bare filepath.Join before os.WriteFile — so "../../../../victim.txt"
// resolves outside markerDir entirely. Since prepareMarkers writes to the
// host as the launching user before the sandbox exists, this test points the
// traversal at a path inside its own t.TempDir() rather than a real host
// file.
func TestSecurityMarkerDirNameCannotEscape(t *testing.T) {
	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "victim.txt")

	// Enough "../" segments to clamp to "/" regardless of markerDir's
	// actual (unpredictable, MkdirTemp-derived) depth; filepath.Clean then
	// appends the remainder verbatim, landing exactly on victim.
	traversal := strings.Repeat("../", 40) + strings.TrimPrefix(victimDir, string(filepath.Separator)) + "/victim.txt"

	// Control: with the default marker filename, prepareMarkers does write
	// a marker file inside its own directory — so a fix that disables
	// marker writing altogether wouldn't make this test pass by accident.
	control := &Grants{DenialText: "denied", ProtectedPaths: []string{"/nonexistent/.ssh"}}
	cleanup, err := control.prepareMarkers()
	if err != nil {
		t.Fatalf("control: prepareMarkers with a default marker name failed: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(control.markerDir, markerDirFileName)); statErr != nil {
		cleanup()
		t.Fatalf("control: no marker file was created inside markerDir (%v): the fixture is broken, not the security property", statErr)
	}
	cleanup()

	g := &Grants{
		DenialText:     "denied",
		ProtectedPaths: []string{"/nonexistent/.ssh"},
		DenialDirName:  traversal,
	}
	cleanup, err = g.prepareMarkers()
	defer cleanup()
	if err == nil {
		if _, statErr := os.Stat(victim); statErr == nil {
			t.Errorf("prepareMarkers wrote a file at %s via DenialDirName %q, escaping the marker directory it created for this launch", victim, traversal)
		}
	}
}
