package sandboxbrief

import (
	"strings"
	"testing"
)

func TestDefaultNonEmpty(t *testing.T) {
	got := Default()
	if strings.TrimSpace(got) == "" {
		t.Fatal("Default() returned empty briefing")
	}
	if !strings.Contains(got, "omac sandbox") {
		t.Errorf("Default() missing expected phrase 'omac sandbox'; got:\n%s", got)
	}
}

func TestResolveUsesOverrideWhenSet(t *testing.T) {
	const custom = "custom briefing text"
	if got := Resolve(custom); got != custom {
		t.Errorf("Resolve(%q) = %q; want the override", custom, got)
	}
}

func TestResolveFallsBackToDefault(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t "} {
		if got := Resolve(in); got != Default() {
			t.Errorf("Resolve(%q) = %q; want Default()", in, got)
		}
	}
}
