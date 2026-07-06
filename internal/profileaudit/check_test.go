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

func TestCheck_FSGrantBaselinePathIsHigh(t *testing.T) {
	p := cleanProfile()
	p.Filesystem.Allow = []string{"~/.ssh"}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatFSGrant && findings[i].Field == "filesystem.allow" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no filesystem.allow finding for ~/.ssh; got %+v", findings)
	}
	if got.Severity != SeverityHigh {
		t.Errorf("severity = %q; want high", got.Severity)
	}
	if !strings.Contains(got.Value, ".ssh") {
		t.Errorf("value %q should contain .ssh", got.Value)
	}
}

func TestCheck_FSGrantExtensionPathIsMedium(t *testing.T) {
	p := cleanProfile()
	p.Filesystem.Read = []string{"~/.pypirc"}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatFSGrant && findings[i].Field == "filesystem.read" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no filesystem.read finding for ~/.pypirc; got %+v", findings)
	}
	if got.Severity != SeverityMedium {
		t.Errorf("severity = %q; want medium", got.Severity)
	}
}

func TestCheck_FSGrantParentOfSecretPathIsFlagged(t *testing.T) {
	// Granting ~ (the home dir) is a parent of ~/.ssh → should flag high.
	p := cleanProfile()
	p.Filesystem.Allow = []string{"~"}
	findings := Check(p)
	foundSSH := false
	for _, f := range findings {
		if f.Category == CatFSGrant && strings.Contains(f.Message, ".ssh") {
			foundSSH = true
		}
	}
	if !foundSSH {
		t.Errorf("granting ~ should flag ~/.ssh as exposed; got %+v", findings)
	}
}

func TestCheck_FSGrantSubpathOfSecretPathNotFlagged(t *testing.T) {
	// Granting ~/.ssh/foo does NOT expose ~/.ssh itself.
	p := cleanProfile()
	p.Filesystem.Allow = []string{"~/.ssh/foo"}
	findings := Check(p)
	for _, f := range findings {
		if f.Category == CatFSGrant {
			t.Errorf("subpath of secret path should not be flagged; got %+v", f)
		}
	}
}

func TestCheck_FSGrantBroadGlobIsMedium(t *testing.T) {
	// A broad grant like "." could expose any file; emit medium findings
	// for each known secret basename glob.
	p := cleanProfile()
	p.Filesystem.Allow = []string{"."}
	findings := Check(p)
	if len(findings) == 0 {
		t.Fatal("broad grant '.' should produce findings for known secret globs")
	}
	for _, f := range findings {
		if f.Severity != SeverityMedium {
			t.Errorf("broad-glob finding %q severity = %q; want medium", f.Value, f.Severity)
		}
		if f.Category != CatFSGrant {
			t.Errorf("broad-glob finding category = %q; want filesystem", f.Category)
		}
	}
}

func TestCheck_FSGrantCleanPathNoFinding(t *testing.T) {
	// /usr/local/bin is in the baseline read set, not a secret path.
	p := cleanProfile()
	p.Filesystem.Allow = []string{"/usr/local/bin"}
	findings := Check(p)
	for _, f := range findings {
		if f.Category == CatFSGrant {
			t.Errorf("clean path should not be flagged; got %+v", f)
		}
	}
}
