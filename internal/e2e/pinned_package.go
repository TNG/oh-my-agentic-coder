package e2e

import (
	"os"
	"regexp"
)

// harnessVersions holds the pinned package spec for each supported harness.
var harnessVersions = map[string]string{
	"opencode":    "opencode-ai@1.17.12",
	"claude-code": "@anthropic-ai/claude-code@2.1.197",
	"codex":       "@openai/codex@0.142.5",
	"copilot":     "@github/copilot@1.0.68",
	"pi":          "@earendil-works/pi-coding-agent@0.80.6",
	"codewhale":   "codewhale@0.9.1",
}

// versionEnvVar maps a harness name to the env var that can override its
// pinned package spec for a single run, without editing this file.
var versionEnvVar = map[string]string{
	"opencode":    "E2E_VERSION_OPENCODE",
	"claude-code": "E2E_VERSION_CLAUDE_CODE",
	"codex":       "E2E_VERSION_CODEX",
	"copilot":     "E2E_VERSION_COPILOT",
	"pi":          "E2E_VERSION_PI",
	"codewhale":   "E2E_VERSION_CODEWHALE",
}

// safePackageSpecRE matches the only shape a pinned install spec should take.
const safePackageSpecRE = `^[a-zA-Z0-9_./@-]+@[a-zA-Z0-9_.+-]+$`

// pinnedPackage returns the package spec for the given harness, honouring
// E2E_USE_LATEST and per-harness version override env vars.
func pinnedPackage(harness string) string {
	if useLatest() {
		pkg := harnessVersions[harness]
		if i := lastIndexByte(pkg, '@'); i > 0 {
			return pkg[:i]
		}
		return pkg
	}
	if ev, ok := versionEnvVar[harness]; ok {
		if v := os.Getenv(ev); v != "" {
			if matched, _ := regexp.MatchString(safePackageSpecRE, v); matched {
				return v
			}
		}
	}
	return harnessVersions[harness]
}

func useLatest() bool {
	return os.Getenv("E2E_USE_LATEST") != ""
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
