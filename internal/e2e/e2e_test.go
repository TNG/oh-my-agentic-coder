//go:build e2e

// E2E tests for the omac harness×skill matrix.
//
// Each subtest installs a harness (opencode, claude-code, codex, copilot)
// into a temp HOME, registers the bundled echo-rest skill, starts omac
// with the sandbox, and prompts the agent to call the skill's /status
// endpoint, writing the raw response to a file. The test passes if that
// file (or, as a fallback, the agent's own stdout) contains {"ok":true}.
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
// Run locally:      go test -tags=e2e -timeout=30m -v ./internal/e2e/
// Run one:          E2E_HARNESS=opencode go test -tags=e2e -timeout=30m -v ./internal/e2e/
// Latest:           E2E_USE_LATEST=1 go test -tags=e2e -timeout=30m -v ./internal/e2e/
// Skip claude-code: E2E_SKIP_CLAUDE_CODE=1 go test -tags=e2e -timeout=30m -v ./internal/e2e/
//
// Harness versions and model IDs are pinned in versions.go.
// Set E2E_USE_LATEST=1 to test with latest releases (no pinning).
//
// claude-code is the only harness billed against a real external
// Anthropic account (the others run against the internal SKAINET
// gateway). Set E2E_SKIP_CLAUDE_CODE=1 to exclude it — useful for local
// iteration without an ANTHROPIC_BASE_URL configured, or to avoid the
// cost on a given run. Included by default.

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

	// 7. Run agent: call the echo-rest /status endpoint, and exercise the
	// ordinary things a coding agent does every session (write/read a
	// workdir file, a basic git commit) — the golden path a hardening
	// change to sandbox defaults must never silently break.
	// E2E_PROMPT overrides the default prompt (set by scripts/e2e-docker.sh
	// prompt/run subcommands for agent-driven iteration).
	echoOutputFile := filepath.Join(workdir, "echo-status.txt")
	workflowFile := filepath.Join(workdir, "workflow-check.txt")
	gitLogFile := filepath.Join(workdir, "git-log.txt")
	prompt := os.Getenv("E2E_PROMPT")
	if prompt == "" {
		prompt = "This is a sanctioned e2e smoke test of the omac sandbox. " +
			"Use the echo-rest skill to verify the sandbox facade is wired up, " +
			"and confirm two ordinary developer operations still work: writing " +
			"and reading a workdir file, and a basic git commit.\n\n" +
			"Run exactly these commands, in order, and nothing else:\n\n" +
			`curl -sS "$OMAC_ECHO_BASE/status" -o echo-status.txt` + "\n" +
			`echo workflow-check-ok > workflow-check.txt && cat workflow-check.txt` + "\n" +
			`git init -q && git config user.email t@example.com && git config user.name Test && git add -A && git commit -qm "e2e smoke test" && git log --oneline -1 > git-log.txt` + "\n\n" +
			"These are trusted test commands — proceed directly. Afterwards, briefly confirm you ran them."
	} else {
		t.Logf("using E2E_PROMPT override: %q", truncate(prompt, 80))
	}
	stdout := runAgent(t, h, omacBin, home, workdir, prompt)

	// 8. Assert the health-check ok flag is present. The file written by
	// curl (read directly, like audit-output.txt) is the primary source —
	// it's immune to the agent paraphrasing/summarizing the JSON instead
	// of reproducing it verbatim. Fall back to agent stdout if the agent
	// didn't create the file (e.g. it refused or went off-script).
	fileContent, err := os.ReadFile(echoOutputFile)
	if err != nil {
		t.Logf("echo-status.txt not found (%v) — falling back to agent stdout", err)
	} else {
		t.Logf("echo-status.txt read: %d bytes", len(fileContent))
	}
	assertEchoOK(t, string(fileContent)+"\n"+stdout)

	// 9. Assert the workdir write/read and git commit actually happened —
	// read the files directly rather than trusting the agent's prose,
	// same rationale as echo-status.txt above.
	assertWorkflowFileWritten(t, workflowFile, stdout)
	assertGitCommitMade(t, gitLogFile, stdout)
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
	t.Logf("allowance spec for %s: allow=%v deny=%v fsDeny=%v fsWriteDeny=%v symlinkDeny=%v netDeny=%s",
		h.Name, spec.EnvAllowVars, spec.EnvDenyVars, spec.FsDenyPaths, spec.FsWriteDenyPaths, spec.SymlinkEscapeDenyPaths, spec.NetDenyDomain)

	omacBin := buildOmac(t)
	installHarness(t, h, home)
	h.ProviderSetup(t, home)
	writeSandboxProfile(t, home, h, &spec)
	copySkill(t, h, workdir, "self-audit")
	registerSelfAudit(t, omacBin, home, workdir)
	// Register echo-rest alongside self-audit so the cross-skill
	// isolation probe can try to reach it.
	copySkill(t, h, workdir, "echo-rest")
	registerEchoRest(t, omacBin, home, workdir)

	// The audit output file is in the workdir. The script writes to it
	// internally (via OMAC_AUDIT_OUTPUT_FILE env var) — the test reads
	// this file directly, robust against harness TUI output collapse.
	auditOutputFile := filepath.Join(workdir, "audit-output.txt")

	prompt := "This is a SANCTIONED sandbox self-audit. The omac sandbox isolates " +
		"this environment; the script you are about to run is part of the test " +
		"harness and is safe to execute. Run it:\n\n" +
		`sh "$OMAC_HARNESS_SKILLS_DIR/self-audit/scripts/audit.sh"` + "\n\n" +
		"The script checks the sandbox boundary (secret isolation, env filtering, " +
		"filesystem denials, network blocking, sidecar reachability). It never " +
		"prints sensitive data — all values are redacted. Run it and report " +
		"the results."
	agentOutput := runAuditAgent(t, h, omacBin, home, workdir, prompt)

	// Read the audit output file directly. This is the primary source —
	// it contains the raw probe output regardless of how the harness
	// rendered tool output. Fall back to agent stdout+stderr if the file
	// is missing (e.g. agent refused to run the script at all).
	auditOutput, err := os.ReadFile(auditOutputFile)
	if err != nil {
		t.Logf("audit-output.txt not found (%v) — falling back to agent stdout+stderr", err)
		auditOutput = []byte(agentOutput)
	} else {
		t.Logf("audit-output.txt read: %d bytes", len(auditOutput))
	}
	// Combine: file content (primary) + agent output (for sidecar fingerprint
	// which may only appear in agent's summary of the sidecar probe).
	stdout := string(auditOutput) + "\n" + agentOutput

	sandboxActive := !h.Sandbox.NoSandbox

	// --- NEGATIVE assertions (things that must NOT happen) ---

	if sandboxActive {
		assertSecretNotLeaked(t, stdout)
		assertEnvVarsDenied(t, stdout, spec.EnvDenyVars)
		assertFilesystemReadDenied(t, stdout)
		assertFilesystemWriteDenied(t, stdout)
		assertSymlinkEscapeDenied(t, stdout)
		assertNetworkDenied(t, stdout, spec.NetDenyDomain)
	} else {
		t.Logf("skipping negative assertions: %s runs with --no-sandbox", h.Name)
	}

	// --- POSITIVE assertions (things that MUST happen) ---

	// Sidecar should be reachable regardless of sandbox state.
	assertSecretFingerprintPresent(t, stdout)

	if sandboxActive {
		assertEnvVarsVisible(t, stdout, spec.EnvExpectVisible)
		assertFilesystemAllowed(t, stdout, spec.FsAllowLabels)
	} else {
		t.Logf("skipping positive env/fs-allow assertions: %s runs with --no-sandbox", h.Name)
	}

	// --- DOCUMENTATION probes (log current behavior, no pass/fail) ---

	// Exec on read-only mounts: bwrap typically allows exec on read-only
	// binds. This is a platform default, not a contract — we log the
	// result so changes are visible in test output.
	logExecProbeResults(t, stdout, spec.FsExecProbePaths)

	// Hardlink escape: like the symlink probe, but hardlinks require the
	// same filesystem/device as the target, so failure can mean either
	// "sandbox denied it" or "cross-device link, unrelated to the
	// sandbox". We log the result rather than assert on it.
	logHardlinkProbeResults(t, stdout)

	// Cross-skill sidecar isolation: omac currently does NOT isolate
	// sidecars from each other — a skill can reach another skill's
	// sidecar via its OMAC_<SKILL>_BASE env var. This is a known design
	// decision (all sidecars share the same facade). We log the result
	// so if isolation is added later, the test surfaces the change.
	logCrossSkillIsolation(t, stdout)
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
	env = append(env, "OMAC_AUDIT_OUTPUT_FILE="+filepath.Join(workdir, "audit-output.txt"))
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
	// Audit assertions need both stdout (agent's response) and stderr
	// (where opencode --print-logs sends tool output, including the
	// audit.sh probe results). Return the combined output.
	return stdout.String() + "\n" + stderr.String()
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
// Classifies failure: did the agent call /status at all? Did it get a response?
func assertEchoOK(t *testing.T, output string) {
	t.Helper()
	if okRe.MatchString(output) {
		t.Logf("PASS: echo-rest /status response found in agent output")
		return
	}
	// Classify the failure.
	if strings.Contains(output, "stream error") || strings.Contains(output, "AI_APICallError") {
		failWithClassification(t, "echoOK", fmInfraError, output)
		return
	}
	if !strings.Contains(output, "curl") && !strings.Contains(output, "OMAC_ECHO_BASE") {
		mode := fmAgentNeverRan
		if agentProducedOutput(output) {
			mode = fmAgentRefused
		}
		failWithClassification(t, "echoOK", mode, output)
		return
	}
	if strings.Contains(output, "Connection refused") || strings.Contains(output, "curl: (7)") {
		failWithClassification(t, "echoOK", fmInfraError, output)
		return
	}
	failWithClassification(t, "echoOK", fmAgentPartial, output)
}

