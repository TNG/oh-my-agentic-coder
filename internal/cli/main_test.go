package cli

import (
	"os"
	"strings"
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
// path and would survive the fake.
func TestMain(m *testing.M) {
	if path := os.Getenv(recordArgvEnv); path != "" {
		recordArgvAndExit(path)
	}
	keyring.MockInit()
	clearHarnessHomeEnvs()
	os.Exit(m.Run())
}

func clearHarnessHomeEnvs() {
	for _, name := range config.HomeEnvNames() {
		os.Unsetenv(name)
	}
}

// recordArgvEnv names a file. When it is set, this test binary only writes
// its arguments there and exits: a runServe test with the built-in sandbox
// launches {{self}}, which is this binary, and checks the argv it got.
const recordArgvEnv = "OMAC_TEST_RECORD_ARGV"

func recordArgvAndExit(path string) {
	if err := os.WriteFile(path, []byte(strings.Join(os.Args[1:], "\n")), 0o600); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
