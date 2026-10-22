package sandboxrun

import (
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxdeny"
	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestProtectedPathSetBaselineMatch(t *testing.T) {
	prof := &sandboxprofile.Profile{}
	set := NewProtectedPathSet(prof, "")
	if set == nil {
		t.Fatal("nil set")
	}
	// Find a baseline path that expands to an absolute path.
	var sample string
	for _, e := range set.entries {
		if len(e) > 0 && e[0] == '/' {
			sample = e
			break
		}
	}
	if sample == "" {
		t.Skip("no expanded baseline path found")
	}
	rule, ok := set.IsProtected(sample)
	if !ok {
		t.Errorf("IsProtected(%q) = false; want true", sample)
	}
	if rule != "baseline" {
		t.Errorf("rule = %q; want baseline", rule)
	}
}

// The facade must report the .omac config dir even when the profile tries to
// override_deny it, mirroring the non-overridable kernel set.
func TestProtectedPathSetOmacDirSurvivesOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workdir := t.TempDir()
	prof := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{OverrideDeny: []string{".omac", "~/.config/omac"}},
	}
	set := NewProtectedPathSet(prof, workdir)
	for _, p := range []string{workdir + "/.omac", workdir + "/.omac/config.yaml"} {
		if rule, ok := set.IsProtected(p); !ok || rule != "omac" {
			t.Errorf("IsProtected(%q) = (%q, %v); want omac-protected", p, rule, ok)
		}
	}
	if _, ok := set.IsProtected(workdir + "/.omac"); !ok {
		t.Error(".omac must be reported protected despite override_deny")
	}
}

func TestUnrestrictedProtectedPathSetKeepsOmac(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workdir := t.TempDir()
	set := UnrestrictedProtectedPathSet(workdir)
	if rule, ok := set.IsProtected(workdir + "/.omac/default.json"); !ok || rule != "omac" {
		t.Errorf("learn-mode set must keep .omac protected, got (%q, %v)", rule, ok)
	}
}

func TestProtectedPathSetSubpathMatch(t *testing.T) {
	prof := &sandboxprofile.Profile{}
	set := NewProtectedPathSet(prof, "")
	var sample string
	for _, e := range set.entries {
		if len(e) > 0 && e[0] == '/' {
			sample = e
			break
		}
	}
	if sample == "" {
		t.Skip("no expanded baseline path found")
	}
	sub := sample + "/credentials"
	rule, ok := set.IsProtected(sub)
	if !ok {
		t.Errorf("IsProtected(%q) = false; want true (subpath of %q)", sub, sample)
	}
	if rule != "baseline" {
		t.Errorf("rule = %q; want baseline", rule)
	}
}

func TestProtectedPathSetNoMatch(t *testing.T) {
	prof := &sandboxprofile.Profile{}
	set := NewProtectedPathSet(prof, "")
	_, ok := set.IsProtected("/tmp/random-file")
	if ok {
		t.Error("IsProtected(/tmp/random-file) = true; want false")
	}
}

func TestProtectedPathSetProfileDeny(t *testing.T) {
	prof := &sandboxprofile.Profile{
		Filesystem: sandboxprofile.Filesystem{
			Deny: []string{"~/secrets.json"},
		},
	}
	set := NewProtectedPathSet(prof, "")
	var found bool
	for _, e := range set.entries {
		rule, ok := set.IsProtected(e)
		if ok && rule == "profile" {
			found = true
			break
		}
	}
	if !found {
		t.Error("no profile-rule entry found for ~/secrets.json")
	}
}

func TestProtectedPathSetNilSafe(t *testing.T) {
	var set *ProtectedPathSet
	_, ok := set.IsProtected("/anything")
	if ok {
		t.Error("nil set should return false")
	}
	set.Add("/x", sandboxdeny.RuleMidSession) // must not panic
}

func TestProtectedPathSetAddMidSession(t *testing.T) {
	set := NewProtectedPathSet(&sandboxprofile.Profile{}, "")
	if _, ok := set.IsProtected("/workdir/.env"); ok {
		t.Fatal("IsProtected before Add = true; want false")
	}
	set.Add("/workdir/.env", sandboxdeny.RuleMidSession)
	rule, ok := set.IsProtected("/workdir/.env")
	if !ok {
		t.Fatal("IsProtected after Add = false; want true")
	}
	if rule != sandboxdeny.RuleMidSession {
		t.Errorf("rule = %q; want %q", rule, sandboxdeny.RuleMidSession)
	}
	// Subpaths of a dynamically added directory are covered too,
	// matching the static-entry semantics.
	rule, ok = set.IsProtected("/workdir/.env/nested")
	if !ok || rule != sandboxdeny.RuleMidSession {
		t.Errorf("IsProtected(subpath) = %q,%v; want %q,true", rule, ok, sandboxdeny.RuleMidSession)
	}
}
