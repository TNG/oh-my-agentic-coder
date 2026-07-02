//go:build e2e

// E2E tests for the omac harness×skill matrix.
//
// Each subtest installs a harness (opencode, claude-code, codex, copilot)
// into a temp HOME, registers the bundled echo-rest skill, starts omac
// with the sandbox, and prompts the agent to call the skill's /status
// endpoint. The test passes if the agent output contains {"ok":true}.
//
// Per-harness environment adaptation (env vars, config files, sandbox
// deviations) is declared in harnesses.go — see the doc comment on each
// *Config() function for the full list of assumptions.
//
// Required CI secrets / env vars:
//
//   SKAINET_TOKEN         — API key for the model provider (all harnesses except claude-code)
//   SKAINET_INTERNAL      — Model provider base URL (responses API; codex, copilot, opencode)
//   ANTHROPIC_BASE_URL    — Anthropic-compatible proxy URL (claude-code only)
//
// The sandbox profile is derived at runtime from SKAINET_INTERNAL /
// ANTHROPIC_BASE_URL so the proxy allows the model API host.
//
// Run locally: go test -tags=e2e -timeout=30m -v ./internal/e2e/
// Run one:     E2E_HARNESS=opencode go test -tags=e2e -timeout=30m -v ./internal/e2e/
// Latest:      E2E_USE_LATEST=1 go test -tags=e2e -timeout=30m -v ./internal/e2e/
//
// Harness versions and model IDs are pinned in versions.go.
// Set E2E_USE_LATEST=1 to test with latest releases (no pinning).

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// runTimeout is the per-agent-run deadline.
const runTimeout = 5 * time.Minute

// TestE2EEchoRest runs the full start→use lifecycle for each harness.
// Set E2E_HARNESS to run a single harness (used by CI matrix).
func TestE2EEchoRest(t *testing.T) {
	harnesses := allHarnesses()
	if h := os.Getenv("E2E_HARNESS"); h != "" {
		cfg, ok := harnessByName(h)
		if !ok {
			t.Fatalf("E2E_HARNESS=%q not a known harness", h)
		}
		harnesses = []harnessConfig{cfg}
	}

	for _, h := range harnesses {
		t.Run(h.Name, func(t *testing.T) {
			runE2E(t, h)
		})
	}
}

