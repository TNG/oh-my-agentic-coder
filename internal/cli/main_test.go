package cli

import (
	"fmt"
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
	if isSandboxRunCapture() {
		captureSandboxRun()
	}
	keyring.MockInit()
	clearHarnessHomeEnvs()
	os.Exit(runWithShortPTYSocketParent(m))
}

func runWithShortPTYSocketParent(m *testing.M) int {
	parent, err := os.MkdirTemp("", "omac-t-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer os.RemoveAll(parent)
	ptySocketParent = parent
	return m.Run()
}

func clearHarnessHomeEnvs() {
	for _, name := range config.HomeEnvNames() {
		os.Unsetenv(name)
	}
}

// isSandboxRunCapture reports whether this test binary was exec'd as the
// native launcher (`{{self}} sandbox run ...`) by a launch test that set
// OMAC_TEST_ARGS and OMAC_TEST_ENV.
func isSandboxRunCapture() bool {
	return len(os.Args) > 2 && os.Args[1] == "sandbox" && os.Args[2] == "run" &&
		os.Getenv("OMAC_TEST_ARGS") != "" && os.Getenv("OMAC_TEST_ENV") != ""
}

// captureSandboxRun records the argv and environment of the native launcher
// the way the capture script does for custom launchers, then exits.
func captureSandboxRun() {
	args := strings.Join(os.Args[1:], "\n") + "\n"
	env := strings.Join(os.Environ(), "\n") + "\n"
	if os.WriteFile(os.Getenv("OMAC_TEST_ARGS"), []byte(args), 0o600) != nil ||
		os.WriteFile(os.Getenv("OMAC_TEST_ENV"), []byte(env), 0o600) != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
