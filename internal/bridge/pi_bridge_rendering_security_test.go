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

// dirRenderExpr returns the interpolation expression the compacted source
// places between two literal markers of a rendered-dir site. Anchoring to
// the surrounding text keeps the guard on the actual render site instead of
// anywhere in the file.
func dirRenderExpr(t *testing.T, compact, open, close string) string {
	t.Helper()
	i := strings.Index(compact, open)
	if i < 0 {
		t.Fatalf("rendered-dir site starting %q not found: the guard is anchored to that site", open)
	}
	rest := compact[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		t.Fatalf("rendered-dir site starting %q has no closing %q", open, close)
	}
	return rest[:j]
}

// stripsQuoteAndBacktick reports whether a rendered-dir expression also
// removes the characters that would change the shape of the JSON sample it
// is rendered into.
func stripsQuoteAndBacktick(expr string) bool {
	if !strings.Contains(expr, `["`) {
		return false
	}
	return strings.Contains(expr, `\x60`) || strings.Contains(expr, "`")
}

// TestSecurityPiBridgeSanitizesSessionDirectory asserts that the manifest's
// session-directory field gets the same treatment as the skill fields before
// it reaches the agent's system prompt.
//
// The directory name is not omac's choice: it is the directory the session
// was started in, attacker-chosen whenever a user is induced to open a
// prepared directory. The name is rendered at two sites in the injected
// block, so a line break in it must not add lines to omac's instructions
// and a quote in it must leave the JSON sample the block hands the agent in
// its intended shape.
func TestSecurityPiBridgeSanitizesSessionDirectory(t *testing.T) {
	src, err := os.ReadFile(piBridgePath)
	if err != nil {
		t.Fatalf("read pi bridge: %v", err)
	}
	text := string(src)
	compact := stripSpace(text)

	if !strings.Contains(text, "function sanitizeField") {
		t.Error("pi bridge does not define sanitizeField: the session directory would reach the manifest unescaped")
	}

	// No render site may interpolate the raw field.
	if strings.Contains(compact, `${manifest.dir`) {
		t.Error("pi bridge interpolates the manifest dir raw: the directory name would shape the agent system prompt the way a hostile skill field no longer can")
	}
	if !strings.Contains(compact, stripSpace("sanitizeField(manifest.dir")) {
		t.Error("pi bridge does not pass the manifest dir through sanitizeField: the directory name is as attacker-chosen as a skill field")
	}

	// Each render site, anchored to its surrounding text.
	for _, site := range []struct {
		name      string
		open      string
		close     string
		needStrip bool
	}{
		{"project-directory line", "project directory is: \\`${", "}", false},
		{"target-path sample", `"target_path": "${`, "/${skillsDir}", true},
	} {
		expr := dirRenderExpr(t, compact, stripSpace(site.open), stripSpace(site.close))
		if !strings.Contains(expr, stripSpace("sanitizeField(manifest.dir")) {
			t.Errorf("pi bridge renders the manifest dir at the %s without sanitizeField: the name must pass through the field sanitizer like every skill field does", site.name)
		}
		if site.needStrip && !stripsQuoteAndBacktick(expr) {
			t.Errorf("pi bridge renders the dir into the %s without stripping quote and backtick characters: the sample must keep its shape whatever the name contains", site.name)
		}
	}
}