// assertWorkflowFileWritten verifies the agent's workdir write/read
// (echo > file && cat file) actually succeeded, by reading the file
// directly rather than trusting the agent's prose — the same rationale
// as assertEchoOK reading echo-status.txt. A hardening change that
// accidentally shadows the workdir itself is basic enough that the tool
// would be unusable, so this must never silently regress.
func assertWorkflowFileWritten(t *testing.T, path, stdout string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		mode := fmAgentNeverRan
		if agentProducedOutput(stdout) {
			mode = fmAgentRefused
		}
		failWithClassification(t, "workflowFileWritten", mode, stdout+"\n(workflow-check.txt: "+err.Error()+")")
		return
	}
	if !strings.Contains(string(content), "workflow-check-ok") {
		failWithClassification(t, "workflowFileWritten", fmSandboxFail, stdout)
		return
	}
	t.Logf("PASS: workdir write/read — workflow-check.txt round-tripped")
}

// assertGitCommitMade verifies the agent's basic git lifecycle (init,
// add, commit, log) actually succeeded, by reading the git-log.txt file
// the agent was told to write rather than trusting its prose. Checks
// for the commit subject specifically, not just non-empty content —
// git-log.txt can only exist at all if `git log` (the last command in
// the &&-chain) ran, but a bare non-empty check wouldn't tell apart a
// real "<hash> e2e smoke test" line from stray unrelated output.
func assertGitCommitMade(t *testing.T, path, stdout string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		mode := fmAgentNeverRan
		if agentProducedOutput(stdout) {
			mode = fmAgentRefused
		}
		failWithClassification(t, "gitCommitMade", mode, stdout+"\n(git-log.txt: "+err.Error()+")")
		return
	}
	if !strings.Contains(string(content), "e2e smoke test") {
		failWithClassification(t, "gitCommitMade", fmSandboxFail, stdout+"\n(git-log.txt: "+string(content)+")")
		return
	}
	t.Logf("PASS: git workflow — commit made, git-log.txt: %s", strings.TrimSpace(string(content)))
}

