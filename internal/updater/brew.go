package updater

import (
	"runtime"
	"strings"
)

// DetectBrewInstalled reports whether the running omac binary is a
// Homebrew-managed install. Being on PATH isn't sufficient (a user might
// have brew for other tools but installed omac via `go install`), so this
// checks the resolved binary path first (cheap, no subprocess), falling
// back to `brew list --formula oh-my-agentic-coder` succeeding.
func DetectBrewInstalled(executable func() (string, error), lookPath func(string) (string, error), run func(name string, args ...string) error) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	if exe, err := executable(); err == nil {
		if strings.Contains(exe, "/Cellar/") || strings.Contains(exe, "/opt/homebrew/") {
			return true
		}
	}
	if _, err := lookPath("brew"); err != nil {
		return false
	}
	return run("brew", "list", "--formula", "oh-my-agentic-coder") == nil
}