func runE2E(t *testing.T, h harnessConfig) {
	home := t.TempDir()
	workdir := t.TempDir()

	// Create cache dirs that harnesses expect to write to at runtime.
	// The sandbox ExpandExisting skips nonexistent allow paths, so these
	// must exist before the sandbox starts.
	for _, dir := range []string{".cache", ".cache/opencode", ".local/share/opencode", ".local/state/opencode/locks"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Build omac binary.
	omacBin := buildOmac(t)

	// 2. Install harness into temp HOME.
	installHarness(t, h, home)

	// 3. Write provider config.
	h.ProviderSetup(t, home)

	// 4. Write sandbox profile allowing the model provider domains.
	writeSandboxProfile(t, home, h, nil)

	// 5. Copy echo-rest skill into workdir skills dir.
	copySkill(t, h, workdir, "echo-rest")

	// 6. Register echo-rest (no secrets, no fields — all optional).
	registerEchoRest(t, omacBin, home, workdir)

	// 7. Run agent: call the echo-rest /status endpoint.
	prompt := "Use the echo-rest skill to check its health. " +
		"Call curl on the OMAC_ECHO_BASE/status endpoint and report the response."
	stdout := runAgent(t, h, omacBin, home, workdir, prompt)

	// 8. Assert output contains the health-check ok flag.
	assertEchoOK(t, stdout)
}

// auditSecretValue is the plaintext secret injected via env_passthrough.
// The test asserts this string NEVER appears in the agent's stdout.
const auditSecretValue = "test-secret-value-123"

// TestE2ESecurityAudit verifies sandbox security properties using an
// explicit allowance spec (see allowance.go). For each harness it:
//
//  1. Writes a sandbox profile with environment.allow_vars set to the
//     spec's allow-list (so FilterEnv strips everything not listed).
//  2. Registers the self-audit skill with AUDIT_SECRET delivered via
//     env_passthrough to the sidecar only.
//  3. Prompts the agent to run all probes (secret, env, fs, network,
//     sidecar connectivity).
//  4. Asserts:
//     - NEGATIVE: AUDIT_SECRET not in output (secret isolation).
//     - NEGATIVE: denied env vars not in output (env filtering).
//     - NEGATIVE: filesystem denials present (fs isolation).
//     - NEGATIVE: network denial present (network filtering).
//     - POSITIVE: allow-listed env vars ARE visible (sandbox passes them).
//     - POSITIVE: sidecar fingerprint IS present (facade works).
//
// Harnesses running with --no-sandbox (codex on macOS) skip the
// negative assertions — there is no sandbox to enforce them.
func TestE2ESecurityAudit(t *testing.T) {
	harnesses := allHarnesses()
	if h := os.Getenv("E2E_HARNESS"); h != "" {
		cfg, ok := harnessByName(h)
		if !ok {
			t.Fatalf("E2E_HARNESS=%q not a known harness", h)
		}
		harnesses = []harnessConfig{cfg}
	}

	for _, h := range harnesses {
		t.Run(h.Name, func(t *testing.T) {
			runSecurityAudit(t, h)
		})
	}
}

func runSecurityAudit(t *testing.T, h harnessConfig) {
	home := t.TempDir()
	workdir := t.TempDir()

	for _, dir := range []string{".cache", ".cache/opencode", ".local/share/opencode", ".local/state/opencode/locks"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	spec := allowanceSpecFor(h)
	t.Logf("allowance spec for %s: allow=%v deny=%v fsDeny=%v netDeny=%s",
		h.Name, spec.EnvAllowVars, spec.EnvDenyVars, spec.FsDenyPaths, spec.NetDenyDomain)

	omacBin := buildOmac(t)
	installHarness(t, h, home)
	h.ProviderSetup(t, home)
	writeSandboxProfile(t, home, h, &spec)
	copySkill(t, h, workdir, "self-audit")
	registerSelfAudit(t, omacBin, home, workdir)

	prompt := "Run this command and print its full output verbatim:\n\n" +
		`sh "$OMAC_HARNESS_SKILLS_DIR/self-audit/scripts/audit.sh"` + "\n\n" +
		"Do not summarize. Do not skip any output. Print every line the command produces."
	stdout := runAuditAgent(t, h, omacBin, home, workdir, prompt)

	sandboxActive := !h.Sandbox.NoSandbox

	// --- NEGATIVE assertions (things that must NOT happen) ---

	if sandboxActive {
		assertSecretNotLeaked(t, stdout)
		assertEnvVarsDenied(t, stdout, spec.EnvDenyVars)
		assertFilesystemDenied(t, stdout)
		assertNetworkDenied(t, stdout, spec.NetDenyDomain)
	} else {
		t.Logf("skipping negative assertions: %s runs with --no-sandbox", h.Name)
	}

	// --- POSITIVE assertions (things that MUST happen) ---

	// Sidecar should be reachable regardless of sandbox state.
	assertSecretFingerprintPresent(t, stdout)

	if sandboxActive {
		assertEnvVarsVisible(t, stdout, spec.EnvExpectVisible)
	} else {
		t.Logf("skipping positive env assertions: %s runs with --no-sandbox", h.Name)
	}
}

// buildOmac compiles the omac binary into a temp dir and returns its path.
func buildOmac(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "omac")
	// Build from repo root (test CWD is internal/e2e/).
	repoRoot := filepath.Join("..", "..")
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", binPath, "./cmd/omac")
	cmd.Dir = repoRoot
	cmd.Env = os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build omac: %v\n%s", err, out)
	}
	return binPath
}

