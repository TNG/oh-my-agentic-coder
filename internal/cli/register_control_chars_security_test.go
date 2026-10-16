package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/secrets"
	"github.com/TNG/oh-my-agentic-coder/internal/skillconfig"
)

// TestSecurityRegisterOutputStripsControlSequences asserts that a config
// field's own name — which comes straight from the skill's omac.yaml, a
// file the skill author controls — cannot inject terminal control
// sequences into `omac register`'s prompt output.
//
// handleOneField writes spec.Name directly into a fmt.Fprintf with no
// escaping; visibleLen (style.go) only skips escape sequences for width
// alignment, never for output. A field name containing an ANSI CSI
// sequence can repaint or clear what the operator sees while approving a
// skill.
func TestSecurityRegisterOutputStripsControlSequences(t *testing.T) {
	env, _, errBuf, drain := newPipeEnv(t, "value\n")
	store := &skillconfig.Store{}
	payload := "harmless\x1b[2Jname"
	spec := config.ConfigSpec{Name: payload, Type: config.ConfigFieldString}

	if _, err := handleOneField(env, store, "skill", spec, false, nil, nil, false, nil); err != nil {
		t.Fatalf("handleOneField: %v", err)
	}
	drain()
	out := errBuf.String()

	// Control: the field name is actually rendered (this fixture reaches
	// the vulnerable code), so a fix that suppresses all prompt output
	// wouldn't make this test pass for the wrong reason.
	if !strings.Contains(out, "harmless") {
		t.Fatalf("control: the field name was not rendered at all: the fixture is broken, not the security property\n%q", out)
	}

	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("omac register prompt output contains a raw ANSI escape from a skill-supplied config field name: %q", out)
	}
}

// TestSecurityRegisterSecretDescriptionStripsControlSequences is the secret
// counterpart of the test above. A secret's description also comes from the
// skill author's omac.yaml, and it is printed right before the hidden
// prompt. An ANSI sequence in it could repaint the terminal at the exact
// moment the user pastes a credential (for example, hide the real prompt
// behind a fake one).
func TestSecurityRegisterSecretDescriptionStripsControlSequences(t *testing.T) {
	env, _, errBuf, drain := newPipeEnv(t, "")
	promptBuf := fakeSecretInput(t, "value")
	spec := config.SecretSpec{
		Name:        "TOKEN",
		Description: "harmless\x1b[2Jdescription\u009b31m",
	}

	if _, err := handleOneSecret(env, "", "sec-desc-skill", spec, true, nil, nil, false, "linux"); err != nil {
		t.Fatalf("handleOneSecret: %v", err)
	}
	drain()
	out := errBuf.String() + promptBuf.String()

	// Control: the description is actually rendered, so a fix that drops
	// the description line altogether cannot make this test pass.
	if !strings.Contains(out, "harmless") || !strings.Contains(out, "description") {
		t.Fatalf("control: the secret description was not rendered: the fixture is broken, not the security property\n%q", out)
	}
	if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x9b) {
		t.Errorf("omac register secret output contains a raw control character from a skill-supplied description: %q", out)
	}
}

// fakeSecretInput replaces readSecretInput for one test. Each call returns
// the next of values (an empty value once they run out) and appends the
// prompt it was given to the returned buffer, so a test can inspect what
// the user would see next to the hidden input.
func fakeSecretInput(t *testing.T, values ...string) *bytes.Buffer {
	t.Helper()
	prompts := &bytes.Buffer{}
	orig := readSecretInput
	readSecretInput = func(prompt string) (secrets.Secret, error) {
		prompts.WriteString(prompt)
		prompts.WriteString("\n")
		v := ""
		if len(values) > 0 {
			v, values = values[0], values[1:]
		}
		return secrets.NewSecretString(v), nil
	}
	t.Cleanup(func() { readSecretInput = orig })
	return prompts
}
