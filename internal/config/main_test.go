package config

import (
	"os"
	"testing"
)

// TestMain clears every harness config-home override (CLAUDE_CONFIG_DIR and
// friends) for the whole test binary: tests fake $HOME, but an ambient
// override names an absolute path and would win over the fake.
func TestMain(m *testing.M) {
	for _, name := range HomeEnvNames() {
		os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
