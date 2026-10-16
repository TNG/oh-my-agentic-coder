package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/osinfo"
	"github.com/TNG/oh-my-agentic-coder/internal/skillconfig"
	"gopkg.in/yaml.v3"
)

const slackLikeManifest = `
name: SLACK_XOXD_TOKEN
description: "Slack browser d-cookie token (xoxd-...)."
hint: |
  In the browser console, paste this snippet and press Enter:

  document.cookie.split('; ').find(c => c.startsWith('d='))?.slice(2);
`

// TestRegisterShowsManifestHint asserts that a manifest `hint:` survives YAML
// decoding and is printed, line breaks intact, above the register prompt.
func TestRegisterShowsManifestHint(t *testing.T) {
	var spec config.ConfigSpec
	if err := yaml.Unmarshal([]byte(slackLikeManifest), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var secret config.SecretSpec
	if err := yaml.Unmarshal([]byte(slackLikeManifest), &secret); err != nil {
		t.Fatalf("unmarshal secret: %v", err)
	}
	if secret.Hint != spec.Hint || secret.Hint == "" {
		t.Fatalf("secret hint not decoded: %q", secret.Hint)
	}

	env, _, errBuf, drain := newPipeEnv(t, "value\n")
	if _, err := handleOneField(env, &skillconfig.Store{}, "skill", spec, false, nil, nil, false, nil); err != nil {
		t.Fatalf("handleOneField: %v", err)
	}
	drain()
	out := errBuf.String()

	for _, want := range []string{
		"Slack browser d-cookie token",
		"\n    In the browser console, paste this snippet and press Enter:\n\n",
		"\n    document.cookie.split('; ').find(c => c.startsWith('d='))?.slice(2);\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("register output missing %q\n%s", want, out)
		}
	}
}

// TestRegisterSecretShowsManifestHint covers the secret prompt path, the
// motivating case (Slack tokens), end to end against the mocked keychain.
func TestRegisterSecretShowsManifestHint(t *testing.T) {
	var spec config.SecretSpec
	if err := yaml.Unmarshal([]byte(slackLikeManifest), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	env, _, errBuf, drain := newPipeEnv(t, "xoxd-value\n")
	origStdin := os.Stdin
	os.Stdin = env.Stdin
	t.Cleanup(func() { os.Stdin = origStdin })

	if _, err := handleOneSecret(env, "", "hint-skill", spec, true, nil, nil, false, osinfo.Linux); err != nil {
		t.Fatalf("handleOneSecret: %v", err)
	}
	drain()
	if want := "\n    document.cookie.split('; ').find(c => c.startsWith('d='))?.slice(2);\n"; !strings.Contains(errBuf.String(), want) {
		t.Errorf("secret prompt missing hint line %q\n%s", want, errBuf.String())
	}
}

// TestPrintFieldHelp pins the rendering, including that a field without a
// hint prints exactly the pre-hint "  NAME description" line.
func TestPrintFieldHelp(t *testing.T) {
	cases := []struct {
		name, desc, hint, want string
	}{
		{"no hint keeps old output", "API token.", "", "  NAME API token.\n"},
		{"nothing to show", "", "", ""},
		{"whitespace-only hint", "", " \t\n", ""},
		{"hint without description", "", "step one\n", "  NAME\n    step one\n"},
		{"multi-line description joined", "first\nsecond", "", "  NAME first second\n"},
		{"blank lines and indent kept", "d", "a\n\n  b\n", "  NAME d\n    a\n\n      b\n"},
		{"CRLF and outer blank lines", "d", "\r\na\r\nb\r\n\r\n", "  NAME d\n    a\n    b\n"},
		{"tabs expanded", "d", "x\ty", "  NAME d\n    x    y\n"},
		{"escapes stripped", "d\x1b[2J", "one\x1b[2J\ntwo", "  NAME d[2J\n    one[2J\n    two\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			printFieldHelp(&b, styler{}, "NAME", tc.desc, tc.hint)
			if got := b.String(); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
