package sandboxprofile

import (
	"strings"
	"testing"
)

func TestEnvironmentSetValidation(t *testing.T) {
	ok := &Profile{Environment: Environment{Set: map[string]string{
		"NPM_CONFIG_USERCONFIG": "~/.config/opencode/tng-npmrc",
		"npm_config_cache":      "/cache",
		"_X1":                   "v",
	}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid set names must pass: %v", err)
	}

	for _, bad := range []string{"", "1LEADING_DIGIT", "WITH-DASH", "WITH.DOT", "WITH SPACE", "lower ok but $BAD"} {
		p := &Profile{Environment: Environment{Set: map[string]string{bad: "v"}}}
		if err := p.Validate(); err == nil {
			t.Errorf("set key %q must be rejected", bad)
		} else if !strings.Contains(err.Error(), "environment.set") {
			t.Errorf("error for %q should name environment.set, got %v", bad, err)
		}
	}
}

func TestExpandEnvValue(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("OMAC_EXPAND_TEST", "value")

	cases := []struct {
		in, want string
	}{
		{"plain", "plain"},
		{"~/sub/dir", "/home/tester/sub/dir"},
		{"~", "/home/tester"},
		{"$OMAC_EXPAND_TEST/x", "value/x"},
		{"${OMAC_EXPAND_TEST}/x", "value/x"},
		{"prefix-$OMAC_EXPAND_TEST", "prefix-value"},
		{"$OMAC_EXPAND_TEST_UNSET", ""},
		{"not-a-path value with spaces", "not-a-path value with spaces"},
	}
	for _, c := range cases {
		got, err := ExpandEnvValue(c.in)
		if err != nil {
			t.Errorf("ExpandEnvValue(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ExpandEnvValue(%q) = %q; want %q", c.in, got, c.want)
		}
	}
}
