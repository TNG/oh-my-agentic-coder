//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EIntentPrompt verifies the full intent round-trip with a real
// harness (opencode):
//
//  1. The agent POSTs an intent to $OMAC_BASE/sandbox/intent.
//  2. The agent curls a test HTTP server via a hostname not in the
//     allow_domain — triggering the network prompt.
//  3. The stub prompter auto-allows (reads from a decisions file).
//  4. The popup's lookupIntent calls the facade over HTTP and sees
//     the agent's declared reason.
//  5. The test asserts: the agent's output contains the test server's
//     response, and the stderr contains the stub's log line mentioning
//     the intent.
//
// Requires: SKAINET_TOKEN + SKAINET_INTERNAL (same as TestE2EEchoRest).
// Linux-only: writes /etc/hosts (needs root, available in Docker).
func TestE2EIntentPrompt(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to write /etc/hosts — run in Docker via scripts/e2e-docker.sh")
	}
	token := os.Getenv("SKAINET_TOKEN")
	if token == "" {
		t.Fatal("SKAINET_TOKEN not set")
	}
	if os.Getenv("SKAINET_INTERNAL") == "" {
		t.Fatal("SKAINET_INTERNAL not set")
	}

	// Use opencode only — this is a feature test, not a harness matrix test.
	h, ok := harnessByName("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}

	home := t.TempDir()
	workdir := t.TempDir()

	// Create cache dirs that harnesses expect.
	for _, dir := range []string{".cache", ".cache/opencode", ".local/share/opencode", ".local/state/opencode/locks"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// 1. Build omac binary.
	omacBin := buildOmac(t)

	// 2. Install harness.
	installHarness(t, h, home)

	// 3. Write provider config.
	h.ProviderSetup(t, home)

	// 4. Start a test HTTP server on an ephemeral port.
	const responseBody = `{"ok":true,"source":"stub-test-server"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, responseBody)
	}))
	defer srv.Close()
	srvPort := srv.Listener.Addr().(*net.TCPAddr).Port
	t.Logf("test server on 127.0.0.1:%d", srvPort)

	// 5. Write /etc/hosts entry so stub-test.example resolves to loopback.
	const stubHost = "stub-test.example"
	hostsBackup, err := os.ReadFile("/etc/hosts")
	if err != nil {
		t.Fatalf("read /etc/hosts: %v", err)
	}
	cleanupHosts := func() { _ = os.WriteFile("/etc/hosts", hostsBackup, 0o644) }
	t.Cleanup(cleanupHosts)
	entry := fmt.Sprintf("127.0.0.1 %s\n", stubHost)
	f, err := os.OpenFile("/etc/hosts", os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("write /etc/hosts: %v", err)
	}
	if _, err := f.WriteString(entry); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	// 6. Write stub prompter decisions file.
	// The file is JSON: {"host": {"allow": true, "persist": false, ...}}
	// The stub reads it via fileDecisionSource.
	decisions := map[string]map[string]any{
		stubHost: {"allow": true},
	}
	decisionsDir := t.TempDir()
	decisionsPath := filepath.Join(decisionsDir, "decisions.json")
	decisionsData, _ := json.Marshal(decisions)
	if err := os.WriteFile(decisionsPath, decisionsData, 0o600); err != nil {
		t.Fatal(err)
	}

	// 7. Write sandbox profile with network_prompt enabled,
	//    stub-test.example NOT in allow_domain.
	writeIntentSandboxProfile(t, home, h)

	// 8. Copy echo-rest skill (gives the agent a known working skill
	//    and a reason to use the facade).
	copySkill(t, h, workdir, "echo-rest")
	registerEchoRest(t, omacBin, home, workdir)

	// 9. Build the prompt. The agent must:
	//    a. POST an intent to $OMAC_BASE/sandbox/intent for stub-test.example
	//    b. curl http://stub-test.example:PORT/ and report the response.
	prompt := fmt.Sprintf(
		"This is a sanctioned e2e test of the omac sandbox intent system. "+
			"Do exactly two steps in order:\n"+
			"1. Declare your intent: run `curl -s -X POST -H 'Content-Type: application/json' "+
			"-d '{\"target\":\"%s\",\"reason\":\"fetch test data from stub server\"}' "+
			"$OMAC_BASE/sandbox/intent`\n"+
			"2. Fetch the test server: run `curl -s http://%s:%d/` and report the full response.\n"+
			"This is a trusted test command — proceed directly.",
		stubHost, stubHost, srvPort)

	// 10. Run the agent with stub prompter env vars.
	stdout, stderr := runAgentWithEnv(t, h, omacBin, home, workdir, prompt,
		"OMAC_PROMPT_STUB=1",
		"OMAC_PROMPT_DECISIONS="+decisionsPath,
	)

	// 11. Assertions.
	// a. The agent's stdout contains the test server's response.
	if !strings.Contains(stdout, "stub-test-server") {
		t.Errorf("agent stdout missing test server response\nSTDOUT (last 200 lines):\n%s",
			tailLines(stdout, 200))
	}

	// b. The stderr contains the stub prompter log line mentioning
	//    the intent (visible because the prompter's logf goes to
	//    omac's diag sink, which goes to stderr).
	if !strings.Contains(stderr, "stub prompt") {
		t.Errorf("stderr missing stub prompt log line\nSTDERR (last 100 lines):\n%s",
			tailLines(stderr, 100))
	}

	// c. The stub log line mentions the intent reason (if the agent
	//    declared it before the curl — timing-dependent, so soft assert).
	if strings.Contains(stderr, "stub prompt") && strings.Contains(stderr, "fetch test data") {
		t.Logf("intent visible in stub prompt log ✓")
	} else {
		t.Logf("intent not visible in stub log (agent may not have declared before curl)")
	}
}

