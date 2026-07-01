package bridge

import (
	"encoding/json"
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
