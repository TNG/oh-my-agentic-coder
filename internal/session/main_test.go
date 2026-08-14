package session

import (
	"os"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// TestMain clears every harness config-home override (CLAUDE_CONFIG_DIR and
// friends) for the whole package test binary. Session listing resolves its
// store from Harness.ConfigHome(), so an ambient override would point a test at
// the developer's real session history instead of the temp dir it staged. CI
// has none of these set, so such failures only appear locally. Tests that want
// a redirect set it themselves with t.Setenv, which still overrides and
// restores normally after this.
func TestMain(m *testing.M) {
	for _, name := range config.HomeEnvNames() {
		os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
