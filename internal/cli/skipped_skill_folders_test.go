package cli

import (
	"bytes"
	"os"
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
	if !strings.Contains(out, `omac start: ignoring skill folder "My_Skill[2J": skill names may only contain lowercase letters`) {
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
