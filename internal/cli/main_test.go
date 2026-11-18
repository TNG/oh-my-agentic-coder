package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
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
// friends): tests fake $HOME, but an ambient override names an absolute
// path and would survive the fake. For the same reason it points
// XDG_CONFIG_HOME and XDG_STATE_HOME at an empty folder, so that no test
// reads the developer's VM project map or VM state.
func TestMain(m *testing.M) {
	keyring.MockInit()
	clearHarnessHomeEnvs()
	xdg, err := os.MkdirTemp("", "omac-cli-test-xdg-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for name, dir := range map[string]string{"XDG_CONFIG_HOME": "config", "XDG_STATE_HOME": "state"} {
		if err := os.Setenv(name, filepath.Join(xdg, dir)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(xdg)
	os.Exit(code)
}

func clearHarnessHomeEnvs() {
	for _, name := range config.HomeEnvNames() {
		os.Unsetenv(name)
	}
}
