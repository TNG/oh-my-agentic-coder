package sandboxrun

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSecurityLearnPromptStripsControlSequences asserts that a learn-mode
// candidate path containing terminal control sequences cannot repaint the
// operator's terminal or forge extra prompt lines.
//
// OfferLearnedFolders prints each candidate with a plain fmt.Fprintf, with
// no escaping — unlike the sibling intent prompt, which renders with %q.
func TestSecurityLearnPromptStripsControlSequences(t *testing.T) {
	var out bytes.Buffer
	payload := "innocent\x1b[2J\rFAKE — approved automatically"

	if err := OfferLearnedFolders("", []string{payload}, strings.NewReader("N\n"), &out, ""); err != nil {
		t.Fatalf("OfferLearnedFolders: %v", err)
	}

	rendered := out.String()

	// Control: the candidate is shown at all (declining doesn't suppress
	// the listing), so a fix that stops listing candidates altogether
	// wouldn't make this test pass for the wrong reason.
	if !strings.Contains(rendered, "innocent") {
		t.Fatalf("control: the candidate was not rendered at all (%q): the fixture is broken, not the security property", rendered)
	}

	if strings.ContainsRune(rendered, 0x1b) || strings.ContainsRune(rendered, '\r') {
		t.Errorf("learn-mode prompt output contains raw control bytes from a candidate path: %q", rendered)
	}
}

func securityLearnRecorder(home string, protected []string, excluded ...string) *learnRecorder {
	r := &learnRecorder{
		seen:      map[string]bool{},
		stop:      make(chan struct{}),
		protected: protected,
		home:      home,
	}
	r.excluded = append(r.excluded, excluded...)
	return r
}

func learnCandidateOverlaps(candidate, protected string) bool {
	return candidate == protected ||
		strings.HasPrefix(candidate, protected+string(filepath.Separator)) ||
		strings.HasPrefix(protected, candidate+string(filepath.Separator))
}

func requireNoLearnedOverlap(t *testing.T, candidates []string, protected ...string) {
	t.Helper()
	for _, c := range candidates {
		for _, p := range protected {
			if learnCandidateOverlaps(c, p) {
				t.Errorf("learned folder %q overlaps protected path %q (candidates: %v)", c, p, candidates)
			}
		}
	}
}

func TestSecurityLearnRejectsSymlinkedProtectedFolder(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "managed", "credentials")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(home, ".keys")
	if err := os.Symlink(target, protected); err != nil {
		t.Fatal(err)
	}

	plainProtected := filepath.Join(home, ".tokens")
	if err := os.MkdirAll(plainProtected, 0o700); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(home, "scratch", "project")

	rec := securityLearnRecorder(home, []string{protected, plainProtected})
	rec.record(target)
	rec.record(plainProtected)
	rec.record(control)

	got := rec.candidates()
	requireNoLearnedOverlap(t, got, target, protected, plainProtected)
	if !slices.Contains(got, control) {
		t.Errorf("control folder %q was not offered; candidates: %v", control, got)
	}
}

func TestSecurityLearnRejectsSymlinkAncestorOfProtectedFolder(t *testing.T) {
	home := t.TempDir()
	protected := filepath.Join(home, "managed", ".keys")
	if err := os.MkdirAll(protected, 0o700); err != nil {
		t.Fatal(err)
	}

	parent := filepath.Dir(protected)
	ancestor := filepath.Join(home, "parent-link")
	if err := os.Symlink(parent, ancestor); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(home, "scratch", "project")

	rec := securityLearnRecorder(home, []string{protected})
	rec.record(ancestor)
	rec.record(control)

	got := rec.candidates()
	requireNoLearnedOverlap(t, got, protected, parent, ancestor)
	if !slices.Contains(got, control) {
		t.Errorf("control folder %q was not offered; candidates: %v", control, got)
	}
}

func TestSecurityLearnRechecksProtectedAfterProjectRoot(t *testing.T) {
	home := t.TempDir()
	protected := filepath.Join(home, "work", "area", "other", ".keys")
	if err := os.MkdirAll(protected, 0o700); err != nil {
		t.Fatal(err)
	}

	observed := filepath.Join(home, "work", "area", "other", "deep", "deeper")
	control := filepath.Join(home, "scratch", "project", "src")
	controlObserved := filepath.Join(control, "deep", "deeper")

	rec := securityLearnRecorder(home, []string{protected})
	rec.record(observed)
	rec.record(controlObserved)

	got := rec.candidates()
	requireNoLearnedOverlap(t, got, protected)
	if !slices.Contains(got, control) {
		t.Errorf("control folder %q was not offered; candidates: %v", control, got)
	}
}
