package session

import (
	"os"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// TestMain clears every harness config-home override (CLAUDE_CONFIG_DIR and
// friends) for the whole test binary: session stores resolve from
// Harness.ConfigHome(), so an ambient override would point tests at the
// developer's real session history instead of their staged temp dirs.
func TestMain(m *testing.M) {
	for _, name := range config.HomeEnvNames() {
		os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
