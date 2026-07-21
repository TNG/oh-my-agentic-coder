// Package sandboxbrief holds the single-source briefing text that omac
// injects into a launched harness so the agent knows it runs inside the
// omac sandbox. The text is embedded so it ships with the binary and is
// edited in exactly one place (brief.md).
package sandboxbrief

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"github.com/tngtech/oh-my-agentic-coder/internal/toolcache"
)

//go:embed brief.md
var defaultBrief string

// Default returns the compiled-in briefing text.
func Default() string {
	return defaultBrief
}

// Resolve returns override when it has non-whitespace content, otherwise
// the embedded Default. This is the precedence rule for the optional
// config.yaml `sandbox.briefing` value: any text wins; empty/unset uses
// the default.
func Resolve(override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return Default()
}

// CacheGuidance renders the cache paragraph appended to every briefing
// (default or custom) so an override can never suppress it. It names
// the actual cache paths/mode selected for this launch, the OMAC_CACHE_*
// selectors, every tool-specific variable omac injects, and explains
// that hardcoded host cache locations are denied by the sandbox.
//
// buildDir is the per-workdir build-cache scope (OMAC_CACHE_DIR); xdgDir is
// the harness's own XDG cache scope (OMAC_XDG_CACHE_DIR), which is shared
// across workdirs. When they are equal (ephemeral mode) the split is invisible.
func CacheGuidance(buildDir, xdgDir string, mode toolcache.Mode) string {
	if xdgDir == "" {
		xdgDir = buildDir
	}
	env := toolcache.EnvironmentSplit(buildDir, xdgDir, mode)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "\n## Tool cache\n\n")
	fmt.Fprintf(&b, "omac redirects every tool's cache into a sandbox-granted directory: `%s` (mode `%s`). ", buildDir, mode)
	fmt.Fprintf(&b, "The selector variables `OMAC_CACHE_DIR` and `OMAC_CACHE_MODE` name the build-cache directory and mode; ")
	fmt.Fprintf(&b, "the harness's own cache (`XDG_CACHE_HOME`) is kept in `OMAC_XDG_CACHE_DIR`, shared across workdirs. ")
	fmt.Fprintf(&b, "Write through the variables below rather than any host path:\n\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "- `%s` → `%s`\n", k, env[k])
	}
	fmt.Fprintf(&b, "\nHardcoded host cache locations (e.g. `~/.cache`, `~/Library/Caches`, `~/.cargo`, `~/.npm`) ")
	fmt.Fprintf(&b, "are denied by the sandbox: write through the variables above instead of touching host paths.\n")
	return b.String()
}
