// Package sandboxbrief holds the single-source briefing text that omac
// injects into a launched harness so the agent knows it runs inside the
// omac sandbox. The text is embedded so it ships with the binary and is
// edited in exactly one place (brief.md).
package sandboxbrief

import (
	_ "embed"
	"strings"
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
