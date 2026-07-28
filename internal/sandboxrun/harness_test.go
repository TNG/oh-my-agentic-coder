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
		// Binary "claude" resolves to the canonical harness name "claude-code"
		// (alias -> canonical), the single source of truth the host map keys on.
		{[]string{"~/.bun/bin/claude"}, "claude-code"},
		{[]string{"cc"}, "claude-code"}, // alias
		{[]string{"node"}, ""},          // not a known harness
		{nil, ""},
		{[]string{}, ""},
	}
	for _, c := range cases {
		if got := harnessName(c.argv); got != c.want {
			t.Errorf("harnessName(%v) = %q, want %q", c.argv, got, c.want)
		}
	}
}