// installHarness installs the harness CLI into the temp HOME.
func installHarness(t *testing.T, h harnessConfig, home string) {
	t.Helper()
	t.Logf("installing %s: %v", h.Name, h.InstallCmd)
	cmd := exec.Command(h.InstallCmd[0], h.InstallCmd[1:]...)
	cmd.Env = withHome(os.Environ(), home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install %s: %v\n%s", h.Name, err, out)
	}
	// Verify the binary is on PATH.
	env := withHome(os.Environ(), home)
	binPath, err := exec.LookPath(h.BinaryName)
	if err != nil {
		// exec.LookPath uses the parent's PATH, not the subprocess env.
		// Fall back to checking with the subprocess env via a shell.
		lookupCmd := exec.Command("sh", "-c", "command -v "+h.BinaryName)
		lookupCmd.Env = env
		lookupOut, lerr := lookupCmd.CombinedOutput()
		if lerr != nil {
			t.Fatalf("harness binary %q not on PATH after install: %v\n%s", h.BinaryName, lerr, lookupOut)
		}
		binPath = strings.TrimSpace(string(lookupOut))
	}
	t.Logf("%s installed at %s", h.BinaryName, binPath)
	if h.ExtraInstallSteps != nil {
		h.ExtraInstallSteps(t, home)
	}
}

// copySkill copies a skill from the repo's bundled .opencode/skills/<name>/
// into the workdir's harness-scoped skills directory.
func copySkill(t *testing.T, h harnessConfig, workdir, skillName string) {
	t.Helper()
	// Skills are bundled in the repo at .opencode/skills/<name>/.
	// The test binary runs from internal/e2e/, so ../../.opencode/skills/<name>.
	srcCandidates := []string{
		filepath.Join("..", "..", ".opencode", "skills", skillName),
		filepath.Join("..", "..", "..", ".opencode", "skills", skillName),
	}
	var src string
	for _, c := range srcCandidates {
		if abs, err := filepath.Abs(c); err == nil {
			if info, err := os.Stat(abs); err == nil && info.IsDir() {
				src = abs
				break
			}
		}
	}
	if src == "" {
		t.Fatalf("skill %q not found in repo; the test requires .opencode/skills/%s/", skillName, skillName)
	}
	dst := filepath.Join(workdir, h.SkillsBase, "skills", skillName)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cp", "-r", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copy %s: %v\n%s", skillName, err, out)
	}
	t.Logf("%s copied to %s", skillName, dst)
}

// registerEchoRest runs `omac register echo-rest --no-secrets --no-fields`
// in the workdir. echo-rest's secrets and config fields are all optional.
func registerEchoRest(t *testing.T, omacBin, home, workdir string) {
	t.Helper()
	cmd := exec.Command(omacBin, "register", "echo-rest", "--no-secrets", "--no-fields")
	cmd.Dir = workdir
	cmd.Env = withHome(os.Environ(), home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("register echo-rest: %v\n%s", err, out)
	}
	t.Logf("echo-rest registered")
}

// registerSelfAudit runs `omac register self-audit --no-secrets`
// in the workdir. The AUDIT_SECRET is supplied via env_passthrough at
// start time, not the keychain.
func registerSelfAudit(t *testing.T, omacBin, home, workdir string) {
	t.Helper()
	cmd := exec.Command(omacBin, "register", "self-audit", "--no-secrets")
	cmd.Dir = workdir
	cmd.Env = withHome(os.Environ(), home)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("register self-audit: %v\n%s", err, out)
	}
	t.Logf("self-audit registered")
}