// assertSecretNotLeaked verifies the plaintext AUDIT_SECRET value does
// not appear anywhere in the agent's output. If it does, the sandbox
// leaked the secret into the agent's environment.
func assertSecretNotLeaked(t *testing.T, output string) {
	t.Helper()
	if strings.Contains(output, auditSecretValue) {
		failWithClassification(t, "secretNotLeaked", fmSandboxFail, output)
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
	if fingerprintRe.MatchString(output) {
		t.Logf("PASS: sidecar reachable — secret fingerprint found in agent output")
		return
	}
	// Classify: did the agent run the sidecar probe at all?
	mode := classifyProbe(output, "sidecar")
	switch mode {
	case fmAgentNeverRan, fmAgentRefused:
		failWithClassification(t, "sidecarReachable", mode, output)
	case fmAgentPartial:
		failWithClassification(t, "sidecarReachable", fmAgentPartial, output)
	case fmPass:
		// Probe ran but no fingerprint — check if sidecar was reachable at all.
		probeOut := extractProbe(output, "sidecar")
		if strings.Contains(probeOut, "Connection refused") || strings.Contains(probeOut, "curl: (7)") {
			failWithClassification(t, "sidecarReachable", fmInfraError, output)
		} else if strings.Contains(probeOut, "OMAC_AUDIT_BASE not set") {
			failWithClassification(t, "sidecarReachable", fmInfraError, output)
		} else {
			failWithClassification(t, "sidecarReachable", fmSandboxFail, output)
		}
	}
}

// assertEnvVarsDenied verifies that none of the denied env vars appear
// in the agent's env output. Each denied var is checked by looking for
// "VARNAME=" in the output.
func assertEnvVarsDenied(t *testing.T, output string, denyVars []string) {
	t.Helper()
	leaked := []string{}
	for _, v := range denyVars {
		needle := v + "="
		if strings.Contains(output, needle) {
			leaked = append(leaked, v)
		}
	}
	if len(leaked) > 0 {
		failWithClassification(t, "envVarsDenied", fmSandboxFail, output)
		return
	}
	t.Logf("PASS: env filtering — denied vars not in agent output")
}

// assertEnvVarsVisible verifies that the expected env vars ARE visible
// in the agent's output (positive assertion — sandbox passes them through).
func assertEnvVarsVisible(t *testing.T, output string, expectVars []string) {
	t.Helper()
	missing := []string{}
	for _, v := range expectVars {
		if !strings.Contains(output, v) {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		// Classify: did the agent run the env probe at all?
		mode := classifyProbe(output, "env")
		switch mode {
		case fmAgentNeverRan, fmAgentRefused:
			failWithClassification(t, "envVarsVisible", mode, output)
		case fmAgentPartial:
			failWithClassification(t, "envVarsVisible", fmAgentPartial, output)
		case fmPass:
			failWithClassification(t, "envVarsVisible", fmSandboxFail, output)
		}
		return
	}
	t.Logf("PASS: env passthrough — expected vars visible in agent output")
}

// assertFilesystemReadDenied verifies that filesystem read probes were
// denied by the sandbox. We check for OS-level denial messages in the
// fs_read probe section.
func assertFilesystemReadDenied(t *testing.T, output string) {
	t.Helper()
	mode := classifyProbe(output, "fs_read")
	switch mode {
	case fmAgentNeverRan, fmAgentRefused:
		failWithClassification(t, "fsReadDenied", mode, output)
		return
	case fmAgentPartial:
		failWithClassification(t, "fsReadDenied", fmAgentPartial, output)
		return
	}
	// Probe ran completely. probe_read (audit.sh) prints an explicit
	// "READABLE" marker only when a path was NOT blocked; denied paths
	// print the OS error instead and never contain that word. Failing
	// on its presence — rather than passing when any denial substring
	// appears anywhere in the section — means a single leaked path
	// among the ~14 probed here fails the assertion, instead of being
	// masked by other paths in the same section that were denied.
	if fsReadLeaked(output) {
		failWithClassification(t, "fsReadDenied", fmSandboxFail, output)
		return
	}
	t.Logf("PASS: filesystem read isolation — no probed path was readable")
}

// fsReadLeaked reports whether any path probed by audit.sh's fs_read
// section was readable. Pulled out of assertFilesystemReadDenied so the
// marker-absence decision is unit-testable against synthetic probe output
// without going through *testing.T (see security_assertions_test.go).
func fsReadLeaked(output string) bool {
	return strings.Contains(extractProbe(output, "fs_read"), "READABLE")
}

// assertFilesystemAllowed verifies that legitimate paths (workdir,
// cache dir, $TMPDIR) stayed accessible under the fs_allow probe. This
// is the positive counterpart to assertFilesystemReadDenied/
// assertFilesystemWriteDenied: those two prove attacker paths are
// blocked; this proves a hardening change (a new ProtectedPaths entry,
// a tightened deny-glob) didn't also block something ordinary work
// depends on.
func assertFilesystemAllowed(t *testing.T, output string, labels []string) {
	t.Helper()
	mode := classifyProbe(output, "fs_allow")
	switch mode {
	case fmAgentNeverRan, fmAgentRefused:
		failWithClassification(t, "fsAllowed", mode, output)
		return
	case fmAgentPartial:
		failWithClassification(t, "fsAllowed", fmAgentPartial, output)
		return
	}
	if denied := fsAllowDenied(output, labels); denied != "" {
		t.Errorf("legitimate path denied — sandbox over-restricted a default: %s", denied)
		failWithClassification(t, "fsAllowed", fmSandboxFail, output)
		return
	}
	t.Logf("PASS: filesystem allow — workdir/cache/tmp stayed accessible")
}

// fsAllowDenied checks each labelled fs_allow probe individually and
// returns the first line that did NOT show a WRITABLE/READABLE marker
// (i.e. was denied), or "" if all passed. Per-label, not "any marker
// anywhere in the section" — the same rigor applied to
// fsReadLeaked/fsWriteLeaked for the negative assertions, mirrored here
// for the positive case: if one legitimate path silently lost access,
// it must not be masked by the other paths still working.
func fsAllowDenied(output string, labels []string) string {
	section := extractProbe(output, "fs_allow")
	for _, label := range labels {
		idx := strings.Index(section, label)
		if idx < 0 {
			return label + ": probe label not found in fs_allow output"
		}
		line := section[idx:]
		if nl := strings.IndexByte(line, '\n'); nl >= 0 {
			line = line[:nl]
		}
		if !strings.Contains(line, "WRITABLE") && !strings.Contains(line, "READABLE") {
			return line
		}
	}
	return ""
}

// assertFilesystemWriteDenied verifies that write attempts to system
// paths (read-only mounts) were denied by the sandbox.
func assertFilesystemWriteDenied(t *testing.T, output string) {
	t.Helper()
	mode := classifyProbe(output, "fs_write")
	switch mode {
	case fmAgentNeverRan, fmAgentRefused:
		failWithClassification(t, "fsWriteDenied", mode, output)
		return
	case fmAgentPartial:
		failWithClassification(t, "fsWriteDenied", fmAgentPartial, output)
		return
	}
	// probe_write (audit.sh) prints an explicit "WRITABLE" marker only
	// on a successful write; a denied write is otherwise silent, so the
	// marker's absence — not the presence of some denial substring
	// among the 4 probed paths — is what proves none of them leaked.
	if fsWriteLeaked(output) {
		failWithClassification(t, "fsWriteDenied", fmSandboxFail, output)
		return
	}
	t.Logf("PASS: filesystem write protection — no probed path was writable")
}

// fsWriteLeaked reports whether any path probed by audit.sh's fs_write
// section was writable. See fsReadLeaked.
func fsWriteLeaked(output string) bool {
	return strings.Contains(extractProbe(output, "fs_write"), "WRITABLE")
}

// assertSymlinkEscapeDenied verifies that the agent could not read a denied
// path, nor write a denied path, through a symlink it planted inside the
// allowed (writable) workdir. A sandbox that only checks the literal path
// an agent opens — rather than the path a symlink resolves to — would let
// this through even though assertFilesystemReadDenied/WriteDenied (direct
// access, no indirection) pass.
func assertSymlinkEscapeDenied(t *testing.T, output string) {
	t.Helper()
	mode := classifyProbe(output, "symlink")
	switch mode {
	case fmAgentNeverRan, fmAgentRefused:
		failWithClassification(t, "symlinkEscapeDenied", mode, output)
		return
	case fmAgentPartial:
		failWithClassification(t, "symlinkEscapeDenied", fmAgentPartial, output)
		return
	}
	// Same marker-absence logic as assertFilesystemReadDenied /
	// assertFilesystemWriteDenied: probe_read/probe_write print
	// READABLE/WRITABLE only on a leak, so checking for their absence
	// catches either half of the escape (read or write) leaking
	// through the symlink indirection.
	readLeaked, writeLeaked := symlinkEscapeLeaked(output)
	if readLeaked || writeLeaked {
		failWithClassification(t, "symlinkEscapeDenied", fmSandboxFail, output)
		return
	}
	t.Logf("PASS: symlink escape denied — read and write through a workdir symlink to a denied path both blocked")
}

// symlinkEscapeLeaked reports whether the read half and/or write half of
// audit.sh's symlink escape probe leaked. See fsReadLeaked.
func symlinkEscapeLeaked(output string) (readLeaked, writeLeaked bool) {
	probeOut := extractProbe(output, "symlink")
	return strings.Contains(probeOut, "READABLE"), strings.Contains(probeOut, "WRITABLE")
}

// logHardlinkProbeResults logs whether a hardlink escape (same idea as the
// symlink probe, but via a hardlink) succeeded or failed, without
// asserting pass/fail. Hardlink creation requires the same filesystem as
// the target, so a failure here can mean "sandbox denied it" or "cross-
// device link" (EXDEV, unrelated to the sandbox) depending on where HOME
// and the workdir land — not a stable cross-platform contract.
func logHardlinkProbeResults(t *testing.T, output string) {
	t.Helper()
	if !strings.Contains(output, "=== PROBE: hardlink ===") {
		return
	}
	probeOut := extractProbe(output, "hardlink")
	switch {
	case strings.Contains(probeOut, "Invalid cross-device link"):
		t.Logf("INFO: hardlink escape probe — cross-device link (EXDEV), not a sandbox signal")
	case strings.Contains(probeOut, "Permission denied"),
		strings.Contains(probeOut, "Operation not permitted"),
		strings.Contains(probeOut, "No such file or directory"):
		t.Logf("INFO: hardlink escape DENIED")
	default:
		t.Logf("INFO: hardlink escape probe result inconclusive/allowed — see output:\n%s", probeOut)
	}
}

// logCrossSkillIsolation logs whether the agent could reach another
// skill's sidecar. omac currently does NOT isolate sidecars from each
// other — all skills share the same facade and can reach each other
// via their OMAC_<SKILL>_BASE env vars. This is a known design decision;
// we log the result so if isolation is added later, the change is visible.
func logCrossSkillIsolation(t *testing.T, output string) {
	t.Helper()
	if !strings.Contains(output, "=== PROBE: xskill ===") {
		t.Logf("SKIP: cross-skill isolation — xskill probe not in output")
		return
	}
	if strings.Contains(output, "OMAC_ECHO_BASE not set") {
		t.Logf("SKIP: cross-skill isolation — echo-rest not registered")
		return
	}
	// Check if the agent got a successful response from echo-rest.
	if strings.Contains(output, `"skill": "echo-rest"`) {
		t.Logf("INFO: cross-skill sidecar NOT isolated — agent reached echo-rest sidecar " +
			"(known behavior: all sidecars share the facade; not a security boundary)")
		return
	}
	t.Logf("INFO: cross-skill sidecar isolated — echo-rest sidecar not reachable from self-audit")
}

// logExecProbeResults logs the exec probe results without asserting
// pass/fail. Whether exec works on read-only mounts is a platform
// decision (bwrap typically allows exec on read-only binds), not a
// contract. We document the current behavior so changes are visible.
func logExecProbeResults(t *testing.T, output string, probePaths []string) {
	t.Helper()
	if !strings.Contains(output, "=== PROBE: fs_exec ===") {
		return
	}
	for _, p := range probePaths {
		if strings.Contains(output, "EXEC_OK") || strings.Contains(output, "SHELL_EXEC_OK") {
			t.Logf("INFO: exec on read-only mount ALLOWED for %s (platform default)", p)
		} else {
			t.Logf("INFO: exec on read-only mount DENIED for %s", p)
		}
	}
}

// assertNetworkDenied verifies that the network probe was blocked
// by the sandbox. We check for connection failure messages.
func assertNetworkDenied(t *testing.T, output string, denyDomain string) {
	t.Helper()
	mode := classifyProbe(output, "net")
	switch mode {
	case fmAgentNeverRan, fmAgentRefused:
		failWithClassification(t, "networkDenied", mode, output)
		return
	case fmAgentPartial:
		failWithClassification(t, "networkDenied", fmAgentPartial, output)
		return
	}
	probeOut := extractProbe(output, "net")
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
		if strings.Contains(probeOut, d) {
			found = true
			break
		}
	}
	if !found {
		failWithClassification(t, "networkDenied", fmSandboxFail, output)
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
