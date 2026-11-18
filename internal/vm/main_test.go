package vm

import (
	"os"
	"testing"
)

// TestMain keeps the developer's git config, such as commit.gpgsign, out of
// the tests that run real git.
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	os.Exit(m.Run())
}