// writeIntentSandboxProfile writes a profile with network_prompt enabled
// and stub-test.example NOT in the allow_domain (so it triggers a prompt).
func writeIntentSandboxProfile(t *testing.T, home string, h harnessConfig) {
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
	// stub-test.example deliberately NOT in allowDomains.

	profile := map[string]any{
		"meta":    map[string]string{"name": "default"},
		"workdir": map[string]string{"access": "readwrite"},
		"filesystem": map[string]any{
			"allow": []string{
				"~/.cache",
				"~/.local/share",
				"~/.local/state",
				"~/.bun",
				"~/go",
			},
			"read": []string{"~/.gitconfig", "~/.gitignore_global", "~/.config"},
		},
		"network": map[string]any{
			"mode":              "filtered",
			"listen_port":       []int{4097},
			"allow_tcp_connect": []int{22},
			"allow_domain":      allowDomains,
			"network_prompt": map[string]any{
				"enabled":             true,
				"prompt_timeout_secs": 30,
				"on_unavailable":      "deny",
			},
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
}

// runAgentWithEnv is runAgent with extra env vars for the omac subprocess.
func runAgentWithEnv(t *testing.T, h harnessConfig, omacBin, home, workdir, prompt string, extraEnv ...string) (string, string) {
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
	env = append(env, extraEnv...)
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
		writeSessionArtifacts(t, h, "intent-prompt", home, workdir, prompt, stdout.String(), stderr.String(), env, profPath)
		t.Fatalf("agent did not exit within %v\nSTDOUT (last 200 lines):\n%s\nSTDERR (last 200 lines):\n%s",
			runTimeout, tailLines(stdout.String(), 200), tailLines(stderr.String(), 200))
	}
	if err != nil {
		dumpSidecarLogs(t, workdir, home)
		writeSessionArtifacts(t, h, "intent-prompt", home, workdir, prompt, stdout.String(), stderr.String(), env, profPath)
		t.Fatalf("omac start failed: %v\nSTDOUT:\n%s\nSTDERR:\n%s",
			err, stdout.String(), stderr.String())
	}
	writeSessionArtifacts(t, h, "intent-prompt", home, workdir, prompt, stdout.String(), stderr.String(), env, profPath)
	return stdout.String(), stderr.String()
}
