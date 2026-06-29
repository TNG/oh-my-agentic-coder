package cli

import (
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxbrief"
)

// briefingInjection decides whether omac should inject the sandbox briefing
// into the inner harness, and returns the resolved briefing text when it
// should.
//
// Injection is active only when a real sandbox wraps the inner command
// (!noSandbox) AND the resolved inner executable is the selected harness's
// own agent binary (inner[0] == harness.InnerCmd[0]). The latter excludes
// profile-pinned/--inner-overridden commands such as the no-sandbox-debug
// `bash` profile, so the briefing flag never lands on the wrong process.
//
// override is the value of config.yaml sandbox.briefing (empty = use the
// embedded default).
func briefingInjection(noSandbox bool, inner []string, harness config.Harness, override string) (string, bool) {
	if noSandbox || len(inner) == 0 || len(harness.InnerCmd) == 0 {
		return "", false
	}
	if inner[0] != harness.InnerCmd[0] {
		return "", false
	}
	return sandboxbrief.Resolve(override), true
}
