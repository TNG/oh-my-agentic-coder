package vm

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// isolatedGitEnv is the git environment the tests run with: it keeps the
// developer's git config, such as commit.gpgsign, out of the tests that run
// real git.
var isolatedGitEnv = map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"}

// TestMain also drops every other GIT_ variable: a git hook exports
// GIT_DIR, which would point the tests' git at the caller's repository.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "GIT_") {
			if err := os.Unsetenv(name); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
	}
	for name, value := range isolatedGitEnv {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

func TestGitEnvironmentIsIsolated(t *testing.T) {
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if want, ok := isolatedGitEnv[name]; ok && value != want {
			t.Errorf("%s=%q, want %q", name, value, want)
		}
		if _, ok := isolatedGitEnv[name]; !ok && strings.HasPrefix(name, "GIT_") {
			t.Errorf("%s is set while the tests run", name)
		}
	}
}
