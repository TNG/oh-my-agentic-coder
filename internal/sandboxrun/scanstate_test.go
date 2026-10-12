package sandboxrun

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// Users updating from an omac without the scan state have no file. That
// must behave as before: nothing accepted, nothing known, and no file is
// created while there is nothing to remember.
func TestScanStateMissingFileBehavesAsBefore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "default.scan.json")
	s, err := loadScanState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.canSave() || s.isAccepted("/data") || len(s.knownSecrets()) > 0 {
		t.Fatalf("missing file must be an empty, writable state: %+v", s)
	}
	if err := s.save([]string{t.TempDir()}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("nothing to remember, but %s was written (err %v)", path, err)
	}
}

// A file this omac cannot read, e.g. written by a newer omac with another
// schema, is ignored and never overwritten. Launch then asks as before.
func TestScanStateUnreadableFileIsLeftAlone(t *testing.T) {
	for name, content := range map[string]string{
		"newer schema": `{"schema": 2, "accepted_incomplete": [{"path": "/data"}]}`,
		"broken":       `{"schema": 1,`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "default.scan.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			s, err := loadScanState(path)
			if err == nil {
				t.Fatal("expected an error for an unreadable file")
			}
			if s.canSave() || s.isAccepted("/data") {
				t.Fatalf("unreadable file must give an empty, read-only state: %+v", s)
			}
			s.acceptAlways([]string{"/data"})
			if err := s.save([]string{"/data"}, []string{"/data/.env"}); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(path); string(got) != content {
				t.Errorf("file was rewritten:\n%s", got)
			}
		})
	}
}

// Two launches that loaded the file at the same time must not drop each
// other's entries. Known secrets inside the folders a launch scanned are
// replaced by what it found (a deleted .env drops out); others are kept.
func TestScanStateSaveMerges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "default.scan.json")
	p1, p2 := t.TempDir(), t.TempDir()
	a, _ := loadScanState(path)
	b, _ := loadScanState(path)

	a.acceptAlways([]string{p1})
	if err := a.save([]string{p1}, []string{filepath.Join(p1, ".env")}); err != nil {
		t.Fatal(err)
	}
	b.acceptAlways([]string{p2})
	if err := b.save([]string{p2}, []string{filepath.Join(p2, ".env")}); err != nil {
		t.Fatal(err)
	}
	got, err := loadScanState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.isAccepted(p1) || !got.isAccepted(p2) {
		t.Errorf("both accepted folders must be kept: %+v", got.file.AcceptedIncomplete)
	}
	if want := []string{filepath.Join(p1, ".env"), filepath.Join(p2, ".env")}; !slices.Equal(got.knownSecrets(), dedupe(want)) {
		t.Errorf("known secrets = %v, want %v", got.knownSecrets(), want)
	}

	// p1's .env is gone: a later scan of p1 finds nothing.
	if err := got.save([]string{p1}, nil); err != nil {
		t.Fatal(err)
	}
	again, _ := loadScanState(path)
	if want := []string{filepath.Join(p2, ".env")}; !slices.Equal(again.knownSecrets(), want) {
		t.Errorf("known secrets = %v, want %v", again.knownSecrets(), want)
	}
}

