package profileaudit

import (
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxprofile"
)

// cleanProfile returns a minimal profile with no grants, ready for tests
// to populate specific fields.
func cleanProfile() *sandboxprofile.Profile {
	return &sandboxprofile.Profile{
		Meta:    sandboxprofile.Meta{Name: "test"},
		Workdir: sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
	}
}

func TestCheck_EmptyProfileNoFindings(t *testing.T) {
	findings := Check(cleanProfile())
	if len(findings) != 0 {
		t.Errorf("empty profile should produce no findings; got %d: %+v", len(findings), findings)
	}
}

func TestCheck_OverrideDenyBaselinePathIsHigh(t *testing.T) {
	// ~/.ssh is in the cross-platform protectedCommon set (baseline.go:35).
	p := cleanProfile()
	p.Filesystem.OverrideDeny = []string{"~/.ssh"}
	findings := Check(p)
	if len(findings) == 0 {
		t.Fatal("expected at least one finding for override_deny on ~/.ssh")
	}
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatOverrideDeny {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no override_deny finding; got %+v", findings)
	}
	if got.Severity != SeverityHigh {
		t.Errorf("severity = %q; want %q", got.Severity, SeverityHigh)
	}
	if !strings.Contains(got.Value, ".ssh") {
		t.Errorf("value %q should mention .ssh", got.Value)
	}
	if !strings.Contains(got.Message, "baseline protection") {
		t.Errorf("message %q should mention baseline protection", got.Message)
	}
}

func TestCheck_OverrideDenyNonBaselinePathNoFinding(t *testing.T) {
	// /tmp/foo is not in the baseline; overriding it is a no-op, not a risk.
	p := cleanProfile()
	p.Filesystem.OverrideDeny = []string{"/tmp/no-such-protected-path"}
	findings := Check(p)
	for _, f := range findings {
		if f.Category == CatOverrideDeny {
			t.Errorf("override_deny on non-baseline path should not produce a finding; got %+v", f)
		}
	}
}

func TestCheck_OverrideDenyDollarHomeForm(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := cleanProfile()
	p.Filesystem.OverrideDeny = []string{"$HOME/.ssh"}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatOverrideDeny {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no override_deny finding for $HOME/.ssh; got %+v", findings)
	}
	if got.Severity != SeverityHigh {
		t.Errorf("severity = %q; want high", got.Severity)
	}
}