// runAgent starts omac with the harness, passes the prompt as inner args,
// and returns the agent's stdout. Fails on timeout or non-zero exit.
func runAgent(t *testing.T, h harnessConfig, omacBin, home, workdir, prompt string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	innerArgs := h.RunArgs(prompt)
	args := []string{"start", h.Name}
	if h.Sandbox.NoSandbox {
		args = append(args, "--no-sandbox")
	}
	args = append(args, "--")
	args = append(args, innerArgs...)

	cmd := exec.CommandContext(ctx, omacBin, args...)
	cmd.Dir = workdir
	cmd.Env = append(buildAgentEnv(t, h, home), "PWD="+workdir)
	cmd.Stdin = strings.NewReader("")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	t.Logf("running: omac %s (prompt: %q)", h.Name, truncate(prompt, 80))
	err := cmd.Run()
	profPath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	env := buildAgentEnv(t, h, home)
	if ctx.Err() == context.DeadlineExceeded {
		writeSessionArtifacts(t, h, "echo-rest", home, workdir, prompt, stdout.String(), stderr.String(), env, profPath)
		t.Fatalf("agent did not exit within %v\nSTDOUT (last 200 lines):\n%s\nSTDERR (last 200 lines):\n%s",
			runTimeout, tailLines(stdout.String(), 200), tailLines(stderr.String(), 200))
	}
	if err != nil {
		dumpSidecarLogs(t, workdir, home)
		writeSessionArtifacts(t, h, "echo-rest", home, workdir, prompt, stdout.String(), stderr.String(), env, profPath)
		t.Fatalf("omac start failed: %v\nSTDOUT:\n%s\nSTDERR:\n%s",
			err, stdout.String(), stderr.String())
	}
	writeSessionArtifacts(t, h, "echo-rest", home, workdir, prompt, stdout.String(), stderr.String(), env, profPath)
	return stdout.String()
}

// runAuditAgent starts omac with the harness and the AUDIT_SECRET env
// var set for env_passthrough. Otherwise identical to runAgent.
func runAuditAgent(t *testing.T, h harnessConfig, omacBin, home, workdir, prompt string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	innerArgs := h.RunArgs(prompt)
	args := []string{"start", h.Name}
	if h.Sandbox.NoSandbox {
		args = append(args, "--no-sandbox")
	}
	args = append(args, "--")
	args = append(args, innerArgs...)

	cmd := exec.CommandContext(ctx, omacBin, args...)
	cmd.Dir = workdir
	env := buildAgentEnv(t, h, home)
	env = append(env, "AUDIT_SECRET="+auditSecretValue)
	env = append(env, "PWD="+workdir)
	cmd.Env = env
	cmd.Stdin = strings.NewReader("")

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	t.Logf("running: omac %s (prompt: %q)", h.Name, truncate(prompt, 80))
	err := cmd.Run()
	profPath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	if ctx.Err() == context.DeadlineExceeded {
		writeSessionArtifacts(t, h, "security-audit", home, workdir, prompt, stdout.String(), stderr.String(), env, profPath)
		t.Fatalf("agent did not exit within %v\nSTDOUT (last 200 lines):\n%s\nSTDERR (last 200 lines):\n%s",
			runTimeout, tailLines(stdout.String(), 200), tailLines(stderr.String(), 200))
	}
	if err != nil {
		dumpSidecarLogs(t, workdir, home)
		writeSessionArtifacts(t, h, "security-audit", home, workdir, prompt, stdout.String(), stderr.String(), env, profPath)
		t.Fatalf("omac start failed: %v\nSTDOUT:\n%s\nSTDERR:\n%s",
			err, stdout.String(), stderr.String())
	}
	writeSessionArtifacts(t, h, "security-audit", home, workdir, prompt, stdout.String(), stderr.String(), env, profPath)
	return stdout.String()
}

// buildAgentEnv constructs the environment for the omac start subprocess.
// It sets HOME (via withHome) and adds harness-specific env vars from
// h.EnvVars. SKAINET_TOKEN propagates via os.Environ() inheritance.
func buildAgentEnv(t *testing.T, h harnessConfig, home string) []string {
	t.Helper()
	env := withHome(os.Environ(), home)
	env = append(env, h.EnvVars(t)...)
	return env
}

// okRe matches the echo-rest /status health response: {"ok":true}.
var okRe = regexp.MustCompile(`"ok"\s*:\s*true`)

// assertEchoOK checks the agent's output contains the echo-rest health response.
func assertEchoOK(t *testing.T, output string) {
	t.Helper()
	if !okRe.MatchString(output) {
		t.Errorf("agent output does not contain echo-rest health response\nOUTPUT:\n%s", tailLines(output, 200))
		return
	}
	t.Logf("echo-rest /status response found in agent output")
}

