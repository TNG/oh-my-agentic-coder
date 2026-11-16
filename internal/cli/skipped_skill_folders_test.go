package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/skillsource"
)

// TestStartReportsSkippedSkillFolders checks that omac start names a skill
// folder it ignores for its invalid name, with control bytes stripped, and
// keeps going to the unregistered-skills check.
func TestStartReportsSkippedSkillFolders(t *testing.T) {
	isolateHome(t)
	wd := stageWorkdir(t, "other", "My_Skill\x1b[2J")
	env := makeEnv(wd)
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	env.Stderr = f

	code := runStart([]string{"--inner", "/usr/bin/true"}, env)
	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)

	if code != ExitPrerequisiteMissing || !strings.Contains(out, "other — register with") {
		t.Fatalf("control: start did not stop at the unregistered-skills check: code=%d stderr=%q", code, out)
	}
	if !strings.Contains(out, `omac start: ignoring skill folder "My_Skill[2J": skill names may only contain lowercase letters, digits and "-", and must start with a letter or digit. Rename the folder and the name in its omac.yaml and SKILL.md, then run omac register.`) {
		t.Errorf("skipped folder not reported: %q", out)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("raw escape byte from the folder name reached stderr: %q", out)
	}
}

func TestWarnSkippedSkillFoldersFiltersByKind(t *testing.T) {
	skipped := []skillsource.Skipped{
		{Name: "Work_Dir", Kind: "workdir", Reason: "r"},
		{Name: "Global_One", Kind: "user-global", Reason: "r"},
	}
	var buf bytes.Buffer
	warnSkippedSkillFolders(&buf, "omac serve", skipped, "user-global")
	if got, want := buf.String(), "omac serve: ignoring skill folder \"Global_One\": r\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

// TestServeRediscoverReportsNewSkippedFolderOnce checks that a skill folder
// with an invalid name created while omac serve runs is reported, and that
// rediscovery on later agent turns does not repeat the warning.
func TestServeRediscoverReportsNewSkippedFolderOnce(t *testing.T) {
	s := newServeServerForTest(t)
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	s.env.Stderr = f

	wd := t.TempDir()
	if _, err := s.activate(wd); err != nil {
		t.Fatalf("activate: %v", err)
	}
	skillDir := filepath.Join(wd, ".opencode", "skills", "My_Skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "omac.yaml"), []byte("name: My_Skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := s.dirs[wd]
	s.rediscover(d)
	s.rediscover(d)

	raw, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if n := strings.Count(out, `omac serve: ignoring skill folder "My_Skill"`); n != 1 {
		t.Errorf("warning printed %d times, want 1: %q", n, out)
	}
}
