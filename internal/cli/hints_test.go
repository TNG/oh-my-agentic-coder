package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/registry"
)

func TestRegisterCmd(t *testing.T) {
	cases := []struct {
		name  string
		skill string
		flags []string
		want  string
	}{
		{name: "no flags", skill: "echo-rest", want: "omac register echo-rest"},
		{name: "one flag follows the skill", skill: "echo-rest", flags: []string{"--force"},
			want: "omac register echo-rest --force"},
		{name: "several flags follow the skill", skill: "echo-rest",
			flags: []string{"--force", "--harness", "opencode"},
			want:  "omac register echo-rest --force --harness opencode"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := registerCmd(c.skill, c.flags...); got != c.want {
				t.Errorf("registerCmd(%q, %v) = %q, want %q", c.skill, c.flags, got, c.want)
			}
		})
	}
}

// TestSkillProblemLine pins the shape issue #227 complained about: the skill
// name and the command must be separated by a label, so the line cannot be
// misread as a single "skill-marketplace - omac register ..." command.
func TestSkillProblemLine(t *testing.T) {
	got := skillProblemLine(styler{}, "skill-marketplace", "re-register",
		registerCmd("skill-marketplace", "--force"))
	want := "    skill-marketplace — re-register: omac register skill-marketplace --force"
	if got != want {
		t.Errorf("skillProblemLine = %q, want %q", got, want)
	}
}

// TestHintsUseDocumentedFlagOrder guards every hint the CLI prints against
// regressing to the flag-before-skill order reported in issue #227. The
// suggested command must match `Usage: omac register <skill> [flags]`, which is
// also the form that reads unambiguously and survives the user appending
// `--harness` after a disambiguation prompt.
func TestHintsUseDocumentedFlagOrder(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	// Any "omac register" immediately followed by a flag is the bad order.
	const bad = "omac register -"
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(".", name))
		if err != nil {
			t.Fatal(err)
		}
		for lineNo, line := range strings.Split(string(src), "\n") {
			// Only user-facing strings matter; skip comments, which discuss
			// the historical form on purpose.
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if strings.Contains(line, bad) {
				t.Errorf("%s:%d suggests flags before the skill name; "+
					"use registerCmd(skill, flags...) instead:\n\t%s",
					name, lineNo+1, strings.TrimSpace(line))
			}
		}
	}
}

// TestRegister_BoolFlagBeforeSkillKeepsLaterFlags pins the parser half of
// issue #227. omac's drift hint used to suggest `omac register --force <skill>`;
// when the skill name was ambiguous across harnesses, omac then asked the user
// to add `--harness`, producing `--force <skill> --harness <h>`. reorderFlagsFirst
// glued the skill name onto the boolean --force, so flag.Parse stopped at the
// positional and never saw --harness: the command died with a usage dump.
func TestRegister_BoolFlagBeforeSkillKeepsLaterFlags(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageHarnessSkillBody(t, wd, "opencode", "slack", "# opencode copy\n")
	stageHarnessSkillBody(t, wd, "claude", "slack", "# claude copy, different bytes\n")
	env := makeEnv(wd)

	if code := runRegister([]string{"slack", "--harness", "opencode"}, env); code != ExitOK {
		t.Fatalf("seed register: code = %d, want ExitOK", code)
	}

	// The exact shape omac used to hand the user.
	if code := runRegister([]string{"--force", "slack", "--harness", "claude"}, env); code != ExitOK {
		t.Fatalf("register --force <skill> --harness <h>: code = %d, want ExitOK", code)
	}

	// --harness must actually have been honored, not silently dropped.
	reg, err := registry.Load(wd)
	if err != nil {
		t.Fatalf("registry.Load: %v", err)
	}
	if cc, _ := reg.FindForHarness("slack", "claude-code"); cc == nil {
		t.Error("claude-code entry missing: --harness was dropped by flag reordering")
	}
}
