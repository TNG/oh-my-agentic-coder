package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

func runSetupCapture(t *testing.T, workdir string, args []string) (stdout, stderr string, code int) {
	t.Helper()
	dir := t.TempDir()
	outF, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	errF, err := os.Create(filepath.Join(dir, "err"))
	if err != nil {
		t.Fatal(err)
	}
	env := &Env{Version: "test", Workdir: workdir, Stdout: outF, Stderr: errF, Stdin: os.Stdin}
	code = runSetup(args, env)
	outF.Close()
	errF.Close()
	o, _ := os.ReadFile(outF.Name())
	e, _ := os.ReadFile(errF.Name())
	return string(o), string(e), code
}

func TestRunSetupProvisionsAllHarnessesAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	// No harness on PATH → setup provisions to all known harnesses.
	t.Setenv("PATH", "")

	out, _, code := runSetupCapture(t, home, nil)
	if code != ExitOK {
		t.Fatalf("exit = %d, stdout = %s", code, out)
	}
	for _, p := range []string{
		filepath.Join(home, ".config", "opencode", "skills", "omac-write-a-skill", "SKILL.md"),
		filepath.Join(home, ".claude", "skills", "omac-write-a-skill", "SKILL.md"),
	} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected provisioned file %s: %v", p, err)
		}
	}
	if !strings.Contains(out, "installed") {
		t.Fatalf("expected an install line, got: %s", out)
	}

	// Second run is idempotent.
	out2, _, code2 := runSetupCapture(t, home, nil)
	if code2 != ExitOK {
		t.Fatalf("second run exit = %d", code2)
	}
	if !strings.Contains(out2, "already up to date") {
		t.Fatalf("expected idempotent re-run, got: %s", out2)
	}
}

func TestEnsureBuiltinSkillsProvisionsActiveHarness(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	cc, ok := config.LookupHarness("claude")
	if !ok {
		t.Fatal("claude harness not found")
	}

	dir := filepath.Join(t.TempDir(), "out")
	errF, err := os.Create(dir)
	if err != nil {
		t.Fatal(err)
	}
	env := &Env{Version: "test", Workdir: home, Stdout: errF, Stderr: errF, Stdin: os.Stdin}

	// First launch provisions into the active (claude) harness dir only.
	ensureBuiltinSkills(env, cc)
	skill := filepath.Join(home, ".claude", "skills", "omac-write-a-skill", "SKILL.md")
	if _, err := os.Stat(skill); err != nil {
		t.Fatalf("expected auto-provisioned skill at %s: %v", skill, err)
	}
	// It targets only the active harness, not opencode.
	if _, err := os.Stat(filepath.Join(home, ".config", "opencode", "skills", "omac-write-a-skill")); !os.IsNotExist(err) {
		t.Fatalf("ensureBuiltinSkills should not touch a non-active harness dir, err=%v", err)
	}

	// Second launch is a no-op (idempotent) and must not error.
	ensureBuiltinSkills(env, cc)
}

func TestRunSetupUnknownHarness(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, errOut, code := runSetupCapture(t, home, []string{"nope"})
	if code != ExitMisuse {
		t.Fatalf("exit = %d, want ExitMisuse", code)
	}
	if !strings.Contains(errOut, "unknown harness") {
		t.Fatalf("expected unknown-harness error, got: %s", errOut)
	}
}
