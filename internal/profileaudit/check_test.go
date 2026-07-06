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
	// ~ is also a parent of ~30 baseline secret paths, so the audit must
	// emit one finding per match, not stop at the first.
	p := cleanProfile()
	p.Filesystem.Allow = []string{"~"}
	findings := Check(p)
	foundSSH := false
	count := 0
	for _, f := range findings {
		if f.Category == CatFSGrant && strings.Contains(f.Message, ".ssh") {
			foundSSH = true
		}
		if f.Category == CatFSGrant {
			count++
		}
	}
	if !foundSSH {
		t.Errorf("granting ~ should flag ~/.ssh as exposed; got %+v", findings)
	}
	if count < 5 {
		t.Errorf("granting ~ should produce ≥5 findings (one per baseline secret under home); got %d: %+v", count, findings)
	}
}

func TestCheck_NilProfileNoPanic(t *testing.T) {
	// A nil profile must not panic; Check returns nil/empty.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Check(nil) panicked: %v", r)
		}
	}()
	findings := Check(nil)
	if len(findings) != 0 {
		t.Errorf("Check(nil) should return no findings; got %d: %+v", len(findings), findings)
	}
}

func TestCheck_FSGrantWildcardGlobIsMedium(t *testing.T) {
	// ~/.* contains a "*" and must be treated as a broad glob, emitting
	// MEDIUM findings for each known secret basename glob. Without the
	// wildcard check this entry would slip through checkExplicitGrant and
	// likely produce no finding.
	p := cleanProfile()
	p.Filesystem.Allow = []string{"~/.*"}
	findings := Check(p)
	if len(findings) == 0 {
		t.Fatal("wildcard glob '~/.*' should produce findings for known secret basename globs")
	}
	for _, f := range findings {
		if f.Severity != SeverityMedium {
			t.Errorf("wildcard-glob finding %q severity = %q; want medium", f.Value, f.Severity)
		}
		if f.Category != CatFSGrant {
			t.Errorf("wildcard-glob finding category = %q; want filesystem", f.Category)
		}
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

func TestCheck_NetworkMetadataHostIsHigh(t *testing.T) {
	p := cleanProfile()
	p.Network.AllowDomain = []string{"169.254.169.254"}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatNetwork && findings[i].Field == "network.allow_domain" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no network finding for metadata host; got %+v", findings)
	}
	if got.Severity != SeverityHigh {
		t.Errorf("severity = %q; want high", got.Severity)
	}
	if !strings.Contains(got.Message, "metadata") {
		t.Errorf("message %q should mention metadata", got.Message)
	}
}

func TestCheck_NetworkInternalSuffixIsMedium(t *testing.T) {
	p := cleanProfile()
	p.Network.AllowDomain = []string{"evil.internal"}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatNetwork && findings[i].Field == "network.allow_domain" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no network finding for .internal host; got %+v", findings)
	}
	if got.Severity != SeverityMedium {
		t.Errorf("severity = %q; want medium", got.Severity)
	}
}

func TestCheck_NetworkOpenPortZeroIsLow(t *testing.T) {
	p := cleanProfile()
	p.Network.OpenPort = []int{0}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatNetwork && findings[i].Field == "network.open_port" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no finding for open_port 0; got %+v", findings)
	}
	if got.Severity != SeverityLow {
		t.Errorf("severity = %q; want low", got.Severity)
	}
}

func TestCheck_NetworkAllowTCPConnect22IsMedium(t *testing.T) {
	p := cleanProfile()
	p.Network.AllowTCPConnect = []int{22}
	findings := Check(p)
	var got *Finding
	for i := range findings {
		if findings[i].Category == CatNetwork && findings[i].Field == "network.allow_tcp_connect" {
			got = &findings[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("no finding for allow_tcp_connect 22; got %+v", findings)
	}
	if got.Severity != SeverityMedium {
		t.Errorf("severity = %q; want medium", got.Severity)
	}
}

func TestCheck_NetworkCleanDomainNoFinding(t *testing.T) {
	p := cleanProfile()
	p.Network.AllowDomain = []string{"github.com"}
	findings := Check(p)
	for _, f := range findings {
		if f.Category == CatNetwork {
			t.Errorf("clean domain should not be flagged; got %+v", f)
		}
	}
}

func TestCheck_NetworkTableDriven(t *testing.T) {
	tests := []struct {
		name      string
		allow     []string
		openPorts []int
		tcpPorts  []int
		wantField string
		wantSev   Severity
	}{
		{"metadata.google.internal", []string{"metadata.google.internal"}, nil, nil, "network.allow_domain", SeverityHigh},
		{"metadata.azure.internal", []string{"metadata.azure.internal"}, nil, nil, "network.allow_domain", SeverityHigh},
		{".local suffix", []string{"evil.local"}, nil, nil, "network.allow_domain", SeverityMedium},
		{"tcp 3389", nil, nil, []int{3389}, "network.allow_tcp_connect", SeverityMedium},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := cleanProfile()
			p.Network.AllowDomain = tc.allow
			p.Network.OpenPort = tc.openPorts
			p.Network.AllowTCPConnect = tc.tcpPorts
			findings := Check(p)
			var got *Finding
			for i := range findings {
				if findings[i].Category == CatNetwork && findings[i].Field == tc.wantField {
					got = &findings[i]
					break
				}
			}
			if got == nil {
				t.Fatalf("no %s finding; got %+v", tc.wantField, findings)
			}
			if got.Severity != tc.wantSev {
				t.Errorf("severity = %q; want %q", got.Severity, tc.wantSev)
			}
		})
	}
}
