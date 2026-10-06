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

// The documented precedence, asserted through the real merge: omac's
// operational injections overwrite a profile's set value, proxy vars win
// over cache env, registry config wins last, and deny_vars still strips a
// set var afterwards.
func TestMergeInjectedEnvPrecedence(t *testing.T) {
	merged := mergeInjectedEnv(
		map[string]string{"A": "profile", "B": "profile", "SHARE": "profile"},
		map[string]string{"B": "cache", "SHARE": "cache"},
		map[string]string{"SHARE": "proxy"},
		map[string]string{"SHARE": "registry"},
	)
	for _, tc := range []struct{ key, want string }{
		{"A", "profile"},
		{"B", "cache"},
		{"SHARE", "registry"},
	} {
		if merged[tc.key] != tc.want {
			t.Errorf("merged[%q] = %q; want %q", tc.key, merged[tc.key], tc.want)
		}
	}
}

// A profile setting a var the omac proxy also injects collides; the merge
// order decides, and FilterEnv must apply deny_vars to the result either way.
func TestProfileSetEnvProxyCollision(t *testing.T) {
	set := profileSetEnv(map[string]string{"HTTPS_PROXY": "http://profile-proxy:1"}, &bytes.Buffer{})
	proxyEnv := map[string]string{"HTTPS_PROXY": "http://omac-proxy:2"}
	merged := mergeInjectedEnv(set, nil, proxyEnv, nil)
	env := sandboxprofile.FilterEnv(nil, nil, nil, merged)

	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "HTTPS_PROXY=http://omac-proxy:2") {
		t.Errorf("the omac proxy env must win over a profile set value, got:\n%s", joined)
	}
}

func TestBlockedEnvValueRef(t *testing.T) {
	cases := map[string]string{
		"$OP_SERVICE_ACCOUNT_TOKEN":   "OP_SERVICE_ACCOUNT_TOKEN",
		"prefix-${LD_PRELOAD}-suffix": "LD_PRELOAD",
		"${NODE_OPTIONS}":             "NODE_OPTIONS",
	}
	for value, want := range cases {
		if got := blockedEnvValueRef(value); got != want {
			t.Errorf("blockedEnvValueRef(%q) = %q; want %q", value, got, want)
		}
	}
	for _, value := range []string{"literal", "$HOME", "${PATH}", "$FAILED_MATCH", "$$escape", "~/.local"} {
		if got := blockedEnvValueRef(value); got != "" {
			t.Errorf("blockedEnvValueRef(%q) = %q; want empty", value, got)
		}
	}
}

// A set value that expands a blocklisted variable under a benign name must be
// dropped: the blocklist is meant to be absolute, and the value would come
// from the supervisor's ambient environment, bypassing allow_vars.
func TestProfileSetEnvDropsBlocklistedValueRefs(t *testing.T) {
	t.Setenv("OP_SERVICE_ACCOUNT_TOKEN", "leaked-by-profile")
	t.Setenv("MY_AUX_ALLOWED", "someone-else")

	var warn bytes.Buffer
	got := profileSetEnv(map[string]string{
		"MY_AUX":        "$OP_SERVICE_ACCOUNT_TOKEN",
		"MY_AGENT_HOME": "~",
	}, &warn)

	if _, ok := got["MY_AUX"]; ok {
		t.Errorf("a set value referencing a blocklisted variable must be dropped, got %v", got)
	}
	if got["MY_AGENT_HOME"] == "" {
		t.Error("a value referencing a non-blocklisted variable must still expand")
	}
	if !strings.Contains(warn.String(), "OP_SERVICE_ACCOUNT_TOKEN") {
		t.Errorf("the drop must be announced with the referenced name, got %q", warn.String())
	}
}
