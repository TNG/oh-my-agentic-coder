package cli

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

// TestMain installs go-keyring's in-memory mock provider for the whole
// package test binary. Without it, tests that resolve skill secrets depend
// on the host's OS keyring backend: on a headless CI runner (no Secret
// Service / D-Bus) keyring.Get returns an I/O error rather than
// keyring.ErrNotFound, which makes serve-mode activation classify a skill
// with a missing required secret as "broken" instead of
// "pending-credentials" (see TestActivatePendingCredentials).
//
// The mock returns ErrNotFound for absent keys and stores Set values in
// memory, which is exactly the deterministic behavior these tests assume.
//
// It also clears every harness config-home override (CLAUDE_CONFIG_DIR and
// friends). Tests here fake $HOME but cannot fake a variable that names an
// absolute path, so an ambient override kept pointing at the developer's real
// config home.
func TestMain(m *testing.M) {
	keyring.MockInit()
	clearHarnessHomeEnvs()
	os.Exit(m.Run())
}

// clearHarnessHomeEnvs unsets every harness HomeEnv for the whole test binary.
func clearHarnessHomeEnvs() {
	for _, name := range config.HomeEnvNames() {
		os.Unsetenv(name)
	}
}
