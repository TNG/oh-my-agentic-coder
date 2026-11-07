package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/config"
	"github.com/TNG/oh-my-agentic-coder/internal/osinfo"
)

// TestSecurityRegisterRefusesHostileInstallScriptName pins the display
// gate on a skill-provided install_scripts reference. The value is
// authored in the skill's omac.yaml and register presents it to the
// operator as a suggested command, so nothing but a plain relative path
// may be surfaced: every other shape must be replaced by a warn line and
// no callout. The skill itself must keep registering either way — the
// gate is warn-only so existing skills do not break.
func TestSecurityRegisterRefusesHostileInstallScriptName(t *testing.T) {
	host := osinfo.Detect()

	// stage registers a workdir skill that declares script as its
	// install script for the host OS. The script is really shipped under
	// that literal name, so the fixture proves the gate does not rest on
	// the reference failing to resolve on disk.
	stage := func(t *testing.T, skill, script string) string {
		t.Helper()
		workdir := t.TempDir()
		src := filepath.Join(workdir, ".opencode", "skills", skill)
		manifest := "name: " + skill + "\ntype: skill\n" +
			"sidecar:\n  command: [\"python3\", \"sidecar.py\"]\n" +
			"  install_scripts:\n    " + host.String() + ": " + fmt.Sprintf("%q", script) + "\n"
		for rel, body := range map[string]string{
			config.MetaFileName: manifest,
			"sidecar.py":        "print('hi')\n",
			script:              "#!/bin/sh\ntrue\n",
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(src, rel)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(src, rel), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return workdir
	}

	// register drives `omac register` on a piped env and returns its
	// exit code plus the captured stdout and stderr.
	register := func(t *testing.T, workdir, skill string) (code int, out, errOut string) {
		t.Helper()
		env, outBuf, errBuf, drain := newPipeEnv(t, "")
		env.Workdir = workdir
		code = runRegister([]string{skill, "--no-secrets", "--no-fields"}, env)
		drain()
		return code, outBuf.String(), errBuf.String()
	}

	t.Run("hostile reference is not displayed", func(t *testing.T) {
		isolateHome(t)
		const (
			skill  = "setup-hostile"
			script = "$(true)|setup.sh"
		)
		workdir := stage(t, skill, script)
		code, out, errOut := register(t, workdir, skill)

		if code != ExitOK {
			t.Fatalf("register exit %d: refusing to display the reference must not refuse the skill", code)
		}
		// Control: registration ran to its successful end, where the
		// callout decision is made — an early exit must not be able to
		// suppress the callout and make the checks below vacuous.
		if !strings.Contains(out, "[ok] registered") {
			t.Fatalf("control: the fixture did not complete registration, so the output says nothing about the gate\n%q", out)
		}
		all := out + errOut
		if want := "[warn] install script for " + host.String() + " not shown:"; !strings.Contains(all, want) {
			t.Errorf("no %q warn line for the refused reference\nstderr: %q", want, errOut)
		}
		if strings.Contains(all, "NEXT STEP") {
			t.Errorf("register displayed the suggested-command callout for a refused install script reference\n%q", all)
		}
		if strings.Contains(out, script) {
			t.Errorf("the refused reference reached the operator-facing output\n%q", out)
		}
	})

	t.Run("benign reference keeps the callout", func(t *testing.T) {
		isolateHome(t)
		const (
			skill  = "setup-benign"
			script = "setup.sh"
		)
		workdir := stage(t, skill, script)
		code, out, errOut := register(t, workdir, skill)

		if code != ExitOK {
			t.Fatalf("register exit %d", code)
		}
		// A gate that suppresses every reference would pass the hostile
		// subtest for the wrong reason; a plain, harmless name must keep
		// its callout.
		if !strings.Contains(out, script) {
			t.Errorf("a benign install script reference lost its callout\n%q", out+errOut)
		}
	})

	t.Run("validator refuses non-plain references", func(t *testing.T) {
		refs := []string{
			"/opt/vendor/setup.sh",
			"~/setup.sh",
			"../setup.sh",
			"vendor/../../setup.sh",
			"setup\x00.sh",
			"setup\n.sh",
			"setup\r.sh",
			"setup\t.sh",
		}
		for _, ref := range refs {
			if config.InstallScriptRefProblem(ref) == "" {
				t.Errorf("InstallScriptRefProblem(%q) = \"\", want a reason to refuse it", ref)
			}
		}
		// One case per shell-live character embedded in an ordinary
		// name: the gate is a character-level allowlist, not a
		// blocklist of known-bad shapes.
		for _, c := range "$`\"'\\ |;&<>(){}!*?[]#" {
			ref := "pre" + string(c) + "post.sh"
			if config.InstallScriptRefProblem(ref) == "" {
				t.Errorf("InstallScriptRefProblem(%q) = \"\", want a reason to refuse it", ref)
			}
		}
	})

	t.Run("validator accepts plain relative references", func(t *testing.T) {
		for _, ref := range []string{"setup.sh", "scripts/setup.sh", "scripts/deps/python.sh"} {
			if problem := config.InstallScriptRefProblem(ref); problem != "" {
				t.Errorf("InstallScriptRefProblem(%q) = %q, want \"\"", ref, problem)
			}
		}
	})
}
