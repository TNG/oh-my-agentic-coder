package cli

import (
	"strings"
	"testing"
)

// The --harness help used to claim "Default: opencode" while the code scans
// every harness and refuses an ambiguous name (issue #514).
func TestRegisterHarnessFlagHelpDescribesScan(t *testing.T) {
	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = t.TempDir()
	_ = runRegister([]string{"--help"}, env)
	drain()
	out := outBuf.String()
	if strings.Contains(out, "Default: opencode") {
		t.Errorf("--harness help still claims a default harness:\n%s", out)
	}
	for _, want := range []string{"-harness string", "opencode|claude-code", `"claude" also works`, "every harness is searched", "more than one harness is refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("--harness help missing %q:\n%s", want, out)
		}
	}
}
