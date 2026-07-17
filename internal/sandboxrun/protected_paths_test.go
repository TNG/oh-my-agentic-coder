package sandboxrun

import (
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestProtectedPathSetBaselineMatch(t *testing.T) {
	prof := &sandboxprofile.Profile{}
	set := NewProtectedPathSet(prof)
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

func TestProtectedPathSetSubpathMatch(t *testing.T) {
	prof := &sandboxprofile.Profile{}
	set := NewProtectedPathSet(prof)
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
	set := NewProtectedPathSet(prof)
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
	set := NewProtectedPathSet(prof)
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
}
