package profileaudit

import (
	"runtime"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

// On Linux, a filtered profile with kernel enforcement has its TCP
// mediated by the stage2 Landlock ruleset, and the supervisor proxy's
// port is always granted for connect. The kernel's net-port rule has no
// address dimension, so that implicit grant reaches every host
// listening on the port, not only loopback. This is an accepted risk
// recorded in docs/security.md; the audit's job is to surface it.
//
// The implicit-grant notice under test must fire for the proxy port
// whenever that Landlock path is active, and stay silent on every
// other shape: blocked mode grants no proxy port at all, open mode
// has no kernel mediation, and env-only enforcement is advisory.

// linuxAuditProfile returns a profile shaped like a resolved Linux
// launch, ready for a test to vary the network section. It mirrors
// check_test.go's cleanProfile so unrelated categories stay quiet.
func linuxAuditProfile(net sandboxprofile.Network) *sandboxprofile.Profile {
	return &sandboxprofile.Profile{
		Meta:        sandboxprofile.Meta{Name: "audit-pin"},
		Workdir:     sandboxprofile.Workdir{Access: sandboxprofile.AccessNone},
		Environment: sandboxprofile.Environment{AllowVars: []string{"HOME", "PATH"}},
		Network:     net,
	}
}

// findAddressBlindNotice returns the first finding whose message names
// the address-blind Landlock reach.
func findAddressBlindNotice(findings []Finding) *Finding {
	for i := range findings {
		if strings.Contains(strings.ToLower(findings[i].Message), "address-blind") {
			return &findings[i]
		}
	}
	return nil
}

// TestSecurityProfileAuditFlagsImplicitProxyPort pins the launch-time
// audit: the address-blind notice must cover the proxy port the
// sandbox itself grants, not only user-configured open ports. Without
// it, every default Linux launch ships with an undocumented residual
// reach while profiles that name an open_port get a notice.
func TestSecurityProfileAuditFlagsImplicitProxyPort(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("the Landlock path is Linux-only (GOOS=%s)", runtime.GOOS)
	}

	// Implicit grant only: no user-configured open ports.
	findings := Check(linuxAuditProfile(sandboxprofile.Network{
		Mode:        sandboxprofile.ModeFiltered,
		Enforcement: sandboxprofile.EnforceKernel,
	}))
	notice := findAddressBlindNotice(findings)
	if notice == nil {
		t.Fatalf("filtered kernel-enforced profile: no address-blind notice covering the implicit proxy port grant; got %+v", findings)
	}
	if !strings.Contains(strings.ToLower(notice.Message), "proxy") {
		t.Errorf("notice does not name the implicit proxy port grant: %q", notice.Message)
	}
	if notice.Severity != SeverityLow {
		t.Errorf("severity = %q; want low, so doctor and provenance surface the reach without failing closed", notice.Severity)
	}

	// The user-configured open_port variant keeps its own notice.
	findings = Check(linuxAuditProfile(sandboxprofile.Network{
		Mode:        sandboxprofile.ModeFiltered,
		Enforcement: sandboxprofile.EnforceKernel,
		OpenPort:    []int{3000},
	}))
	var openPortNotice *Finding
	for i := range findings {
		f := &findings[i]
		if f.Category == CatNetwork && f.Field == "network.open_port" && strings.Contains(strings.ToLower(f.Message), "address-blind") {
			openPortNotice = f
			break
		}
	}
	if openPortNotice == nil {
		t.Fatalf("open_port 3000 lost its address-blind notice; got %+v", findings)
	}

	// Shapes where the Landlock path is not active must stay silent.
	for _, tc := range []struct {
		name string
		net  sandboxprofile.Network
	}{
		{"blocked mode grants no proxy port", sandboxprofile.Network{Mode: sandboxprofile.ModeBlocked, Enforcement: sandboxprofile.EnforceKernel}},
		{"open mode has no kernel mediation", sandboxprofile.Network{Mode: sandboxprofile.ModeOpen, Enforcement: sandboxprofile.EnforceKernel}},
		{"env-only enforcement is advisory", sandboxprofile.Network{Mode: sandboxprofile.ModeFiltered, Enforcement: sandboxprofile.EnforceEnvOnly}},
	} {
		if got := findAddressBlindNotice(Check(linuxAuditProfile(tc.net))); got != nil {
			t.Errorf("%s: unexpected address-blind notice: %+v", tc.name, got)
		}
	}
}
