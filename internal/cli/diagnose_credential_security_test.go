package cli

import (
	"strings"
	"testing"
)

// TestSecurityDiagnoseRedactsProxyCredentials asserts that neither `omac
// diagnose -v` nor `omac diagnose --json` prints the upstream proxy's
// userinfo (credentials) in the clear.
//
// policyFromProfile copies p.Network.UpstreamProxy verbatim into the
// diagnose DTO, and both the verbose text renderer and the JSON renderer
// print it unconditionally — so a proxy URL configured as
// http://user:password@host:port leaks the password to anyone who runs
// `omac diagnose` or reads a bug report pasting its output.
func TestSecurityDiagnoseRedactsProxyCredentials(t *testing.T) {
	isolateHome(t)
	writeProfileFixture(t, `{"network":{"upstream_proxy":"http://corpuser:hunter2@proxy.corp.example:8080"}}`)

	env, out, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	runDiagnose([]string{"-v"}, env)
	drain()
	verbose := out.String()

	// Control: the effective config section is actually shown (the fixture
	// reaches the code under test), so a silent early-exit can't be
	// mistaken for the property holding.
	if !strings.Contains(verbose, "upstream_proxy") {
		t.Fatalf("control: -v output does not mention upstream_proxy at all: the fixture is broken, not the security property\n%s", verbose)
	}
	if strings.Contains(verbose, "hunter2") {
		t.Errorf("omac diagnose -v printed the upstream proxy password in the clear:\n%s", verbose)
	}

	env2, out2, _, drain2 := newPipeEnv(t, "")
	env2.Workdir = t.TempDir()
	runDiagnose([]string{"--json"}, env2)
	drain2()
	jsonOut := out2.String()

	if strings.Contains(jsonOut, "hunter2") {
		t.Errorf("omac diagnose --json printed the upstream proxy password in the clear:\n%s", jsonOut)
	}
}
