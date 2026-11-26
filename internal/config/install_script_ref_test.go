package config

import (
	"strings"
	"testing"
)

// TestInstallScriptRefProblem pins the character-level gate on
// `sidecar.install_scripts` values: register presents them to the
// operator as part of a shell command, so only a plain relative path
// may pass. The gate is warn-only, so an unusable value must never
// make a skill fail to load or validate.
func TestInstallScriptRefProblem(t *testing.T) {
	cases := []struct {
		ref  string
		want string // "" means the value may be shown
	}{
		{"", "no script path"},
		{"install.sh", ""},
		{"scripts/setup.sh", ""},
		{"scripts/../install.sh", ""}, // stays inside after cleaning
		{"/opt/vendor/setup.sh", "not an absolute path"},
		{"~/setup.sh", "'~' is not expanded"},
		{"../setup.sh", "stay inside the skill directory"},
		{"vendor/../../setup.sh", "stay inside the skill directory"},
		{"..", "stay inside the skill directory"},
		{"setup\x00.sh", "control characters"},
		{"setup\n.sh", "control characters"},
		{"setup\r.sh", "control characters"},
		{"setup\t.sh", "control characters"},
		{"my setup.sh", "shell-active characters"},
	}
	for _, tc := range cases {
		got := InstallScriptRefProblem(tc.ref)
		if tc.want == "" {
			if got != "" {
				t.Errorf("InstallScriptRefProblem(%q) = %q, want \"\"", tc.ref, got)
			}
			continue
		}
		if !strings.Contains(got, tc.want) {
			t.Errorf("InstallScriptRefProblem(%q) = %q, want it to contain %q", tc.ref, got, tc.want)
		}
	}

	// Every shell-active character embedded in an otherwise plain name.
	for _, c := range "$`\"'\\ |;&<>(){}!*?[]#" {
		ref := "pre" + string(c) + "post.sh"
		if got := InstallScriptRefProblem(ref); got == "" {
			t.Errorf("InstallScriptRefProblem(%q) = \"\", want a reason", ref)
		}
	}
}

// An unusable install_scripts value must not affect LoadMeta or Validate:
// the gate only decides whether register shows the callout.
func TestLoadMeta_OddInstallScriptStillLoads(t *testing.T) {
	p := writeMeta(t, `name: s
sidecar:
  command: ["true"]
  install_scripts: {linux: "../escape.sh"}
`)
	m, err := LoadMeta(p)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate must not fail over install_scripts: %v", err)
	}
	if InstallScriptRefProblem(m.Sidecar.InstallScripts["linux"]) == "" {
		t.Errorf("the gate must still refuse the value for display")
	}
}
