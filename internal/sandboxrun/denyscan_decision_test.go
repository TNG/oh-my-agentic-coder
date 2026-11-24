package sandboxrun

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

type recordedAsk struct {
	roots []string
	limit int64
}

// scriptedAsk answers with choices in order and records every call.
func scriptedAsk(calls *[]recordedAsk, choices ...ScanChoice) ScanAsk {
	return func(roots []string, limit int64) ScanChoice {
		*calls = append(*calls, recordedAsk{roots: slices.Clone(roots), limit: limit})
		if len(*calls) > len(choices) {
			return ScanAbort
		}
		return choices[len(*calls)-1]
	}
}

func profileWithReadRoots(limit int64, roots ...string) *sandboxprofile.Profile {
	return &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{
			Read:     roots,
			DenyScan: &sandboxprofile.DenyScan{MaxEntries: int64Ptr(limit)},
		},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
}

// lateEnvTree builds root/{1000 files, d1/{1000 files, d2/{400 files, .env}}}:
// with a limit of 1000 the .env is only reached at a limit of 3000.
func lateEnvTree(t *testing.T) (root, env string) {
	t.Helper()
	root = canonicalPath(t.TempDir())
	writeFiles(t, root, 1000)
	writeFiles(t, filepath.Join(root, "d1"), 1000)
	d2 := filepath.Join(root, "d1", "d2")
	writeFiles(t, d2, 400)
	return root, writeEnv(t, d2)
}

