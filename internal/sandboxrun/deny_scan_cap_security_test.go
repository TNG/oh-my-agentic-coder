package sandboxrun

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// TestSecurityDenyScanCapFailsClosed asserts that when the launch-time
// protection scan cannot enumerate a granted tree completely, the sandbox
// refuses to start instead of proceeding with an incomplete protected set.
// A protection scan that cannot finish must not degrade to "unprotected":
// any file the configuration promises is blocked must remain blocked, so
// the only safe response to an incomplete enumeration is to fail closed.
func TestSecurityDenyScanCapFailsClosed(t *testing.T) {
	setDenyByName(t, false) // Linux; macOS hides by name instead (deny_by_name_test.go)
	workdir := t.TempDir()

	// A protected baseline basename file placed at a lexically late path.
	// The protection scan walks granted roots in lexical order and stops
	// after the scan limit, so a target sorted after the padding tree
	// is never visited and would be left unmasked by an implementation
	// that fails open. Placing it late makes the incompleteness concrete:
	// under the broken behavior the file is genuinely absent from the
	// protected set, not merely unconfirmed.
	protectedDir := filepath.Join(workdir, "zzz-protected")
	if err := os.Mkdir(protectedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	protectedFile := filepath.Join(protectedDir, ".env")
	if err := os.WriteFile(protectedFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	populateLargeTree(t, workdir)

	prof := &sandboxprofile.Profile{
		Workdir:    sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Filesystem: sandboxprofile.Filesystem{DenyScan: smallScanLimit()},
		Network:    sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}

	// Control: a small workdir resolves without error, proving any
	// refusal below is caused by the unbounded tree, not by the setup.
	controlProf := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}
	if _, err := ResolveGrants(controlProf, t.TempDir(), nil); err != nil {
		t.Fatalf("control: small workdir should resolve, got %v", err)
	}

	// Fixed state: an incomplete protection scan refuses to launch
	// rather than proceed with a partial protected set. ResolveGrants is
	// the real launch-time entry point, so the refusal must surface here.
	g, err := ResolveGrants(prof, workdir, nil)
	if err == nil {
		// Diagnose: confirm the scan was actually incomplete (the late
		// protected target is absent from the protected set), so the
		// failure message reports the concrete gap rather than a bare
		// "expected refusal".
		for _, p := range g.ProtectedPaths {
			if p == protectedFile {
				t.Fatalf("protection scan completed for %s but launch was not refused; an incomplete scan must fail closed", protectedFile)
			}
		}
		t.Fatalf("launch proceeded after an incomplete protection scan (%s unmasked); expected refusal", protectedFile)
	}
	var scanErr *IncompleteScanError
	if !errors.As(err, &scanErr) {
		t.Fatalf("want *IncompleteScanError, got %T: %v", err, err)
	}
	for _, want := range []string{"stopped after 20,000 entries", "no terminal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}

func flaggedProfile(t *testing.T, args ...string) *sandboxprofile.Profile {
	t.Helper()
	flags, err := sandboxprofile.ParseFlags(append(args, "--block-net", "--", "true"))
	if err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	base := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}
	merged, _ := sandboxprofile.Merge(base, flags)
	return merged
}

// writeEnvPair puts one .env where the scan reads it first (the root) and
// one where it never gets to: the walk takes folders from a stack, so g0000,
// created first and pushed first, is read last.
func writeEnvPair(t *testing.T, workdir string) (first, late string) {
	t.Helper()
	first = filepath.Join(workdir, ".env")
	if err := os.WriteFile(first, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	populateLargeTree(t, workdir)
	lateDir := filepath.Join(workdir, "g0000", "late")
	if err := os.Mkdir(lateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	late = filepath.Join(lateDir, ".env")
	if err := os.WriteFile(late, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	return first, late
}

func TestSecurityDenyScanAcceptedRootStarts(t *testing.T) {
	setDenyByName(t, false)
	workdir := t.TempDir()
	first, late := writeEnvPair(t, workdir)
	prof := flaggedProfile(t, "--deny-scan-limit", "1000", "--deny-scan-accept-incomplete", workdir)

	var notices bytes.Buffer
	g, err := ResolveGrants(prof, workdir, &notices)
	if err != nil {
		t.Fatalf("accepted incomplete root must start: %v", err)
	}
	if !slices.Contains(g.ProtectedPaths, first) {
		t.Errorf("%s found before the limit must stay protected", first)
	}
	if slices.Contains(g.ProtectedPaths, late) {
		t.Errorf("%s is past the limit and cannot be protected", late)
	}
	if !strings.Contains(notices.String(), workdir) || !strings.Contains(notices.String(), "not fully protected") {
		t.Errorf("notices must name the accepted root, got %q", notices.String())
	}
}

func TestSecurityDenyScanGrewDuringStart(t *testing.T) {
	setDenyByName(t, false)
	workdir := t.TempDir()
	writeEnvPair(t, workdir)
	prof := flaggedProfile(t, "--deny-scan-limit", "1000")

	_, err := ResolveGrants(prof, workdir, nil)
	var scanErr *IncompleteScanError
	if !errors.As(err, &scanErr) {
		t.Fatalf("want *IncompleteScanError, got %T: %v", err, err)
	}
	if !scanErr.GrewDuringStart {
		t.Error("a decided scan that is still incomplete must report that the folder grew")
	}
	if !strings.Contains(err.Error(), "grew past 1,000 entries while omac was starting") {
		t.Errorf("unexpected text: %v", err)
	}
}

func TestIncompleteScanErrorText(t *testing.T) {
	noTerminal := &IncompleteScanError{Roots: []string{"/home/x/monorepo"}, Limit: 1000000, ProfilePath: "/p/default.json"}
	wantNoTerminal := `the .env scan of /home/x/monorepo stopped after 1,000,000 entries.
omac cannot ask whether to continue because there is no terminal, so nothing was started.
Fix it in /p/default.json, then start again:
  - raise the limit:            "filesystem": {"deny_scan": {"max_entries": 2000000}}
  - or skip folders that hold no secrets, such as build output:
                                "filesystem": {"deny_scan": {"skip_dir_names": ["build-gradle"]}}
To find the right limit, run ` + "`omac start`" + ` in a terminal once and choose "continue";
omac prints the value it needed when the session ends.`
	if got := noTerminal.Error(); got != wantNoTerminal {
		t.Errorf("no-terminal text:\n%s\nwant:\n%s", got, wantNoTerminal)
	}

	grew := &IncompleteScanError{Roots: []string{"/home/x/monorepo", "/home/x/other"}, Limit: 2000000, ProfilePath: "/p/default.json", GrewDuringStart: true}
	wantGrew := `/home/x/monorepo, /home/x/other grew past 2,000,000 entries while omac was starting
(files were added during the .env scan), so nothing was started.
Start again. If this keeps happening, raise "filesystem.deny_scan.max_entries"
in /p/default.json.`
	if got := grew.Error(); got != wantGrew {
		t.Errorf("grew text:\n%s\nwant:\n%s", got, wantGrew)
	}

	noPath := &IncompleteScanError{Roots: []string{"/r"}, Limit: 5, GrewDuringStart: true}
	if !strings.Contains(noPath.Error(), "in your sandbox profile.") {
		t.Errorf("empty ProfilePath must read \"your sandbox profile\": %v", noPath)
	}
}

// smallScanLimitEntries is the scan cap tests put in the profile, so a
// test needs thousands of files instead of a million.
const smallScanLimitEntries int64 = 20000

func smallScanLimit() *sandboxprofile.DenyScan {
	n := smallScanLimitEntries
	return &sandboxprofile.DenyScan{MaxEntries: &n}
}

// populateLargeTree fills root with more entries than the protection scan
// can enumerate (smallScanLimitEntries), so the scan is guaranteed to
// be incomplete. It is laid out as many small directories whose names
// sort before "zzz-*", keeping the protected target past the enumeration
// bound. Directory reads are kept cheap by spreading entries across many
// dirs.
func populateLargeTree(t *testing.T, root string) {
	t.Helper()
	const dirs = 400
	perDir := int(smallScanLimitEntries)/dirs + 5

	workers := 16
	jobs := make(chan int, workers*2)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for d := range jobs {
				dir := filepath.Join(root, fmt.Sprintf("g%04d", d))
				if err := os.Mkdir(dir, 0o755); err != nil {
					t.Errorf("mkdir %s: %v", dir, err)
					return
				}
				for f := 0; f < perDir; f++ {
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("n%05d", f)), nil, 0o644); err != nil {
						t.Errorf("write %s: %v", dir, err)
						return
					}
				}
			}
		}()
	}
	for d := 0; d < dirs; d++ {
		jobs <- d
	}
	close(jobs)
	wg.Wait()
}
