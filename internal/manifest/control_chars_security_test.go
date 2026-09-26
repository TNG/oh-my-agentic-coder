package manifest

import (
	"strings"
	"testing"
)

// TestSecurityManifestRendererStripsControlChars asserts that formatting and
// separator control characters carried in a skill name cannot reach the
// manifest block omac injects into the agent's own system prompt. The
// renderer is the last sink before the text becomes part of the prompt, so
// every control rune — including zero-width, bidi-override and Unicode
// line/paragraph separators that can forge markdown structure or invert text
// direction — must be removed here regardless of what discovery accepted.
func TestSecurityManifestRendererStripsControlChars(t *testing.T) {
	hostile := "ok\u2028ok\u202eok\u200bok\ufeffok"
	activateJSON := `{
  "dir": "/work",
  "skills": [
    {"name": "` + hostile + `", "scope": "workdir", "state": "broken", "detail": "boom"}
  ]
}`
	out := Render(activateJSON, ".opencode/skills")

	// Control: an ordinary skill name renders, so a fix that emptied the
	// manifest entirely would not satisfy this test for the wrong reason.
	benign := Render(`{"dir":"/work","skills":[{"name":"jira","scope":"workdir","state":"broken","detail":"boom"}]}`, ".opencode/skills")
	if !strings.Contains(benign, "**jira**") {
		t.Fatalf("control: an ordinary skill name did not render; fixture is broken, not the security property: %q", benign)
	}

	controlRunes := []rune{
		'\u2028', '\u2029', // line / paragraph separators
		'\u200b', '\u200c', '\u200d', '\ufeff', // zero-width
		'\u202a', '\u202b', '\u202c', '\u202d', '\u202e', // bidi overrides
		'\u2066', '\u2067', '\u2068', '\u2069', // bidi isolates
	}
	for _, r := range controlRunes {
		if strings.ContainsRune(out, r) {
			t.Errorf("manifest output retains a raw control/format rune U+%04X from a skill name: %q", r, out)
		}
	}
}
