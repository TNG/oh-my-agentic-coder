package sandboxrun

import "testing"

func TestHarnessName(t *testing.T) {
	cases := []struct {
		argv []string
		want string
	}{
		{[]string{"opencode", "serve"}, "opencode"},
		{[]string{"/usr/bin/opencode.exe", "serve"}, "opencode"},
		{[]string{"OpenCode"}, "opencode"},
		{[]string{"codex"}, "codex"},
		{[]string{"~/.bun/bin/claude"}, "claude"},
		{nil, ""},
		{[]string{}, ""},
	}
	for _, c := range cases {
		if got := harnessName(c.argv); got != c.want {
			t.Errorf("harnessName(%v) = %q, want %q", c.argv, got, c.want)
		}
	}
}
