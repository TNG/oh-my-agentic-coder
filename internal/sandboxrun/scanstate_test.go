package sandboxrun

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// Users updating from an omac without the scan state have no file. That
// must behave as before: nothing accepted, nothing known, and nothing is
// created (not even the lock) while there is nothing to remember.
func TestScanStateMissingFileBehavesAsBefore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "default.scan.json")
	s, err := loadScanState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !s.canSave() || s.isAccepted("/data") || len(s.knownSecrets()) > 0 {
		t.Fatalf("missing file must be an empty, writable state: %+v", s)
	}
	if err := s.save([]string{t.TempDir()}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 0 {
		t.Errorf("nothing to remember, but files were created: %v", entries)
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
			if err := s.save([]string{"/data"}, []string{"/data/.env"}, nil); err != nil {
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
// replaced by what it found or kept (a deleted .env drops out); others
// are left alone.
func TestScanStateSaveMerges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "default.scan.json")
	p1, p2 := resolvedPath(t.TempDir()), resolvedPath(t.TempDir())
	a, _ := loadScanState(path)
	b, _ := loadScanState(path)

	a.acceptAlways([]string{p1})
	if err := a.save([]string{p1}, []string{filepath.Join(p1, ".env")}, nil); err != nil {
		t.Fatal(err)
	}
	b.acceptAlways([]string{p2})
	if err := b.save([]string{p2}, []string{filepath.Join(p2, ".env")}, []string{filepath.Join(p2, "kept", ".env")}); err != nil {
		t.Fatal(err)
	}
	got, err := loadScanState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.isAccepted(p1) || !got.isAccepted(p2) {
		t.Errorf("both accepted folders must be kept: %+v", got.file.AcceptedIncomplete)
	}
	want := []string{filepath.Join(p1, ".env"), filepath.Join(p2, ".env"), filepath.Join(p2, "kept", ".env")}
	if !slices.Equal(got.knownSecrets(), dedupe(want)) {
		t.Errorf("known secrets = %v, want %v", got.knownSecrets(), want)
	}

	// p1's .env is gone: a later scan of p1 finds nothing.
	if err := got.save([]string{p1}, nil, nil); err != nil {
		t.Fatal(err)
	}
	again, _ := loadScanState(path)
	if want := want[1:]; !slices.Equal(again.knownSecrets(), want) {
		t.Errorf("known secrets = %v, want %v", again.knownSecrets(), want)
	}
}

// Past maxKnownSecrets, files remembered earlier win over new ones, so an
// agent that creates many .env files cannot push the real ones out.
func TestMergeKnownSecretsKeepsOldEntriesWhenFull(t *testing.T) {
	root := "/p"
	real := "/p/zzz/.env"
	var found []string
	for i := range maxKnownSecrets + 10 {
		found = append(found, fmt.Sprintf("/p/a%05d/.env", i))
	}
	found = append(found, real)
	got := mergeKnownSecrets([]string{real}, []string{root}, found, nil)
	if len(got) != maxKnownSecrets {
		t.Fatalf("len = %d, want %d", len(got), maxKnownSecrets)
	}
	if !slices.Contains(got, real) {
		t.Errorf("the earlier entry %s was pushed out", real)
	}
}

// An "always" answer covers exactly the folder it was given for: not a
// folder inside it, and not another folder an agent swaps in by replacing
// the accepted one with a symlink.
func TestScanStateAcceptsOnlyTheSameFolder(t *testing.T) {
	home := t.TempDir()
	data := mkdirAll(t, home, "data")
	other := mkdirAll(t, home, "secretrepo")
	s, _ := loadScanState(filepath.Join(t.TempDir(), "default.scan.json"))
	s.file.AcceptedIncomplete = []acceptedFolder{{Path: resolvedPath(data)}}

	if !s.isAccepted(data) {
		t.Fatal("the accepted folder itself must count")
	}
	if s.isAccepted(mkdirAll(t, data, "sub")) {
		t.Error("a folder inside the accepted one must not count")
	}
	if err := os.Rename(data, data+".bak"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, data); err != nil {
		t.Fatal(err)
	}
	if s.isAccepted(other) || s.isAccepted(data) {
		t.Error("an acceptance must not carry over through a swapped-in symlink")
	}
}

// TestStartAfterDenyScanRemembersAlways checks the "a" answer across
// launches: the next launch starts without asking, even without a
// terminal, and only a folder not accepted yet is asked about.
func TestStartAfterDenyScanRemembersAlways(t *testing.T) {
	path := filepath.Join(t.TempDir(), "default.scan.json")
	data, other := t.TempDir(), t.TempDir()
	flags, err := sandboxprofile.ParseFlags([]string{"--", "true"})
	if err != nil {
		t.Fatal(err)
	}
	grants := func(roots ...string) *Grants {
		return &Grants{IncompleteDenyScanRoots: roots, DenyScanRoots: roots}
	}

	first, _ := loadScanState(path)
	var out bytes.Buffer
	if !startAfterDenyScan(&out, strings.NewReader("a\n"), true, flags, grants(data), first, t.Logf) {
		t.Fatalf("answer a must start; output:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "saved in "+path) {
		t.Errorf("must say where the answer is saved, got:\n%s", out.String())
	}

	next, err := loadScanState(path)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	var logged bytes.Buffer
	logf := func(format string, args ...any) { fmt.Fprintf(&logged, format, args...) }
	if !startAfterDenyScan(&out, strings.NewReader(""), false, flags, grants(data), next, logf) {
		t.Fatalf("an accepted folder must start without a terminal; output:\n%s", out.String())
	}
	if out.Len() > 0 {
		t.Errorf("an accepted folder must not warn on the terminal, got:\n%s", out.String())
	}
	if !strings.Contains(logged.String(), data) {
		t.Errorf("starting without asking must be noted in the log, got %q", logged.String())
	}

	out.Reset()
	if startAfterDenyScan(&out, strings.NewReader(""), false, flags, grants(data, other), next, t.Logf) {
		t.Fatal("a folder not accepted yet must still need an answer")
	}
	if !strings.Contains(out.String(), other) || strings.Contains(out.String(), data) {
		t.Errorf("must ask about %s only, got:\n%s", other, out.String())
	}

	// A remembered secret that is gone from an accepted folder may have
	// been moved past the point where the scan stops: ask again.
	out.Reset()
	g := grants(data)
	g.MissingSecrets = []string{filepath.Join(data, "secrets", ".env")}
	if startAfterDenyScan(&out, strings.NewReader(""), false, flags, g, next, t.Logf) {
		t.Fatal("a missing known secret must override the earlier acceptance")
	}
	if !strings.Contains(out.String(), g.MissingSecrets[0]) || !strings.Contains(out.String(), data) {
		t.Errorf("must name the missing file and ask about %s, got:\n%s", data, out.String())
	}
}

// TestResolveGrantsKnownSecretsAfterFlood covers an agent that adds over
// 200,000 files in front of an existing .env so the next scan stops
// before reaching it. The .env found by an earlier launch stays hidden.
// Moving it (renaming its folder, or replacing the folder by a symlink)
// reports it as missing instead of following it, and a profile change
// that stops hiding it does not make omac forget it.
func TestResolveGrantsKnownSecretsAfterFlood(t *testing.T) {
	realWD := t.TempDir()
	secrets := mkdirAll(t, realWD, "zzz-secrets")
	env := filepath.Join(secrets, ".env")
	writeFile(t, env)
	// The workdir is reached through a symlink, as with /home -> /var/home.
	workdir := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(realWD, workdir); err != nil {
		t.Fatal(err)
	}
	envHere := filepath.Join(workdir, "zzz-secrets", ".env")
	prof := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}

	before, err := resolveGrants(prof, workdir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	known := before.FoundSecrets
	if !slices.Equal(known, []string{resolvedPath(env)}) {
		t.Fatalf("first launch must remember %s (symlinks resolved), got %v", env, known)
	}

	populateLargeTree(t, realWD) // "g0000".. sorts before "zzz-secrets"

	without, err := resolveGrants(prof, workdir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(without.ProtectedPaths, envHere) {
		t.Fatal("test setup: the flooded scan must not reach the .env on its own")
	}

	with, err := resolveGrants(prof, workdir, nil, known)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(with.IncompleteDenyScanRoots, []string{workdir}) {
		t.Fatalf("IncompleteDenyScanRoots = %v, want [%s]", with.IncompleteDenyScanRoots, workdir)
	}
	// The mask uses this launch's spelling: bwrap compares it literally
	// with the workdir it mounts.
	if !slices.Contains(with.ProtectedPaths, envHere) || !slices.Equal(with.KeptSecrets, known) || len(with.MissingSecrets) > 0 {
		t.Errorf("known secret must stay hidden as %s and stay remembered: protected has it = %v, kept %v, missing %v",
			envHere, slices.Contains(with.ProtectedPaths, envHere), with.KeptSecrets, with.MissingSecrets)
	}

	t.Run("lifted by override_deny, still remembered", func(t *testing.T) {
		lifted := *prof
		lifted.Filesystem.OverrideDeny = []string{envHere}
		g, err := resolveGrants(&lifted, workdir, nil, known)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(g.ProtectedPaths, envHere) || !slices.Equal(g.KeptSecrets, known) {
			t.Errorf("override_deny must lift the mask but keep the entry: kept %v", g.KeptSecrets)
		}
	})
	t.Run("folder already denied, no nested mask", func(t *testing.T) {
		denied := *prof
		denied.Filesystem.Deny = []string{filepath.Join(workdir, "zzz-secrets")}
		g, err := resolveGrants(&denied, workdir, nil, known)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(g.ProtectedPaths, envHere) || !slices.Equal(g.KeptSecrets, known) {
			t.Errorf("a file inside a denied folder must not get its own mask but stay remembered: kept %v", g.KeptSecrets)
		}
	})
	t.Run("parent replaced by a symlink", func(t *testing.T) {
		outside := mkdirAll(t, t.TempDir(), "x")
		writeFile(t, filepath.Join(outside, ".env"))
		if err := os.Rename(secrets, secrets+".moved"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Remove(secrets); os.Rename(secrets+".moved", secrets) })
		if err := os.Symlink(outside, secrets); err != nil {
			t.Fatal(err)
		}
		g, err := resolveGrants(prof, workdir, nil, known)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(g.ProtectedPaths, envHere) || !slices.Equal(g.MissingSecrets, known) {
			t.Errorf("must not follow the symlink, and must report the entry missing: missing %v", g.MissingSecrets)
		}
	})
	t.Run("parent renamed", func(t *testing.T) {
		if err := os.Rename(secrets, secrets+".moved"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Rename(secrets+".moved", secrets) })
		g, err := resolveGrants(prof, workdir, nil, known)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(g.MissingSecrets, known) || len(g.KeptSecrets) > 0 {
			t.Errorf("a moved secret must be reported missing, got missing %v kept %v", g.MissingSecrets, g.KeptSecrets)
		}
	})
}

// A remembered file is only looked at in a folder whose scan stopped
// early; elsewhere the scan's own result is complete.
func TestCheckKnownSecretsOnlyInIncompleteFolders(t *testing.T) {
	root := resolvedPath(t.TempDir())
	env := filepath.Join(root, ".env")
	writeFile(t, env)
	if got := checkKnownSecrets([]string{env}, nil, []string{".env"}, nil, nil); len(got.hide)+len(got.kept)+len(got.missing) > 0 {
		t.Errorf("no incomplete folder, nothing to check: %+v", got)
	}
	if got := checkKnownSecrets([]string{env}, []string{t.TempDir()}, []string{".env"}, nil, nil); len(got.hide)+len(got.kept)+len(got.missing) > 0 {
		t.Errorf("entry outside the incomplete folder must be ignored: %+v", got)
	}
	if got := checkKnownSecrets([]string{env}, []string{root}, []string{".env"}, nil, nil); !slices.Equal(got.hide, []string{env}) {
		t.Errorf("entry inside the incomplete folder must be hidden: %+v", got)
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
