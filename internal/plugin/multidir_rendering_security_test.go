package plugin

import (
	"os"
	"strings"
	"testing"
)

// stripSpace removes all whitespace so a source-level match is immune to
// reformatting.
func stripSpace(s string) string {
	return strings.Join(strings.Fields(s), "")
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
// removes the characters that would change the shape of the JSON or curl
// sample it is rendered into.
func stripsQuoteAndBacktick(expr string) bool {
	if !strings.Contains(expr, `["`) {
		return false
	}
	return strings.Contains(expr, `\x60`) || strings.Contains(expr, "`")
}

// stripsSingleQuote reports whether a rendered-dir expression also removes
// the single quote. Only the curl sample needs this: its -d argument is a
// single-quoted shell word, so a single quote in the name would end that
// word and let the rest of the name run as shell syntax when the agent
// executes the sample.
func stripsSingleQuote(expr string) bool {
	return strings.Contains(expr, `["'`)
}

// TestSecurityMultidirPluginSanitizesSessionDirectory asserts that the
// embedded OpenCode plugin renders the manifest's session-directory field
// through the same field sanitizer the skill fields go through.
//
// The plugin fetches the manifest and renders it into the system-prompt block
// it injects. The manifest names the session directory, and that name is
// attacker-chosen whenever a user is induced to open a prepared directory —
// the same premise internal/bridge's security tests already accept. The name
// is rendered at three sites in the block, so a line break in it must not
// add lines to omac's instructions, and a quote or backtick in it must leave
// the JSON and curl samples the block hands the agent in their intended
// shape. The curl sample's shell word is single-quoted, so a single quote
// in the name must be stripped there as well.
func TestSecurityMultidirPluginSanitizesSessionDirectory(t *testing.T) {
	src, err := os.ReadFile("assets/omac-multidir.ts")
	if err != nil {
		t.Fatalf("read multidir plugin: %v", err)
	}
	text := string(src)
	compact := stripSpace(text)

	if !strings.Contains(text, "function sanitizeField") {
		t.Error("multidir plugin does not define sanitizeField: the session directory would reach the manifest unescaped")
	}

	// No render site may interpolate the raw field. The needle carries the
	// closing brace so it cannot match the distinct m.dir_token field.
	if strings.Contains(compact, `${m.dir}`) {
		t.Error("multidir plugin interpolates the manifest dir raw: the directory name would shape the agent system prompt the way a hostile skill field no longer can")
	}
	if !strings.Contains(compact, stripSpace("sanitizeField(m.dir")) {
		t.Error("multidir plugin does not pass the manifest dir through sanitizeField: the directory name is as attacker-chosen as a skill field")
	}

	// Each render site, anchored to its surrounding text.
	for _, site := range []struct {
		name            string
		open            string
		close           string
		needStrip       bool
		needSingleQuote bool
	}{
		{"project-directory line", "project directory is: \\`${", "}", false, false},
		{"target-path sample", `"target_path": "${`, "/${skillsDir}", true, false},
		{"curl sample", `{"dir":"${`, "}", true, true},
	} {
		expr := dirRenderExpr(t, compact, stripSpace(site.open), stripSpace(site.close))
		if !strings.Contains(expr, stripSpace("sanitizeField(m.dir")) {
			t.Errorf("multidir plugin renders the manifest dir at the %s without sanitizeField: the name must pass through the field sanitizer like every skill field does", site.name)
		}
		if site.needStrip && !stripsQuoteAndBacktick(expr) {
			t.Errorf("multidir plugin renders the dir into the %s without stripping quote and backtick characters: the sample must keep its shape whatever the name contains", site.name)
		}
		if site.needSingleQuote && !stripsSingleQuote(expr) {
			t.Errorf("multidir plugin renders the dir into the %s without stripping the single quote: the sample is a single-quoted shell word the agent executes, so a quote in the name would break it out of that word", site.name)
		}
	}
}
