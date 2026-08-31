package config

import (
	"os"
	"testing"
)

// TestMain clears every harness config-home override (CLAUDE_CONFIG_DIR and
// friends) for the whole package test binary. Tests here fake $HOME to assert
// the DEFAULT config home, but cannot fake a variable that names an absolute
// path, so an ambient override kept winning over the faked $HOME.
func TestMain(m *testing.M) {
	for _, name := range HomeEnvNames() {
		os.Unsetenv(name)
	}
	os.Exit(m.Run())
}
