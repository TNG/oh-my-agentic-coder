package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/skillstate"
)

// TestSecurityStartRefusalStripsControlChars asserts that the consolidated
// launch-refusal report omits raw control bytes carried in any skill-derived
// field. The report is the operator's trusted explanation of why a launch
// was refused, so none of its inputs — skill name, field name, cause text,
// remedy command — may repaint or forge terminal output.
func TestSecurityStartRefusalStripsControlChars(t *testing.T) {
	const payload = "ok\x1bok\x00ok"
	problems := []skillstate.Problem{
		{Kind: skillstate.MetaBroken, Skill: payload, Detail: "d" + payload, Fix: "omac register " + payload},
		{Kind: skillstate.MissingSecret, Skill: payload, Field: "F" + payload, Detail: "d", Fix: "omac secrets set " + payload + " F" + payload},
		{Kind: skillstate.KeychainUnavailable, Skill: payload, Field: "F" + payload, Detail: "d" + payload, Fix: "omac keychain " + payload},
		{Kind: skillstate.MissingField, Skill: payload, Field: "F" + payload, Detail: "d", Fix: "omac register " + payload + " --reprompt-fields"},
	}

	var buf bytes.Buffer
	renderSkillRefusal(&buf, "omac start", problems)
	out := buf.String()

	// Control: the report is actually rendered and the problem content is
	// visible, so a fix that suppressed all output would not pass for the
	// wrong reason.
	if !strings.Contains(out, "refusing to start") {
		t.Fatalf("control: refusal header not rendered; fixture is broken, not the security property: %q", out)
	}
	if !strings.Contains(out, "ok") {
		t.Fatalf("control: problem content not rendered; fixture is broken, not the security property: %q", out)
	}

	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("start refusal report retains a raw escape byte from a skill-derived field: %q", out)
	}
	if strings.ContainsRune(out, 0x00) {
		t.Errorf("start refusal report retains a raw NUL byte from a skill-derived field: %q", out)
	}
}
