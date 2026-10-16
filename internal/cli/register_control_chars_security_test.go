package cli

import (
	"bytes"
	"os"
	"path/filepath"
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

	if _, err := handleOneField(env, store, "skill", "", spec, false, nil, nil, false, nil); err != nil {
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

	if _, err := handleOneSecret(env, "", "sec-desc-skill", "", spec, true, nil, nil, false, "linux"); err != nil {
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

// TestSecurityRegisterHintAndDocsStripControlSequences covers the `hint`
// and `docs` fields of secrets and config fields. Both come from the skill
// author's omac.yaml and are printed next to the prompt, like the
// description. The docs file name and its section anchor both carry a
// payload; the file really exists so the "More:" line is printed.
func TestSecurityRegisterHintAndDocsStripControlSequences(t *testing.T) {
	const esc = "\x1b[2J"
	const c1 = "\u009b31m"
	dir := t.TempDir()
	docFile := "doc" + esc + "file.md"
	if err := os.WriteFile(filepath.Join(dir, docFile), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	hint := "hintline" + esc + "one\nsecond" + c1 + "line\n"
	docs := docFile + "#sec" + esc + "tion" + c1

	check := func(t *testing.T, out string) {
		t.Helper()
		// Control: hint, docs path and section all reach the output, so a
		// fix that drops the help block cannot pass for the wrong reason.
		for _, frag := range []string{"hintline", "second", "More:", "file.md", `(section "sec`} {
			if !strings.Contains(out, frag) {
				t.Fatalf("control: %q not rendered: the fixture is broken, not the security property\n%q", frag, out)
			}
		}
		if strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x9b) {
			t.Errorf("register output contains a raw control character from a skill-supplied hint or docs: %q", out)
		}
	}

	t.Run("secret", func(t *testing.T) {
		env, _, errBuf, drain := newPipeEnv(t, "")
		// The first value fails the pattern, so the hint is printed a second
		// time on the retry path, which must be sanitized as well.
		promptBuf := fakeSecretInput(t, "bad", "ok-value")
		spec := config.SecretSpec{Name: "TOKEN", Pattern: "^ok-", Hint: hint, Docs: docs}
		if _, err := handleOneSecret(env, "", "sec-hint-skill", dir, spec, true, nil, nil, false, "linux"); err != nil {
			t.Fatalf("handleOneSecret: %v", err)
		}
		drain()
		out := errBuf.String() + promptBuf.String()
		if strings.Count(out, "hintline") != 2 {
			t.Fatalf("control: the hint was not repeated on the retry path\n%q", out)
		}
		check(t, out)
	})

	t.Run("config field", func(t *testing.T) {
		env, _, errBuf, drain := newPipeEnv(t, "BAD\nok\n")
		spec := config.ConfigSpec{Name: "FIELD", Pattern: "^[a-z]+$", Hint: hint, Docs: docs}
		if _, err := handleOneField(env, &skillconfig.Store{}, "sec-hint-field", dir, spec, true, nil, nil, false, nil); err != nil {
			t.Fatalf("handleOneField: %v", err)
		}
		drain()
		out := errBuf.String()
		if strings.Count(out, "hintline") != 2 {
			t.Fatalf("control: the hint was not repeated on the retry path\n%q", out)
		}
		check(t, out)
	})
}

// TestSecurityRegisterDefaultsStripControlSequences covers the default
// values shown on the prompts. A config field's `default:` is printed in
// the prompt (" [<default>, from spec.default]") and again in the "set"
// line after Enter; a `default_from_env:` variable name is printed in the
// config prompt and in the secret prompt note. All of them come from
// omac.yaml.
func TestSecurityRegisterDefaultsStripControlSequences(t *testing.T) {
	const payload = "\x1b[2J"
	const c1 = "\u009b31m"
	hasControl := func(out string) bool {
		return strings.ContainsRune(out, 0x1b) || strings.ContainsRune(out, 0x9b)
	}

	t.Run("config default", func(t *testing.T) {
		env, _, errBuf, drain := newPipeEnv(t, "\n")
		spec := config.ConfigSpec{Name: "FIELD", Default: "def" + payload + "ault" + c1}
		if _, err := handleOneField(env, &skillconfig.Store{}, "sec-default", "", spec, true, nil, nil, false, nil); err != nil {
			t.Fatalf("handleOneField: %v", err)
		}
		drain()
		out := errBuf.String()
		// Control: the default reaches both the prompt and the set line.
		if strings.Count(out, "def[2Jault") != 2 || !strings.Contains(out, "from spec.default") {
			t.Fatalf("control: default not rendered in prompt and set line: the fixture is broken\n%q", out)
		}
		if hasControl(out) {
			t.Errorf("config prompt output contains a raw control character from a skill-supplied default: %q", out)
		}
	})

	t.Run("config default_from_env", func(t *testing.T) {
		envName := "OMAC_TEST_DEF" + payload + c1
		t.Setenv(envName, "fromenv")
		env, _, errBuf, drain := newPipeEnv(t, "\n")
		spec := config.ConfigSpec{Name: "FIELD", DefaultFromEnv: envName}
		if _, err := handleOneField(env, &skillconfig.Store{}, "sec-default-env", "", spec, true, nil, nil, false, nil); err != nil {
			t.Fatalf("handleOneField: %v", err)
		}
		drain()
		out := errBuf.String()
		if strings.Count(out, "$OMAC_TEST_DEF") != 2 || !strings.Contains(out, "fromenv") {
			t.Fatalf("control: env default source not rendered in prompt and set line: the fixture is broken\n%q", out)
		}
		if hasControl(out) {
			t.Errorf("config prompt output contains a raw control character from a default_from_env name: %q", out)
		}
	})

	t.Run("secret default_from_env", func(t *testing.T) {
		envName := "OMAC_TEST_SECDEF" + payload + c1
		t.Setenv(envName, "fromenv")
		env, _, errBuf, drain := newPipeEnv(t, "")
		prompts := fakeSecretInput(t, "") // Enter: accept the env value
		spec := config.SecretSpec{Name: "TOKEN", DefaultFromEnv: envName}
		if _, err := handleOneSecret(env, "", "sec-secret-default", "", spec, true, nil, nil, false, "linux"); err != nil {
			t.Fatalf("handleOneSecret: %v", err)
		}
		drain()
		out := errBuf.String() + prompts.String()
		// Control: the name reaches the prompt note and the "stored" line.
		if !strings.Contains(prompts.String(), "press Enter to accept value from $OMAC_TEST_SECDEF") ||
			!strings.Contains(errBuf.String(), "(from $OMAC_TEST_SECDEF") {
			t.Fatalf("control: default_from_env name not rendered in prompt and stored line: the fixture is broken\n%q", out)
		}
		if hasControl(out) {
			t.Errorf("secret prompt output contains a raw control character from a default_from_env name: %q", out)
		}
	})
}
