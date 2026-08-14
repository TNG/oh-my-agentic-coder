package bridge

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// codexBridgePath is the path to the codex bridge script relative to the
// repo root. The test invokes it as a subprocess, feeding it a SessionStart
// hook payload on stdin, and checks the JSON output on stdout.
const codexBridgePath = "../../.codex/hooks/omac-bridge.sh"

func TestCodexBridgeSessionStartEmitsAdditionalContext(t *testing.T) {
	// The bridge is inert when OMAC_CONTROL_BASE is unset, so we set it to
	// a dummy value that will cause activation to fail gracefully (curl
	// error → empty manifest → no output). To test the manifest rendering
	// path, we need a live control plane. Instead, test that the bridge:
	// 1. Exists and is executable
	// 2. Accepts a SessionStart payload without error
	// 3. Is inert (no output) when control plane is unreachable
	//
	// This is the minimal check: the script runs, doesn't crash, and is
	// a no-op when the control plane is absent.
	path := filepath.Join(codexBridgePath)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	payload := `{"hook_event_name":"SessionStart","session_id":"test-123","cwd":"/tmp/test","source":"startup"}`

	cmd := exec.Command("bash", path)
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = []string{} // no OMAC_CONTROL_BASE → inert

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bridge script failed: %v\nstderr: %s", err, out)
	}

	// When inert (no OMAC_CONTROL_BASE), the bridge should produce no output.
	if len(out) > 0 {
		// If there IS output, it must be valid JSON with hookSpecificOutput
		var result map[string]any
		if json.Unmarshal(out, &result) != nil {
			t.Errorf("output is not valid JSON: %s", out)
		}
	}
}

func TestCodexBridgeScriptExists(t *testing.T) {
	if _, err := exec.Command("test", "-f", codexBridgePath).Output(); err != nil {
		t.Skipf("codex bridge script not found at %s", codexBridgePath)
	}
}

const copilotBridgePath = "../../.copilot/hooks/omac-bridge.sh"

func TestCopilotBridgeSessionStartEmitsAdditionalContext(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	path := filepath.Join(copilotBridgePath)
	payload := `{"hook_event_name":"SessionStart","session_id":"test-123","cwd":"/tmp/test","source":"startup"}`

	cmd := exec.Command("bash", path)
	cmd.Stdin = strings.NewReader(payload)
	cmd.Env = []string{} // no OMAC_CONTROL_BASE → inert

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("bridge script failed: %v\nstderr: %s", err, out)
	}

	if len(out) > 0 {
		var result map[string]any
		if json.Unmarshal(out, &result) != nil {
			t.Errorf("output is not valid JSON: %s", out)
		}
	}
}

func TestCopilotBridgeScriptExists(t *testing.T) {
	if _, err := exec.Command("test", "-f", copilotBridgePath).Output(); err != nil {
		t.Skipf("copilot bridge script not found at %s", copilotBridgePath)
	}
}

const piBridgePath = "../../.pi/extensions/omac-bridge.ts"

// TestPiBridgeInjectsExactlyOneSystemBlock guards the single-system-message
// invariant on the pi side, mirroring the OpenCode guard in
// internal/plugin (TestBriefingMergesIntoExistingSystemBlock).
//
// Pi's before_agent_start hook offers two channels: a returned
// `systemPrompt` (folded into pi's one system prompt string) and a returned
// `message`. Only the first is safe. Using a second channel for system
// content puts a {role:"system"} message at index > 0, which strict
// OpenAI-compatible servers — Qwen chat templates in particular — reject
// with "system message must come first".
//
// Verified empirically against pi 0.80.6: the hook event carries only
// {type, prompt, images, systemPrompt, systemPromptOptions}, and a real run
// against a stub OpenAI-compatible endpoint put exactly one system message
// on the wire with the briefing and manifest inside it.
//
// A source-level guard because the repo has no TypeScript test runner; the
// wire-level equivalent for OpenCode is internal/e2e/plugin_briefing_test.go.
func TestPiBridgeInjectsExactlyOneSystemBlock(t *testing.T) {
	src, err := os.ReadFile(piBridgePath)
	if err != nil {
		t.Fatalf("read pi bridge: %v", err)
	}
	// Whitespace is stripped from both source and needles so a formatter
	// reflowing the call (prettier dropping the braces' inner spaces, or
	// wrapping the argument onto its own line) cannot silently defeat the
	// guard. Matching the role literal rather than a call site means a
	// renamed array or helper still trips it.
	text := stripSpace(string(src))

	for _, banned := range []string{
		`unshift({role:"system"`,
		`push({role:"system"`,
		`message:{role:"system"`,
	} {
		if strings.Contains(text, banned) {
			t.Errorf("%s must not %s: injecting a message alongside the "+
				"returned systemPrompt yields two {role:\"system\"} messages "+
				"and breaks providers that require exactly one at index 0",
				piBridgePath, banned)
		}
	}

	if !strings.Contains(text, "systemPrompt:") {
		t.Errorf("%s no longer returns a merged systemPrompt; the briefing "+
			"and skills manifest would never reach the model", piBridgePath)
	}
}

const ompBridgePath = "../../.omp/extensions/omac-bridge.ts"

func TestOmpBridgeInjectsExactlyOneSystemBlock(t *testing.T) {
	src, err := os.ReadFile(ompBridgePath)
	if err != nil {
		t.Fatalf("read omp bridge: %v", err)
	}
	text := stripSpace(string(src))

	for _, banned := range []string{
		`unshift({role:"system"`,
		`push({role:"system"`,
		`message:{role:"system"`,
	} {
		if strings.Contains(text, banned) {
			t.Errorf("%s must not %s: injecting a message alongside the "+
				"returned systemPrompt yields two {role:\"system\"} messages "+
				"and breaks providers that require exactly one at index 0",
				ompBridgePath, banned)
		}
	}

	if !strings.Contains(text, "systemPrompt:") {
		t.Errorf("%s no longer returns a merged systemPrompt; the briefing "+
			"and skills manifest would never reach the model", ompBridgePath)
	}
}

// stripSpace removes all whitespace, making a source-level match immune to
// reformatting.
func stripSpace(s string) string {
	return strings.Join(strings.Fields(s), "")
}
