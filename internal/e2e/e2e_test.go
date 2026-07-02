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
	writeSandboxProfile(t, home, h)

	// 5. Copy echo-rest skill into workdir skills dir.
	copyEchoRest(t, h, workdir)

	// 6. Register echo-rest (no secrets, no fields — all optional).
	registerEchoRest(t, omacBin, home, workdir)

	// 7. Run agent: call the echo-rest /status endpoint.
	prompt := "Use the echo-rest skill to check its health. " +
		"Call curl on the OMAC_ECHO_BASE/status endpoint and report the response."
	stdout := runAgent(t, h, omacBin, home, workdir, prompt)

	// 8. Assert output contains the health-check ok flag.
	assertEchoOK(t, stdout)
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

// copyEchoRest copies the echo-rest skill from the repo's bundled copy
// into the workdir's harness-scoped skills directory.
func copyEchoRest(t *testing.T, h harnessConfig, workdir string) {
	t.Helper()
	// The echo-rest skill is bundled in the repo at .opencode/skills/echo-rest/.
	// The test binary runs from internal/e2e/, so ../../.opencode/skills/echo-rest.
	srcCandidates := []string{
		filepath.Join("..", "..", ".opencode", "skills", "echo-rest"),
		filepath.Join("..", "..", "..", ".opencode", "skills", "echo-rest"),
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
		t.Fatal("echo-rest skill not found in repo; the test requires .opencode/skills/echo-rest/")
	}
	dst := filepath.Join(workdir, h.SkillsBase, "skills", "echo-rest")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cp", "-r", src, dst)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("copy echo-rest: %v\n%s", err, out)
	}
	t.Logf("echo-rest copied to %s", dst)
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
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("agent did not exit within %v\nSTDOUT (last 200 lines):\n%s\nSTDERR (last 200 lines):\n%s",
			runTimeout, tailLines(stdout.String(), 200), tailLines(stderr.String(), 200))
	}
	if err != nil {
		// Dump sidecar logs if present (helps diagnose health timeouts).
		dumpSidecarLogs(t, workdir, home)
		t.Fatalf("omac start failed: %v\nSTDOUT:\n%s\nSTDERR:\n%s",
			err, stdout.String(), stderr.String())
	}
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
// Base profile (applies to all harnesses):
//
//   workdir        — readwrite
//   network        — filtered, listen_port 4097 (echo-rest), allow_tcp_connect 22 (SSH)
//   filesystem.allow — ~/.cache, ~/.local/share, ~/.local/state, ~/.bun,
//                       ~/Library/Caches, ~/go, ~/.rustup, ~/.cargo
//   filesystem.read  — ~/.gitconfig, ~/.gitignore_global, ~/.config
//
// Per-harness deviations (h.Sandbox):
//
//   ExtraAllowDomains — additional domains beyond the model provider host
//   ExtraReadPaths    — additional filesystem read paths
//
// The model provider host (from SKAINET_INTERNAL / ANTHROPIC_BASE_URL) is
// always allowed — it is derived at runtime so the sandbox proxy doesn't
// deny the agent's API calls.
//
// AUDIT NOTES — base filesystem.allow paths:
//
//   ~/.cache         — opencode writes cache here at runtime
//   ~/.local/share   — opencode auth.json + XDG data for all harnesses
//   ~/.local/state   — opencode locks
//   ~/.bun           — bun global bin (opencode installed via bun)
//   ~/Library/Caches — macOS cache dir (harnesses may write here on macOS)
//   ~/go             — Go toolchain (none of the harnesses are Go-based;
//                      kept for omac binary build cache, safe to remove if
//                      build moves outside HOME)
//   ~/.rustup        — Rust toolchain (only codex is a Rust binary;
//                      codex reads its toolchain at startup)
//   ~/.cargo         — Rust cargo (same as above)
//
// AUDIT NOTES — base filesystem.read paths:
//
//   ~/.gitconfig       — harnesses may read git config for repo context
//   ~/.gitignore_global — same
//   ~/.config          — XDG config base (all harnesses read from here)
//
// AUDIT NOTES — base network:
//
//   listen_port 4097    — echo-rest skill listens here
//   allow_tcp_connect 22 — SSH; unclear if any harness uses it, possibly
//                          needed for git over SSH in workdir. Candidate
//                          for removal if no harness requires it.
func writeSandboxProfile(t *testing.T, home string, h harnessConfig) {
	t.Helper()
	// Model provider host — always allowed. Derived from SKAINET_INTERNAL
	// (codex, copilot, opencode) and ANTHROPIC_BASE_URL (claude-code).
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
	profDir := filepath.Join(home, ".config", "omac", "sandbox-profiles")
	if err := os.MkdirAll(profDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.MarshalIndent(profile, "", "  ")
	if err := os.WriteFile(filepath.Join(profDir, "default.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("sandbox profile written with %d allow_domain entries", len(allowDomains))
}

// extractHost parses a URL string and returns the hostname.
func extractHost(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
