package cli

import (
	"bytes"
	"testing"

	"github.com/tngtech/oh-my-agentic-coder/internal/config"
)

func TestCheckInnerBinaryFound(t *testing.T) {
	env, _, _, _ := newPipeEnv(t, "")
	h := testHarnessWithInnerCmd("echo")
	if code := checkInnerBinary(h.ResolveInnerCmd(nil, ""), "test", env); code != ExitOK {
		t.Errorf("checkInnerBinary(echo) = %d, want ExitOK(%d)", code, ExitOK)
	}
}

func TestCheckInnerBinaryMissing(t *testing.T) {
	env, _, errBuf, drain := newPipeEnv(t, "")
	h := testHarnessWithInnerCmd("nonexistent-binary-xyz-12345")
	code := checkInnerBinary(h.ResolveInnerCmd(nil, ""), "test", env)
	drain()
	if code != ExitPrerequisiteMissing {
		t.Errorf("checkInnerBinary(nonexistent) = %d, want ExitPrerequisiteMissing(%d)", code, ExitPrerequisiteMissing)
	}
	if !bytes.Contains(errBuf.Bytes(), []byte("not found on $PATH")) {
		t.Errorf("stderr missing 'not found' message; got:\n%s", errBuf.String())
	}
}

func TestCheckInnerBinaryEmptyCmd(t *testing.T) {
	env, _, _, _ := newPipeEnv(t, "")
	h := testHarnessWithInnerCmd("")
	if code := checkInnerBinary(h.ResolveInnerCmd(nil, ""), "test", env); code != ExitOK {
		t.Errorf("checkInnerBinary(empty) = %d, want ExitOK (skip)", code)
	}
}

// A profile-pinned inner_cmd takes precedence over the harness default
// (Harness.ResolveInnerCmd), so checking the default validated a binary the
// launch would never run — reporting OK for opencode while starting something
// else, which then failed inside the sandbox naming the wrong cause.
func TestCheckInnerBinaryChecksProfileInnerCmdNotTheHarnessDefault(t *testing.T) {
	h := testHarnessWithInnerCmd("echo") // installed; the pinned one is not

	env, _, errBuf, drain := newPipeEnv(t, "")
	code := checkInnerBinary(h.ResolveInnerCmd([]string{"nonexistent-binary-xyz-12345"}, ""), "test", env)
	drain()

	if code != ExitPrerequisiteMissing {
		t.Errorf("profile inner_cmd unchecked: got %d, want ExitPrerequisiteMissing(%d)", code, ExitPrerequisiteMissing)
	}
	if !bytes.Contains(errBuf.Bytes(), []byte("nonexistent-binary-xyz-12345")) {
		t.Errorf("stderr does not name the pinned binary; got:\n%s", errBuf.String())
	}
}

func testHarnessWithInnerCmd(bin string) config.Harness {
	cmd := []string{}
	if bin != "" {
		cmd = []string{bin}
	}
	return config.Harness{Name: "test", InnerCmd: cmd}
}

func TestDoctorHarnessBinarySection(t *testing.T) {
	dir := t.TempDir()
	env, outBuf, _, drain := newPipeEnv(t, "")
	env.Workdir = dir

	_ = runDoctor([]string{}, env)
	drain()
	output := outBuf.String()
	if !bytes.Contains([]byte(output), []byte("Inner harnesses")) {
		t.Errorf("doctor output missing 'Inner harnesses' section; got:\n%s", output)
	}
	for _, name := range []string{"opencode", "claude", "codex", "copilot"} {
		if !bytes.Contains([]byte(output), []byte(name)) {
			t.Errorf("doctor output missing harness %q; got:\n%s", name, output)
		}
	}
}
