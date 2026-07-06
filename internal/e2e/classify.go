//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// failureMode classifies why an assertion failed, so CI output
// distinguishes "the agent didn't do the thing" from "the sandbox
// is broken" from "the infrastructure crashed".
//
// The classification is based on probe markers in the output:
//   - audit.sh emits "=== PROBE: <name> ===" ... "=== END: <name> ==="
//   - If markers are absent, the agent didn't run the script.
//   - If markers are present but the expected content is missing,
//     the agent ran it but didn't print verbatim (summarized).
//   - If the expected content is present but the security property
//     is violated, the sandbox is broken.
type failureMode string

const (
	fmPass          failureMode = "PASS"
	fmAgentNoOutput failureMode = "AGENT_NO_OUTPUT"  // agent didn't run the probe at all
	fmAgentPartial  failureMode = "AGENT_PARTIAL"   // agent ran it but output is incomplete/summarized
	fmSandboxFail   failureMode = "SANDBOX_FAIL"    // agent ran it, probe output present, security violated
	fmInfraError    failureMode = "INFRA_ERROR"      // omac/sidecar crashed or returned error
)

// classifyProbe checks whether a named probe's output is present
// and complete in the agent's combined stdout+stderr.
//
// Returns:
//   - fmAgentNoOutput if the "=== PROBE: <name> ===" marker is absent
//   - fmAgentPartial if the marker is present but "=== END: <name> ===" is absent
//   - fmPass if both markers are present (the caller still needs to
//     check the security property within the probe section)
func classifyProbe(output, probeName string) failureMode {
	startMarker := "=== PROBE: " + probeName + " ==="
	endMarker := "=== END: " + probeName + " ==="
	if !strings.Contains(output, startMarker) {
		return fmAgentNoOutput
	}
	if !strings.Contains(output, endMarker) {
		return fmAgentPartial
	}
	return fmPass
}

// extractProbe returns the content between the start and end markers
// of a named probe, or empty string if markers are absent.
func extractProbe(output, probeName string) string {
	startMarker := "=== PROBE: " + probeName + " ==="
	endMarker := "=== END: " + probeName + " ==="
	start := strings.Index(output, startMarker)
	if start < 0 {
		return ""
	}
	start += len(startMarker)
	end := strings.Index(output[start:], endMarker)
	if end < 0 {
		return output[start:]
	}
	return output[start : start+end]
}

// classifyAgentOutput inspects the full agent output and returns a
// human-readable summary of what the agent did. Called on assertion
// failure so CI output explains the failure mode.
func classifyAgentOutput(output string) string {
	probes := []string{"secret", "env", "fs_read", "fs_write", "fs_exec", "net", "sidecar", "xskill"}
	var present, complete, absent []string
	for _, p := range probes {
		switch classifyProbe(output, p) {
		case fmPass:
			present = append(present, p)
			complete = append(complete, p)
		case fmAgentPartial:
			present = append(present, p)
		case fmAgentNoOutput:
			absent = append(absent, p)
		}
	}
	var b strings.Builder
	b.WriteString("agent output classification:\n")
	b.WriteString("  probes complete: " + strings.Join(complete, ", ") + "\n")
	if len(present) > len(complete) {
		b.WriteString("  probes partial: ")
		first := true
		for _, p := range present {
			if !contains(complete, p) {
				if !first {
					b.WriteString(", ")
				}
				b.WriteString(p)
				first = false
			}
		}
		b.WriteString("\n")
	}
	if len(absent) > 0 {
		b.WriteString("  probes absent: " + strings.Join(absent, ", ") + "\n")
	}
	// Check for infra errors.
	if strings.Contains(output, "omac start failed") ||
		strings.Contains(output, "sidecar") && strings.Contains(output, "error") {
		b.WriteString("  infra: sidecar/omac errors detected\n")
	}
	if strings.Contains(output, "stream error") || strings.Contains(output, "AI_APICallError") {
		b.WriteString("  infra: model API error detected\n")
	}
	if strings.Contains(output, "agent did not exit within") {
		b.WriteString("  infra: agent timed out\n")
	}
	return b.String()
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// assertWithClassification wraps an assertion with failure-mode classification.
// If the assertion fails, it calls t.Errorf with the failure reason plus
// the classified agent output, so CI shows whether it's an agent issue,
// a sandbox issue, or an infra issue.
func assertWithClassification(t *testing.T, output string, assertName string, check func() failureMode) {
	t.Helper()
	mode := check()
	switch mode {
	case fmPass:
		t.Logf("PASS: %s", assertName)
	case fmAgentNoOutput:
		t.Errorf("FAIL [%s]: %s — agent did not run the probe\n%s",
			assertName, mode, classifyAgentOutput(output))
	case fmAgentPartial:
		t.Errorf("FAIL [%s]: %s — agent ran probe but output is incomplete (summarized?)\n%s",
			assertName, mode, classifyAgentOutput(output))
	case fmSandboxFail:
		t.Errorf("FAIL [%s]: %s — sandbox did not enforce security property\n%s",
			assertName, mode, classifyAgentOutput(output))
	case fmInfraError:
		t.Errorf("FAIL [%s]: %s — infrastructure error\n%s",
			assertName, mode, classifyAgentOutput(output))
	}
}