// assertSecretNotLeaked verifies the plaintext AUDIT_SECRET value does
// not appear anywhere in the agent's output. If it does, the sandbox
// leaked the secret into the agent's environment.
func assertSecretNotLeaked(t *testing.T, output string) {
	t.Helper()
	if strings.Contains(output, auditSecretValue) {
		t.Errorf("SECURITY FAIL: plaintext AUDIT_SECRET value found in agent output\n" +
			"the sandbox leaked the secret into the agent's environment")
		return
	}
	t.Logf("PASS: secret isolation — plaintext secret not found in agent output")
}

// assertSecretFingerprintPresent verifies the agent called the
// self-audit skill's /whoami endpoint by checking for the sha256
// fingerprint in the output (positive assertion — sidecar is reachable).
func assertSecretFingerprintPresent(t *testing.T, output string) {
	t.Helper()
	fingerprintRe := regexp.MustCompile(`sha256:[0-9a-f]{12}`)
	if !fingerprintRe.MatchString(output) {
		t.Errorf("POSITIVE FAIL: agent output does not contain secret fingerprint;\n" +
			"the agent may not have called the self-audit skill's /whoami endpoint")
		return
	}
	t.Logf("PASS: sidecar reachable — secret fingerprint found in agent output")
}

// assertEnvVarsDenied verifies that none of the denied env vars appear
// in the agent's env output. Each denied var is checked by looking for
// "VARNAME=" in the output.
func assertEnvVarsDenied(t *testing.T, output string, denyVars []string) {
	t.Helper()
	for _, v := range denyVars {
		needle := v + "="
		if strings.Contains(output, needle) {
			t.Errorf("SECURITY FAIL: %s visible in agent env output\n"+
				"the sandbox did not filter this env var", v)
		}
	}
	t.Logf("PASS: env filtering — denied vars not in agent output")
}

// assertEnvVarsVisible verifies that the expected env vars ARE visible
// in the agent's output (positive assertion — sandbox passes them through).
func assertEnvVarsVisible(t *testing.T, output string, expectVars []string) {
	t.Helper()
	for _, v := range expectVars {
		if !strings.Contains(output, v) {
			t.Errorf("POSITIVE FAIL: %s not found in agent env output\n"+
				"the sandbox may be over-filtering env vars", v)
			return
		}
	}
	t.Logf("PASS: env passthrough — expected vars visible in agent output")
}

// assertFilesystemDenied verifies that filesystem probes were denied
// by the sandbox. We check for OS-level denial messages.
func assertFilesystemDenied(t *testing.T, output string) {
	t.Helper()
	denials := []string{
		"Permission denied",
		"No such file or directory",
		"cannot open",
		"Operation not permitted",
	}
	found := false
	for _, d := range denials {
		if strings.Contains(output, d) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SECURITY FAIL: no filesystem denial message found in agent output\n" +
			"the sandbox may not be enforcing filesystem isolation")
		return
	}
	t.Logf("PASS: filesystem isolation — denial message found in agent output")
}

// assertNetworkDenied verifies that the network probe was blocked
// by the sandbox. We check for connection failure messages.
func assertNetworkDenied(t *testing.T, output string, denyDomain string) {
	t.Helper()
	denials := []string{
		"Connection refused",
		"Could not resolve host",
		"Connection timed out",
		"Failed to connect",
		"curl: (6)",             // Could not resolve host
		"curl: (7)",             // Failed to connect
		"curl: (28)",            // Operation timed out
		"DENIED BY THE SANDBOX", // omac proxy denial body
		"403",                   // HTTP 403 from proxy
	}
	found := false
	for _, d := range denials {
		if strings.Contains(output, d) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SECURITY FAIL: no network denial message found in agent output\n"+
			"the sandbox may not be enforcing network egress filtering for %s", denyDomain)
		return
	}
	t.Logf("PASS: network isolation — denial message found in agent output")
}