// TestMayStartAfterDenyScanRemembersAlways checks the "a" answer across
// launches: the next launch starts without asking, even without a
// terminal, and only a folder not accepted yet is asked about.
func TestMayStartAfterDenyScanRemembersAlways(t *testing.T) {
	path := filepath.Join(t.TempDir(), "default.scan.json")
	data, other := t.TempDir(), t.TempDir()
	flags, err := sandboxprofile.ParseFlags([]string{"--", "true"})
	if err != nil {
		t.Fatal(err)
	}

	first, _ := loadScanState(path)
	var out bytes.Buffer
	if !mayStartAfterDenyScan(&out, strings.NewReader("a\n"), true, flags, []string{data}, first, t.Logf) {
		t.Fatalf("answer a must start; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("must say where the answer is saved, got:\n%s", out.String())
	}
	if err := first.save(nil, nil); err != nil {
		t.Fatal(err)
	}

	next, err := loadScanState(path)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	var logged bytes.Buffer
	logf := func(format string, args ...any) { logged.WriteString(format) }
	if !mayStartAfterDenyScan(&out, strings.NewReader(""), false, flags, []string{data}, next, logf) {
		t.Fatalf("an accepted folder must start without a terminal; output:\n%s", out.String())
	}
	if out.Len() > 0 {
		t.Errorf("an accepted folder must not warn on the terminal, got:\n%s", out.String())
	}
	if logged.Len() == 0 {
		t.Error("starting without asking must be noted in the log")
	}

	out.Reset()
	if mayStartAfterDenyScan(&out, strings.NewReader(""), false, flags, []string{data, other}, next, t.Logf) {
		t.Fatal("a folder not accepted yet must still need an answer")
	}
	if !strings.Contains(out.String(), other) || strings.Contains(out.String(), data) {
		t.Errorf("must ask about %s only, got:\n%s", other, out.String())
	}
}

// TestResolveGrantsKeepsKnownSecretAfterFlood covers an agent that adds
// over 200,000 files in front of an existing .env so the next scan stops
// before reaching it. The .env found by an earlier launch stays hidden,
// unless it was deleted or override_deny now lifts it.
func TestResolveGrantsKeepsKnownSecretAfterFlood(t *testing.T) {
	workdir := t.TempDir()
	env := filepath.Join(mkdirAll(t, workdir, "zzz-secrets"), ".env")
	writeFile(t, env)
	prof := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}

	before, err := resolveGrants(prof, workdir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(before.FoundSecrets, env) || !slices.Equal(before.DenyScanRoots, []string{workdir}) {
		t.Fatalf("first launch must find %s in %s: found %v, roots %v", env, workdir, before.FoundSecrets, before.DenyScanRoots)
	}

	populateLargeTree(t, workdir) // "g0000".. sorts before "zzz-secrets"
	known := before.FoundSecrets

	without, err := resolveGrants(prof, workdir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(without.ProtectedPaths, env) {
		t.Fatal("test setup: the flooded scan must not reach the .env on its own")
	}

	with, err := resolveGrants(prof, workdir, nil, known)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(with.IncompleteDenyScanRoots, []string{workdir}) {
		t.Fatalf("IncompleteDenyScanRoots = %v, want [%s]", with.IncompleteDenyScanRoots, workdir)
	}
	if !slices.Contains(with.ProtectedPaths, env) || !slices.Contains(with.FoundSecrets, env) {
		t.Errorf("known secret %s must stay hidden and stay remembered", env)
	}

	lifted := *prof
	lifted.Filesystem.OverrideDeny = []string{".env"}
	if g, err := resolveGrants(&lifted, workdir, nil, known); err != nil || slices.Contains(g.ProtectedPaths, env) {
		t.Errorf("override_deny .env must lift the known secret (err %v)", err)
	}

	if err := os.Remove(env); err != nil {
		t.Fatal(err)
	}
	if g, err := resolveGrants(prof, workdir, nil, known); err != nil || slices.Contains(g.ProtectedPaths, env) || slices.Contains(g.FoundSecrets, env) {
		t.Errorf("a deleted known secret must be dropped (err %v)", err)
	}
}

func TestScanStatePath(t *testing.T) {
	if got := sandboxprofile.ScanStatePath("/c/sandbox-profiles/default.json"); got != "/c/sandbox-profiles/default.scan.json" {
		t.Errorf("ScanStatePath = %q", got)
	}
	if got := sandboxprofile.ScanStatePath(""); got != "" {
		t.Errorf("ScanStatePath(\"\") = %q, want \"\"", got)
	}
}
