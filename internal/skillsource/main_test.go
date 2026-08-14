package skillsource

import (
	"os"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// TestMain clears every harness config-home override (CLAUDE_CONFIG_DIR and
// friends) for the whole package test binary. withFakeHome fakes $HOME, but a
// config-home override names an absolute path and so survived it: with an
// ambient CLAUDE_CONFIG_DIR, Discover in claude scope scanned the developer's
// real skills dir and returned host skills the test never staged.
func TestMain(m *testing.M) {
	for _, name := range config.HomeEnvNames() {
		os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
