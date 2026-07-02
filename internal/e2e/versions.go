//go:build e2e

package e2e

import "os"

// Versions and model configuration for e2e tests.
//
// Bump these when testing a new harness release or model.
// Set E2E_USE_LATEST=1 in CI or locally to skip pinning
// and install the latest version instead (for fast testing).

// Harness versions (last-known-good as of 2026-07-01).
var harnessVersions = map[string]string{
	"opencode":    "opencode-ai@1.17.12",
	"claude-code": "@anthropic-ai/claude-code@2.1.197",
	"codex":       "@openai/codex@0.142.5",
	"copilot":     "@github/copilot@1.0.68",
}

// Model identifiers per harness.
var modelIDs = map[string]string{
	"opencode":    "zai-org/GLM-5.2",
	"claude-code": "claude-sonnet-5",
	"codex":       "zai-org/GLM-5.2",
	"copilot":     "zai-org/GLM-5.2",
}

// pinnedPackage returns the package spec for a harness.
// When E2E_USE_LATEST=1, returns the bare package name (latest).
func pinnedPackage(harness string) string {
	if useLatest() {
		// Strip @version from "pkg@1.2.3" → "pkg".
		pkg := harnessVersions[harness]
		if i := lastIndexByte(pkg, '@'); i > 0 {
			return pkg[:i]
		}
		return pkg
	}
	return harnessVersions[harness]
}

// useLatest returns true when E2E_USE_LATEST=1 is set, which
// skips version pinning and installs the latest harness release.
func useLatest() bool {
	return os.Getenv("E2E_USE_LATEST") != ""
}

// lastIndexByte returns the index of the last occurrence of b in s, or -1.
func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
