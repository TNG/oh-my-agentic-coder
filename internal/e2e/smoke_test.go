//go:build e2e

// Smoke tier: model-free harness-version compatibility checks.
//
// These sit between the model-free/harness-free PR gate (e2e_fast) and the
// full agent-driven suite (TestE2EEchoRest / TestE2ESecurityAudit). They
// install the REAL harness binary — pinned or, under E2E_USE_LATEST, the
// latest release — and verify the assumptions omac makes about it WITHOUT a
// model round-trip:
//
//   - TestHarnessCLIContract asserts every CLI flag/subcommand omac derives
//     from its registry still exists in the harness's own --help, so a release
//     that renames or moves a flag fails fast with a precise signal instead of
//     surfacing a week later as an inscrutable agent failure.
//   - TestHarnessLaunchProbe drives `omac start <harness> -- --version` through
//     the real sandbox, exercising omac's launch/PATH/config-home/sandbox-
//     admission wiring end-to-end (everything except the model call).
//
// Both are selectable without secrets (no SKAINET/Anthropic token needed):
//
//	go test -tags=e2e -run 'TestHarnessCLIContract|TestHarnessLaunchProbe' ./internal/e2e/
//
// so a version-bump PR can gate on them cheaply. The nightly drift workflow
// runs them with E2E_USE_LATEST=1 across every harness and OS.
//
// Each test prints one machine-readable OMAC_COMPAT line per harness/stage
// (see compatLine); the workflow parses those into the persisted compatibility
// matrix.

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// smokeCmdTimeout bounds a single harness --version/--help invocation.
const smokeCmdTimeout = 60 * time.Second

// launchProbeTimeout bounds the `omac start ... -- --version` launch probe.
// Generous vs. a bare --version because it spins up the sandbox first.
const launchProbeTimeout = 2 * time.Minute

// smokeHarnesses returns the harnesses to exercise, honoring E2E_HARNESS
// (single-harness CI matrix leg) exactly like TestE2EEchoRest.
func smokeHarnesses(t *testing.T) []harnessConfig {
	t.Helper()
	if name := os.Getenv("E2E_HARNESS"); name != "" {
		cfg, ok := harnessByName(name)
		if !ok {
			t.Fatalf("E2E_HARNESS=%q not a known harness", name)
		}
		return []harnessConfig{cfg}
	}
	return allHarnesses()
}

// TestHarnessCLIContract installs each harness and verifies omac's CLI
// assumptions against the installed version's --help. Model-free.
func TestHarnessCLIContract(t *testing.T) {
	for _, h := range smokeHarnesses(t) {
		t.Run(h.Name, func(t *testing.T) {
			home := t.TempDir()
			installHarness(t, h, home)

			version := harnessVersion(t, h, home)
			t.Logf("%s resolved version: %q", h.Name, version)

			contract, ok := deriveContract(h.Name)
			if !ok {
				t.Fatalf("deriveContract(%q) returned ok=false", h.Name)
			}

			corpus := helpCorpus(t, h, home, contract.Subcommands)
			missing := missingTokens(corpus, contract.Tokens)

			result := "PASS"
			if len(missing) > 0 {
				result = "FAIL"
			}
			// Emit the compat line before asserting so the matrix records the
			// outcome even when the assertion fails the test.
			emitCompat(compatLine(h.Name, version, runtime.GOOS, "contract", result))

			if len(missing) > 0 {
				var b strings.Builder
				b.WriteString("CLI contract drift — the installed ")
				b.WriteString(h.Name)
				b.WriteString(" version no longer exposes flags/subcommands omac depends on:\n")
				for _, m := range missing {
					b.WriteString("  - ")
					b.WriteString(m.Token)
					b.WriteString("  (omac uses it in: ")
					b.WriteString(m.Source)
					b.WriteString(")\n")
				}
				b.WriteString("Fix the harness descriptor (internal/config/harness.go) or the e2e config " +
					"(internal/e2e/harnesses.go) to match the new CLI, then re-run.")
				ghaContractAnnotation(t, h.Name, missing)
				t.Fatalf("%s", b.String())
			}
			t.Logf("%s: all %d contract tokens present", h.Name, len(contract.Tokens))
		})
	}
}

