package sandboxrun

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// setDenyByName switches name rules on or off for one test, so the
// shared code is covered on Linux and the Linux refusal on macOS.
func setDenyByName(t *testing.T, on bool) {
	t.Helper()
	old := denyByName.Swap(on)
	t.Cleanup(func() { denyByName.Store(old) })
}

// TestSbplNameRegex checks the regular expressions behind the macOS name
// rule with Go's engine, which reads this subset (anchors, groups, ?, *,
// [^/]) the same way.
func TestSbplNameRegex(t *testing.T) {
	res, err := sbplNameRegex("/Users/me/big.data+x", []string{".env", ".env.*", "id_rsa?"})
	if err != nil {
		t.Fatal(err)
	}
	var compiled []*regexp.Regexp
	for _, r := range res {
		compiled = append(compiled, regexp.MustCompile(r))
	}
	matches := func(p string) bool {
		return slices.ContainsFunc(compiled, func(re *regexp.Regexp) bool { return re.MatchString(p) })
	}
	for _, p := range []string{
		"/Users/me/big.data+x/.env",
		"/Users/me/big.data+x/a/b/.env",
		"/Users/me/big.data+x/a/.env/inside", // a folder named .env
		"/Users/me/big.data+x/.env.local",
		"/Users/me/big.data+x/node_modules/pkg/.env",
		"/Users/me/big.data+x/k/id_rsa2",
		"/Users/me/big.data+x/.ENV",
		"/Users/me/big.data+x/a/.Env",
	} {
		if !matches(p) {
			t.Errorf("%s must be hidden", p)
		}
	}
	for _, p := range []string{
		"/Users/me/big.data+x",
		"/Users/me/big.data+x/x.env",
		"/Users/me/big.data+x/.envrc",
		"/Users/me/big.data+x/a/.env.d/../x", // ".env.d" matches .env.*, but "x" is the name; see below
		"/Users/me/bigXdata+x/.env",          // the dot in the folder name is literal
		"/Users/me/big.data+x2/.env",
		"/Users/me/other/.env",
		"/Users/me/big.data+x/id_rsa",
	} {
		if p == "/Users/me/big.data+x/a/.env.d/../x" {
			continue // paths reach Seatbelt resolved; listed to document the case
		}
		if matches(p) {
			t.Errorf("%s must not be hidden", p)
		}
	}
	for _, bad := range [][2]string{{`/a"b`, ".env"}, {`/a\b`, ".env"}, {"/p", "[ab].env"}, {"/p", `\.env`}, {"/p", "a/b"}} {
		if _, err := sbplNameRegex(bad[0], []string{bad[1]}); err == nil {
			t.Errorf("sbplNameRegex(%q, %q) must fail", bad[0], bad[1])
		}
	}
}

// TestResolveGrantsHidesByNameWhenScanIsIncomplete covers a granted
// folder too large to scan on macOS: launch goes ahead and the profile
// hides secret files there by name. An override_deny path inside it would
// be hidden too, so that setup keeps refusing, as does Linux.
func TestResolveGrantsHidesByNameWhenScanIsIncomplete(t *testing.T) {
	setDenyByName(t, true)
	setDenyScanLimit(t, 20000)
	workdir := t.TempDir()
	populateLargeTree(t, workdir)
	prof := &sandboxprofile.Profile{
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite},
		Network: sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked},
	}

	var notices strings.Builder
	g, err := ResolveGrants(prof, workdir, &notices)
	if err != nil {
		t.Fatalf("a folder too large to scan must be hidden by name, not refused: %v", err)
	}
	if !slices.Equal(g.NameDenyRoots, []string{workdir}) || !slices.Contains(g.NameDenyGlobs, ".env") {
		t.Fatalf("NameDenyRoots = %v, NameDenyGlobs = %v", g.NameDenyRoots, g.NameDenyGlobs)
	}
	if !strings.Contains(notices.String(), workdir) {
		t.Errorf("launch must say which folder is hidden by name, got %q", notices.String())
	}
	res, _ := sbplNameRegex(workdir, []string{".env"})
	if profile := GenerateSBPL(g); !strings.Contains(profile, `(deny file-read* (regex #"`+res[0]+`"))`) {
		t.Errorf("profile lacks the name rule for %s", workdir)
	}

	stop := make(chan struct{})
	close(stop)
	if err := WatchNewProtected(prof, workdir, time.Hour, func(string) {}, stop); err != nil {
		t.Errorf("the mid-session watch must start too: %v", err)
	}

	lifted := *prof
	lifted.Filesystem.OverrideDeny = []string{filepath.Join(workdir, "keep", ".env")}
	if _, err := ResolveGrants(&lifted, workdir, nil); err == nil || !strings.Contains(err.Error(), "override_deny") {
		t.Errorf("an override_deny path inside the folder must keep refusing, got %v", err)
	}
}

// A folder the scan finished gets no name rule: its files are listed.
func TestResolveGrantsNoNameRuleForCompleteScan(t *testing.T) {
	setDenyByName(t, true)
	workdir := t.TempDir()
	if err := os.WriteFile(filepath.Join(workdir, ".env"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	g, err := ResolveGrants(&sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}, workdir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.NameDenyRoots) > 0 || strings.Contains(GenerateSBPL(g), "(regex #\"^"+regexQuote(workdir)) {
		t.Errorf("complete scan must not add a name rule: %v", g.NameDenyRoots)
	}
}

// A folder that grows past the cap during the session has no name rule
// in the profile (that is fixed at launch), so the mid-session watch
// must stop with an error instead of quietly checking only part of it.
func TestWatchNewProtectedStopsWhenFolderOutgrowsCap(t *testing.T) {
	setDenyByName(t, true)
	setDenyScanLimit(t, 20000)
	workdir := t.TempDir()
	prof := &sandboxprofile.Profile{Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessReadWrite}}
	stop := make(chan struct{})
	defer close(stop)
	done := make(chan error, 1)
	go func() { done <- WatchNewProtected(prof, workdir, 10*time.Millisecond, func(string) {}, stop) }()

	populateLargeTree(t, workdir)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "during the session") {
			t.Errorf("watch must stop with the cap error, got %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("watch kept running after the folder outgrew the cap")
	}
}
