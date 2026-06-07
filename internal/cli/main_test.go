package cli

import (
	"os"
	"testing"

	"github.com/zalando/go-keyring"
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
func TestMain(m *testing.M) {
	keyring.MockInit()
	os.Exit(m.Run())
}