func TestScanProtectedContinueFindsLateEnv(t *testing.T) {
	setDenyByName(t, false)
	root, env := lateEnvTree(t)
	var calls []recordedAsk

	s, err := ScanProtected(profileWithReadRoots(1000, root), root, "", scriptedAsk(&calls, ScanContinue, ScanContinue))
	if err != nil {
		t.Fatalf("ScanProtected: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("ask called %d times, want 2: %+v", len(calls), calls)
	}
	if calls[0].limit != 1000 || calls[1].limit != 2000 {
		t.Errorf("ask limits = %d, %d; want 1000, 2000", calls[0].limit, calls[1].limit)
	}
	if s.Limit != 3000 || !s.Continued {
		t.Errorf("Limit = %d, Continued = %v; want 3000, true", s.Limit, s.Continued)
	}
	if !slices.Contains(s.Matches, env) {
		t.Errorf("%s not in Matches %v", env, s.Matches)
	}
	if got := s.Entries[root]; got <= 2000 || got > 3000 {
		t.Errorf("Entries[root] = %d, want the last pass's count between 2000 and 3000", got)
	}
}

func TestScanProtectedContinueReusesCache(t *testing.T) {
	setDenyByName(t, false)
	steadyState(t)
	root := canonicalPath(t.TempDir())
	writeFiles(t, filepath.Join(root, "a"), 600)
	writeFiles(t, filepath.Join(root, "b"), 600)
	var calls []recordedAsk

	s, err := ScanProtected(profileWithReadRoots(1000, root), root, "", scriptedAsk(&calls, ScanContinue))
	if err != nil {
		t.Fatalf("ScanProtected: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("ask called %d times, want 1", len(calls))
	}
	if skips := s.cache.state(root).skips.Load(); skips == 0 {
		t.Error("second pass re-read every folder; want unchanged folders skipped via the cache")
	}
}

func TestScanProtectedStartIncomplete(t *testing.T) {
	setDenyByName(t, false)
	root, _ := lateEnvTree(t)
	var calls []recordedAsk

	s, err := ScanProtected(profileWithReadRoots(1000, root), root, "", scriptedAsk(&calls, ScanStartIncomplete))
	if err != nil {
		t.Fatalf("ScanProtected: %v", err)
	}
	if !slices.Equal(s.Accepted, []string{root}) {
		t.Errorf("Accepted = %v, want [%s]", s.Accepted, root)
	}
	flags := strings.Join(s.SandboxFlags(), " ")
	for _, want := range []string{"--deny-scan-limit 1000", "--deny-scan-accept-incomplete " + root} {
		if !strings.Contains(flags, want) {
			t.Errorf("SandboxFlags() = %q, missing %q", flags, want)
		}
	}
	notes := strings.Join(s.SessionEndNotes(""), "\n")
	if !strings.Contains(notes, "these folders were not fully protected this session: "+root) {
		t.Errorf("SessionEndNotes = %q, want the incomplete-protection reminder", notes)
	}
}

func TestScanProtectedNoTerminal(t *testing.T) {
	setDenyByName(t, false)
	root, _ := lateEnvTree(t)

	s, err := ScanProtected(profileWithReadRoots(1000, root), root, "/p/default.json", nil)
	var incomplete *IncompleteScanError
	if !errors.As(err, &incomplete) {
		t.Fatalf("err = %v (%T), want *IncompleteScanError", err, err)
	}
	if s != nil {
		t.Errorf("result = %+v, want nil on error", s)
	}
	if incomplete.GrewDuringStart {
		t.Error("GrewDuringStart set for a first scan without a terminal")
	}
	if !slices.Equal(incomplete.Roots, []string{root}) || incomplete.Limit != 1000 || incomplete.ProfilePath != "/p/default.json" {
		t.Errorf("error = %+v, want root %s, limit 1000, profile /p/default.json", incomplete, root)
	}
}

func TestScanProtectedAbort(t *testing.T) {
	setDenyByName(t, false)
	root, _ := lateEnvTree(t)
	var calls []recordedAsk

	s, err := ScanProtected(profileWithReadRoots(1000, root), root, "", scriptedAsk(&calls, ScanAbort))
	if !errors.Is(err, errScanAborted) {
		t.Fatalf("err = %v, want errScanAborted", err)
	}
	if s != nil {
		t.Errorf("result = %+v, want nil", s)
	}
	if err.Error() != "the .env scan was aborted, so nothing was started" {
		t.Errorf("err text = %q", err.Error())
	}
}

func TestScanProtectedMultipleRoots(t *testing.T) {
	setDenyByName(t, false)
	a := canonicalPath(t.TempDir())
	b := canonicalPath(t.TempDir())
	writeFiles(t, a, 1500)
	writeFiles(t, b, 1500)
	var calls []recordedAsk

	if _, err := ScanProtected(profileWithReadRoots(1000, a, b), a, "", scriptedAsk(&calls, ScanStartIncomplete)); err != nil {
		t.Fatalf("ScanProtected: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("ask called %d times, want one question for both roots", len(calls))
	}
	got := slices.Sorted(slices.Values(calls[0].roots))
	want := slices.Sorted(slices.Values([]string{a, b}))
	if !slices.Equal(got, want) {
		t.Errorf("ask roots = %v, want %v", got, want)
	}
}

func TestScanProtectedNameRuleRootNoPrompt(t *testing.T) {
	setDenyByName(t, true)
	a := canonicalPath(t.TempDir())
	b := canonicalPath(t.TempDir())
	writeFiles(t, a, 1500)
	writeFiles(t, b, 1500)
	p := profileWithReadRoots(1000, a, b)
	p.Filesystem.OverrideDeny = []string{filepath.Join(b, ".env")}
	var calls []recordedAsk

	s, err := ScanProtected(p, a, "", scriptedAsk(&calls, ScanStartIncomplete))
	if err != nil {
		t.Fatalf("ScanProtected: %v", err)
	}
	if len(calls) != 1 || !slices.Equal(calls[0].roots, []string{b}) {
		t.Fatalf("ask calls = %+v, want one call with only %s", calls, b)
	}
	if !slices.Equal(s.ByNameRoots, []string{a}) {
		t.Errorf("ByNameRoots = %v, want [%s]", s.ByNameRoots, a)
	}
}

func TestScanProtectedNoPromptUnderLimit(t *testing.T) {
	setDenyByName(t, false)
	root := canonicalPath(t.TempDir())
	writeFiles(t, root, 10)
	env := writeEnv(t, filepath.Join(root, "x"))
	var calls []recordedAsk

	s, err := ScanProtected(profileWithReadRoots(1000, root), root, "", scriptedAsk(&calls))
	if err != nil {
		t.Fatalf("ScanProtected: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("ask called %d times for a tree under the limit", len(calls))
	}
	if !slices.Contains(s.Matches, env) {
		t.Errorf("%s not in Matches %v", env, s.Matches)
	}
	if notes := s.SessionEndNotes(""); len(notes) != 0 {
		t.Errorf("SessionEndNotes = %q, want none", notes)
	}
	if flags := s.SandboxFlags(); !slices.Equal(flags, []string{"--deny-scan-limit", "1000"}) {
		t.Errorf("SandboxFlags() = %q, want only the limit", flags)
	}
}

func TestSessionEndNotesRounding(t *testing.T) {
	root := "/home/x/monorepo"
	s := &ProtectedScan{Continued: true, Entries: map[string]int64{root: 2_400_000}, asked: []string{root}}

	notes := strings.Join(s.SessionEndNotes("/p/default.json"), "\n")
	for _, want := range []string{
		"the .env scan of /home/x/monorepo needed 2,400,000 entries",
		"Set filesystem.deny_scan.max_entries to 3000000 in /p/default.json",
	} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes = %q, missing %q", notes, want)
		}
	}

	exact := &ProtectedScan{Continued: true, Entries: map[string]int64{root: 2_000_000}, asked: []string{root}}
	if notes := strings.Join(exact.SessionEndNotes(""), "\n"); !strings.Contains(notes, "max_entries to 2000000 in your sandbox profile") {
		t.Errorf("notes = %q, want an exact million kept and the generic profile name", notes)
	}
}
