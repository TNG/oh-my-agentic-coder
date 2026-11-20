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
// operator as a suggested command, so nothing but a plain relative
// path may be surfaced: every other shape must be replaced by a warn
// line and no callout. The skill itself must keep registering either
// way — the gate is warn-only so existing skills do not break.
func TestSecurityRegisterRefusesHostileInstallScriptName(t *testing.T) {
	host := osinfo.Detect()

	// stage registers a workdir skill that declares script as its
	// install script for the host OS. When ship is set, the script is
	// really present under that literal name, proving the gate cannot
	// rest on the reference failing to resolve on disk.
	stage := func(t *testing.T, skill, script string, ship bool) string {
		t.Helper()
		workdir := t.TempDir()
		src := filepath.Join(workdir, ".opencode", "skills", skill)
		files := map[string]string{
			config.MetaFileName: "name: " + skill + "\ntype: skill\n" +
				"sidecar:\n  command: [\"python3\", \"sidecar.py\"]\n" +
				"  install_scripts:\n    " + host.String() + ": " + fmt.Sprintf("%q", script) + "\n",
			"sidecar.py": "print('hi')\n",
		}
		if ship {
			files[script] = "#!/bin/sh\ntrue\n"
		}
		for rel, body := range files {
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

	// refuse asserts the contract for one hostile reference: the skill
	// still registers, the operator gets a warn line instead of the
	// reference, and no NEXT STEP callout appears. The reference must
	// not reach stdout, the surface the operator pastes the suggested
	// command from; a diagnostic warn line on stderr may still name it.
	refuse := func(t *testing.T, skill, script string, ship bool) {
		t.Helper()
		isolateHome(t)
		workdir := stage(t, skill, script, ship)
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
			t.Errorf("no %q warn line for the refused reference %q\nstderr: %q", want, script, errOut)
		}
		if strings.Contains(all, "NEXT STEP") {
			t.Errorf("register displayed the suggested-command callout for the refused reference %q\n%q", script, all)
		}
		if strings.Contains(out, script) {
			t.Errorf("the refused reference %q reached the operator-facing output\n%q", script, out)
		}
	}

	t.Run("hostile reference is not displayed", func(t *testing.T) {
		// The literal-named file really ships, so a gate that only
		// relied on the reference not resolving on disk could not
		// pass this.
		refuse(t, "setup-hostile", "$(true)|setup.sh", true)
	})

	t.Run("benign reference keeps the callout", func(t *testing.T) {
		// A gate that suppresses every reference would pass the
		// hostile cases for the wrong reason; plain names, including
		// ones in subdirectories, must keep their callout.
		for i, script := range []string{"setup.sh", "scripts/setup.sh"} {
			t.Run(fmt.Sprintf("shows %q", script), func(t *testing.T) {
				isolateHome(t)
				skill := fmt.Sprintf("setup-benign-%d", i)
				workdir := stage(t, skill, script, true)
				code, out, errOut := register(t, workdir, skill)

				if code != ExitOK {
					t.Fatalf("register exit %d", code)
				}
				if !strings.Contains(out, script) {
					t.Errorf("a benign install script reference lost its callout\n%q", out+errOut)
				}
			})
		}
	})

	t.Run("non-local reference shapes are refused", func(t *testing.T) {
		// Absolute paths, '~' and any '..' that leaves the skill
		// directory are refused without ever touching the disk, so
		// these files are deliberately not shipped.
		shapes := []string{
			"/opt/vendor/setup.sh",
			"~/setup.sh",
			"../setup.sh",
			"vendor/../../setup.sh",
			"setup\x00.sh",
			"setup\n.sh",
			"setup\r.sh",
			"setup\t.sh",
			"my setup.sh",
		}
		for i, script := range shapes {
			t.Run(fmt.Sprintf("refuses %q", script), func(t *testing.T) {
				refuse(t, fmt.Sprintf("setup-shape-%d", i), script, false)
			})
		}
	})

	t.Run("shell-live characters are refused", func(t *testing.T) {
		// One case per shell-live character embedded in an ordinary
		// name: the gate is a character-level allowlist, not a
		// blocklist of known-bad shapes. Each file really ships under
		// its literal name, so a stat-based defense cannot pass this.
		for i, c := range "$`\"'\\ |;&<>(){}!*?[]#" {
			script := "pre" + string(c) + "post.sh"
			t.Run(fmt.Sprintf("refuses %q", script), func(t *testing.T) {
				refuse(t, fmt.Sprintf("setup-metachar-%d", i), script, true)
			})
		}
	})
}
