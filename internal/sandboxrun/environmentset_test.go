package sandboxrun

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/sandboxprofile"
)

func TestProfileSetEnv(t *testing.T) {
	t.Setenv("OMAC_SET_TEST_VAR", "from-env")
	t.Setenv("HOME", "/home/tester")
	// IsDangerousEnvVar reads the static blocklist; force a name onto it.
	dangerous := "LD_PRELOAD"

	var warn bytes.Buffer
	got := profileSetEnv(map[string]string{
		"PLAIN":      "value",
		"EXPANDED":   "$OMAC_SET_TEST_VAR/suffix",
		"HOME_PATH":  "~/.config/tool",
		"UNSET_VAR":  "$OMAC_SET_TEST_UNSET",
		dangerous:    "/tmp/evil.so",
		"BRACE_FORM": "${OMAC_SET_TEST_VAR}",
	}, &warn)

	want := map[string]string{
		"PLAIN":      "value",
		"EXPANDED":   "from-env/suffix",
		"HOME_PATH":  "/home/tester/.config/tool",
		"UNSET_VAR":  "",
		"BRACE_FORM": "from-env",
	}
	if len(got) != len(want) {
		t.Fatalf("profileSetEnv = %v; want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("profileSetEnv[%q] = %q; want %q", k, got[k], v)
		}
	}
	if _, ok := got[dangerous]; ok {
		t.Errorf("a blocklisted name must never be injected, got %v", got)
	}
	if !strings.Contains(warn.String(), dangerous) {
		t.Errorf("dropping a blocklisted set name must be announced, got %q", warn.String())
	}
}

// The documented precedence: omac's operational injections overwrite a
// profile's set value, and deny_vars still strips a set var afterwards.
func TestProfileSetEnvPrecedenceAndDeny(t *testing.T) {
	var warn bytes.Buffer
	set := profileSetEnv(map[string]string{
		"NPM_CONFIG_CACHE": "/profile/npm",
		"DROPME":           "kept-unless-denied",
	}, &warn)

	// Run merges the supervisor's cache env after the profile's set values.
	injected := map[string]string{}
	for k, v := range set {
		injected[k] = v
	}
	injected["NPM_CONFIG_CACHE"] = "/omac/managed/npm"

	env := sandboxprofile.FilterEnv(
		[]string{"PATH=/usr/bin"},
		sandboxprofile.EffectiveAllowVars(nil),
		[]string{"DROPME"},
		injected,
	)
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "NPM_CONFIG_CACHE=/omac/managed/npm") {
		t.Errorf("omac's injection must win over the profile's set value, got:\n%s", joined)
	}
	if strings.Contains(joined, "/profile/npm") {
		t.Errorf("the shadowed profile value must not appear, got:\n%s", joined)
	}
	if strings.Contains(joined, "DROPME=") {
		t.Errorf("deny_vars must strip a profile set var, got:\n%s", joined)
	}
}