// TestHarnessLaunchProbe verifies omac can launch each harness through the real
// sandbox and reach the actual binary, without a model call, by running the
// harness's own `--version` as the inner command.
func TestHarnessLaunchProbe(t *testing.T) {
	for _, h := range smokeHarnesses(t) {
		t.Run(h.Name, func(t *testing.T) {
			home := t.TempDir()
			workdir := t.TempDir()

			// Runtime dirs some harnesses expect; the sandbox skips nonexistent
			// allow paths, so create them before launch (mirrors runE2E).
			for _, dir := range []string{".cache", ".cache/opencode", ".local/share/opencode", ".local/state/opencode/locks"} {
				if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			omacBin := buildOmac(t)
			installHarness(t, h, home)
			version := harnessVersion(t, h, home)
			writeSandboxProfile(t, home, h, nil)

			ctx, cancel := context.WithTimeout(context.Background(), launchProbeTimeout)
			defer cancel()

			args := []string{"start", h.Name}
			if h.Sandbox.NoSandbox {
				args = append(args, "--no-sandbox")
			}
			args = append(args, "--", "--version")

			cmd := exec.CommandContext(ctx, omacBin, args...)
			cmd.Dir = workdir
			cmd.Env = append(buildAgentEnv(t, h, home), "PWD="+workdir)
			cmd.Stdin = strings.NewReader("")
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr

			t.Logf("launch probe: omac %s", strings.Join(args, " "))
			err := cmd.Run()
			combined := stdout.String() + "\n" + stderr.String()

			result := "PASS"
			if err != nil || ctx.Err() == context.DeadlineExceeded {
				result = "FAIL"
			}
			emitCompat(compatLine(h.Name, version, runtime.GOOS, "launch", result))

			if ctx.Err() == context.DeadlineExceeded {
				t.Fatalf("launch probe timed out after %v\nSTDOUT:\n%s\nSTDERR:\n%s",
					launchProbeTimeout, tailLines(stdout.String(), 100), tailLines(stderr.String(), 100))
			}
			if err != nil {
				t.Fatalf("omac start ... -- --version failed: %v\nSTDOUT:\n%s\nSTDERR:\n%s",
					err, stdout.String(), stderr.String())
			}
			if strings.TrimSpace(combined) == "" {
				t.Fatalf("launch probe produced no output — inner binary may not have run")
			}
			t.Logf("%s launch probe ok (%d bytes of --version output)", h.Name, len(strings.TrimSpace(combined)))
		})
	}
}

// harnessVersion runs `<bin> --version` in the temp HOME and returns the
// sanitized version string. Fails the test if the binary can't report a
// version (a strong signal the install itself drifted).
func harnessVersion(t *testing.T, h harnessConfig, home string) string {
	t.Helper()
	out, err := runInHome(t, home, h.BinaryName+" --version", smokeCmdTimeout)
	if err != nil {
		t.Fatalf("%s --version failed: %v\n%s", h.BinaryName, err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Fatalf("%s --version produced no output", h.BinaryName)
	}
	return parseVersion(out)
}

// helpCorpus concatenates `<bin> --help` and `<bin> <sub> --help` for every
// subcommand omac invokes, so a flag documented only under a subcommand still
// resolves. Help output frequently goes to stderr and/or exits non-zero, so
// this captures combined output and ignores exit status.
func helpCorpus(t *testing.T, h harnessConfig, home string, subs []string) string {
	t.Helper()
	var b strings.Builder
	out, _ := runInHome(t, home, h.BinaryName+" --help", smokeCmdTimeout)
	b.WriteString(out)
	b.WriteString("\n")
	for _, sub := range subs {
		subOut, _ := runInHome(t, home, h.BinaryName+" "+sub+" --help", smokeCmdTimeout)
		b.WriteString(subOut)
		b.WriteString("\n")
	}
	return b.String()
}

// runInHome runs a simple command line via `sh -c` with the temp-HOME
// environment (so the harness binary on the temp PATH resolves), capturing
// combined stdout+stderr. cmdline must contain only trusted, shell-safe tokens
// (binary name + fixed flags) — never interpolated user input.
func runInHome(t *testing.T, home, cmdline string, timeout time.Duration) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", cmdline)
	cmd.Env = withHome(os.Environ(), home)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// emitCompat prints a machine-readable compatibility line to stdout for the
// workflow to parse (and echoes it to the test log via nothing extra — it is
// already visible under `go test -v`).
func emitCompat(line string) {
	// A bare Println (not t.Logf) keeps the line unindented and greppable on
	// stdout regardless of the test's verbosity framing.
	fmt.Println(line)
}

// ghaContractAnnotation surfaces contract drift as a GitHub Actions error
// annotation so the run summary shows which flags disappeared without opening
// the log. No-op outside GHA.
func ghaContractAnnotation(t *testing.T, harness string, missing []contractToken) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	toks := make([]string, 0, len(missing))
	for _, m := range missing {
		toks = append(toks, m.Token)
	}
	fmt.Println("::error title=CLI contract drift (" + harness + ")::" +
		harness + " no longer exposes: " + strings.Join(toks, ", "))
}
