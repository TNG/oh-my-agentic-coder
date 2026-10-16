package cli

import (
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
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

// TestPrintFieldHelpStripsControlSequences asserts that an author-controlled
// hint cannot inject terminal escapes while its line breaks are kept.
func TestPrintFieldHelpStripsControlSequences(t *testing.T) {
	var b strings.Builder
	printFieldHelp(&b, styler{}, "NAME", "", "line one\x1b[2J\nline two\n")
	out := b.String()
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("hint output contains a raw ANSI escape: %q", out)
	}
	if !strings.Contains(out, "    line one[2J\n    line two\n") {
		t.Errorf("hint lines not rendered as expected: %q", out)
	}
}
