//go:build vuln

package bridge

import (
	"os"
	"strings"
	"testing"
)

// TestSecurityPiBridgeRenderingSanitizesSkillFields asserts that the Pi bridge
// extension sanitizes skill-supplied fields (name, scope, detail, missing) before
// interpolating them into the manifest injected into the agent's system prompt.
//
// A skill directory whose name contains newlines or markdown bold markers can
// forge instruction blocks in the system prompt. A skill name containing shell
// metacharacters (;, &) in the secretsHint output can mislead the operator.
//
// The guard is source-level (no TypeScript test runner in this repo): verify
// that sanitizeField is defined and applied to each interpolation site, and that
// secretsHint validates the name before emitting a shell command.
func TestSecurityPiBridgeRenderingSanitizesSkillFields(t *testing.T) {
	src, err := os.ReadFile(piBridgePath)
	if err != nil {
		t.Fatalf("read pi bridge: %v", err)
	}
	text := string(src)
	compact := stripSpace(text)

	// sanitizeField must be defined and used at the render sites.
	if !strings.Contains(text, "function sanitizeField") {
		t.Error("pi bridge does not define sanitizeField: skill-supplied fields reach the manifest unescaped")
	}

	// Each interpolation site must pass the field through sanitizeField.
	for _, site := range []string{
		"sanitizeField(sk.name)",
		"sanitizeField(sk.scope",
		"sanitizeField(sk.detail",
		// missing array elements must be sanitized (via .map(sanitizeField))
		".map(sanitizeField)",
	} {
		if !strings.Contains(compact, stripSpace(site)) {
			t.Errorf("pi bridge does not apply sanitizeField at site %q: a hostile skill manifest field can inject content into the agent system prompt", site)
		}
	}

	// secretsHint must guard against skill names with shell metacharacters.
	if !strings.Contains(text, "function secretsHint") {
		t.Error("pi bridge does not define secretsHint: omac secrets set commands may be built from unvalidated skill names")
	}
	if !strings.Contains(compact, stripSpace(`/^[a-z0-9][a-z0-9-]*$/`)) {
		t.Error("pi bridge secretsHint does not validate the skill name: a name with ; or & would produce an injectable shell command fragment")
	}
}
