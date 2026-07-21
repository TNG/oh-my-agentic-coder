package cli

import (
	"github.com/tngtech/oh-my-agentic-coder/internal/config"
	"github.com/tngtech/oh-my-agentic-coder/internal/sandboxbrief"
	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

// briefingInjection reports whether omac should inject the sandbox briefing
// and, if so, the resolved text (override is config.yaml sandbox.briefing;
// empty uses the embedded default).
//
// It injects only when a real sandbox wraps the inner command AND that command
// is the harness's own agent binary. The latter excludes profile-pinned or
// --inner-overridden commands such as the no-sandbox-debug `bash` profile, so
// the briefing never lands on the wrong process.
//
// The cache guidance paragraph is always appended (default or custom
// briefing) because hardcoded host caches are denied by the sandbox —
// an override must not be able to suppress it.
func briefingInjection(noSandbox bool, inner []string, harness config.Harness, override string, cacheScope, xdgScope *toolcache.Scope) (string, bool) {
	if noSandbox || len(inner) == 0 || len(harness.InnerCmd) == 0 {
		return "", false
	}
	if inner[0] != harness.InnerCmd[0] {
		return "", false
	}
	text := sandboxbrief.Resolve(override)
	var dir, xdgDir string
	var mode toolcache.Mode
	if cacheScope != nil {
		dir = cacheScope.Dir
		xdgDir = dir
		mode = cacheScope.Mode
	}
	if xdgScope != nil {
		xdgDir = xdgScope.Dir
	}
	text += sandboxbrief.CacheGuidance(dir, xdgDir, mode)
	return text, true
}
