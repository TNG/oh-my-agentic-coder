package skillsource

import (
	"os"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// TestMain clears every harness config-home override (CLAUDE_CONFIG_DIR and
// friends) for the whole test binary: withFakeHome fakes $HOME, but an
// ambient override names an absolute path and survives it, so discovery
// would scan the developer's real skills dirs.
func TestMain(m *testing.M) {
	for _, name := range config.HomeEnvNames() {
		os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
