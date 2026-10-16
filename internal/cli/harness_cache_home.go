package cli

import (
	"github.com/TNG/oh-my-agentic-coder/internal/config"
)

// injectHarnessCacheHome points a harness's package-extraction cache at its
// sandbox-granted config home. The standalone copilot binary (Node SEA, e.g.
// the Homebrew cask) extracts its bundled package on startup into
// ~/Library/Caches/copilot on macOS — a dir the default profile
// deliberately does not grant (tool-cache isolation, #26) — and honors only
// COPILOT_CACHE_HOME for relocation, ignoring TMPDIR. Redirecting into the
// config home (granted read+write via SandboxDirs) lets the first sandboxed
// launch extract there instead of dying with EPERM (#375). The value is set
// unconditionally — sandboxed or not — so both modes share one cache, and a
// HomeEnv redirect (COPILOT_HOME) is honored because ConfigHome resolves it.
// Mirrors the CLAUDE_CODE_TMPDIR injection in start.go.
func injectHarnessCacheHome(harness config.Harness, extra map[string]string) {
	if harness.CacheHomeEnv == "" {
		return
	}
	if dir := harness.ConfigHome(); dir != "" {
		extra[harness.CacheHomeEnv] = dir
	}
}