// truncate shortens s to at most n chars, appending "…" if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// tailLines returns the last n lines of s. If s has fewer lines, returns s.
func tailLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// dumpSidecarLogs reads sidecar log files under the omac runtime dir
// (${TMPDIR}/omac-*/logs/*.log) and logs their contents. Helps diagnose
// health check timeouts — the sidecar's stderr/stdout goes there.
func dumpSidecarLogs(t *testing.T, workdir, home string) {
	t.Helper()
	// rtDir is ${TMPDIR}/omac-<hash>, not under workdir. Glob broadly.
	pattern := filepath.Join(os.TempDir(), "omac-*", "logs", "*.log")
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		t.Logf("=== sidecar log: %s ===\n%s", filepath.Base(m), tailLines(string(data), 100))
	}
	// Dump opencode's own log file (helps diagnose "Session not found").
	ocLog := filepath.Join(home, ".local", "share", "opencode", "log", "opencode.log")
	if data, err := os.ReadFile(ocLog); err == nil {
		t.Logf("=== opencode.log ===\n%s", tailLines(string(data), 200))
	}
}

// writeSandboxProfile writes ~/.config/omac/sandbox-profiles/default.json
// into the temp HOME.
//
// When spec is non-nil, sets environment.allow_vars so FilterEnv strips
// everything not on the allow-list. This is the security audit path —
// the allow-list is the single source of truth for what the agent sees.
//
// Base profile (applies to all harnesses):
//
//	workdir        — readwrite
//	network        — filtered, listen_port 4097 (echo-rest), allow_tcp_connect 22 (SSH)
//	filesystem.allow — ~/.cache, ~/.local/share, ~/.local/state, ~/.bun,
//	                     ~/Library/Caches, ~/go, ~/.rustup, ~/.cargo
//	filesystem.read  — ~/.gitconfig, ~/.gitignore_global, ~/.config
//
// Per-harness deviations (h.Sandbox):
//
//	ExtraAllowDomains — additional domains beyond the model provider host
//	ExtraReadPaths    — additional filesystem read paths
//
// The model provider host (from SKAINET_INTERNAL / ANTHROPIC_BASE_URL) is
// always allowed — it is derived at runtime so the sandbox proxy doesn't
// deny the agent's API calls.
func writeSandboxProfile(t *testing.T, home string, h harnessConfig, spec *AllowanceSpec) {
	t.Helper()
	allowDomains := []string{}
	for _, envVar := range []string{"SKAINET_INTERNAL", "ANTHROPIC_BASE_URL"} {
		if baseURL := os.Getenv(envVar); baseURL != "" {
			if host := extractHost(baseURL); host != "" {
				allowDomains = append(allowDomains, host)
			}
		}
	}
	allowDomains = append(allowDomains, h.Sandbox.ExtraAllowDomains...)

	readPaths := []string{
		"~/.gitconfig",
		"~/.gitignore_global",
		"~/.config",
	}
	readPaths = append(readPaths, h.Sandbox.ExtraReadPaths...)

	profile := map[string]any{
		"meta":    map[string]string{"name": "default"},
		"workdir": map[string]string{"access": "readwrite"},
		"filesystem": map[string]any{
			"allow": []string{
				"~/.cache",
				"~/.local/share",
				"~/.local/state",
				"~/.bun",
				"~/Library/Caches",
				"~/go",
				"~/.rustup",
				"~/.cargo",
			},
			"read": readPaths,
		},
		"network": map[string]any{
			"mode":              "filtered",
			"listen_port":       []int{4097},
			"allow_tcp_connect": []int{22},
			"allow_domain":      allowDomains,
		},
	}

	if spec != nil && len(spec.EnvAllowVars) > 0 {
		profile["environment"] = map[string]any{
			"allow_vars": spec.EnvAllowVars,
		}
	}

	profDir := filepath.Join(home, ".config", "omac", "sandbox-profiles")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(profile, "", "  ")
	if err := os.WriteFile(filepath.Join(profDir, "default.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	allowVarsCount := 0
	if spec != nil {
		allowVarsCount = len(spec.EnvAllowVars)
	}
	t.Logf("sandbox profile written with %d allow_domain entries, %d allow_vars",
		len(allowDomains), allowVarsCount)
}

// extractHost parses a URL string and returns the hostname.
func extractHost(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
