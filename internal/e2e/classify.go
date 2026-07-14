//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
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
	fmAgentNeverRan failureMode = "AGENT_NEVER_RAN" // agent produced no output — likely infra crash / API error / never started
	fmAgentRefused  failureMode = "AGENT_REFUSED"   // agent ran (produced output) but didn't run the probe — refused or went off-script
	fmAgentPartial  failureMode = "AGENT_PARTIAL"   // agent ran it but output is incomplete/summarized
	fmSandboxFail   failureMode = "SANDBOX_FAIL"    // agent ran it, probe output present, security violated
	fmInfraError    failureMode = "INFRA_ERROR"     // omac/sidecar crashed or returned error
)

// agentProducedOutput reports whether the agent produced any non-whitespace
// output. Assertions only run after a clean agent exit (infra crashes hit
// t.Fatalf first), so empty output here means the agent started, exited 0,
// and printed nothing — a strong "did not do its job at all" signal.
func agentProducedOutput(output string) bool {
	return strings.TrimSpace(output) != ""
}

// classifyProbe checks whether a named probe's output is present
// and complete in the agent's combined stdout+stderr.
//
// Returns:
//   - fmAgentNeverRan if the "=== PROBE: <name> ===" marker is absent
//     AND the agent produced no output (likely infra: API error, crash,
//     or the agent never started despite exit 0).
//   - fmAgentRefused if the marker is absent but the agent DID produce
//     output — it ran but refused, summarized, or went off-script and
//     never executed the probe.
//   - fmAgentPartial if the marker is present but "=== END: <name> ==="
//     is absent.
//   - fmPass if both markers are present (the caller still needs to
//     check the security property within the probe section).
func classifyProbe(output, probeName string) failureMode {
	startMarker := "=== PROBE: " + probeName + " ==="
	endMarker := "=== END: " + probeName + " ==="
	if !strings.Contains(output, startMarker) {
		if agentProducedOutput(output) {
			return fmAgentRefused
		}
		return fmAgentNeverRan
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
	probes := []string{"secret", "env", "fs_read", "fs_write", "fs_exec", "symlink", "hardlink", "net", "sidecar", "xskill"}
	var present, complete, refused, neverRan []string
	for _, p := range probes {
		switch classifyProbe(output, p) {
		case fmPass:
			present = append(present, p)
			complete = append(complete, p)
		case fmAgentPartial:
			present = append(present, p)
		case fmAgentRefused:
			refused = append(refused, p)
		case fmAgentNeverRan:
			neverRan = append(neverRan, p)
		}
	}
	var b strings.Builder
	b.WriteString("agent output classification:\n")
	b.WriteString("  agent produced output: " + fmt.Sprintf("%v", agentProducedOutput(output)) + "\n")
	if sidecarCalled := sidecarSawRequests(); sidecarCalled {
		b.WriteString("  sidecar: received HTTP requests (agent DID run and call the facade)\n")
	} else {
		b.WriteString("  sidecar: no requests recorded (agent may not have started)\n")
	}
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
	if len(refused) > 0 {
		b.WriteString("  probes refused/off-script: " + strings.Join(refused, ", ") + "\n")
	}
	if len(neverRan) > 0 {
		b.WriteString("  probes never ran (empty output): " + strings.Join(neverRan, ", ") + "\n")
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

// sidecarSawRequests reads the omac sidecar log files and reports whether
// any HTTP request line (e.g. `"GET /status HTTP/1.1" 200`) appears. This is
// the ground-truth signal that the agent started, reached the facade, and
// made at least one call — distinguishing "agent never ran / infra crash"
// from "agent ran but refused/summarized" when probe markers are absent.
func sidecarSawRequests() bool {
	pattern := filepath.Join(os.TempDir(), "omac-*", "logs", "*.log")
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), "HTTP/1.1") {
			return true
		}
	}
	return false
}

// ghaAnnotation emits a GitHub Actions workflow command so the failure
// shows up as a red indicator on the run summary (no log digging needed).
// Safe no-op outside GHA (no $GITHUB_ACTIONS env var). level: error|warning|notice.
//
// The annotation is a one-liner (assertion + mode + short reason); the full
// classification block stays in the test log via t.Errorf so it isn't duplicated.
func ghaAnnotation(t *testing.T, level string, assertName string, mode failureMode, output string) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		return
	}
	short := shortReason(mode)
	// Workflow commands: newlines must be %-encoded as %0A.
	fmt.Printf("::%s file=internal/e2e/e2e_test.go,title=%s [%s]::%s — %s\n",
		level, assertName, mode, assertName, short)
}

// shortReason returns a single-line human description for a failure mode.
func shortReason(mode failureMode) string {
	switch mode {
	case fmAgentNeverRan:
		return "agent produced no output (likely infra: API error, crash, or never started)"
	case fmAgentRefused:
		return "agent ran but did not run the probe (refused or off-script)"
	case fmAgentPartial:
		return "agent ran probe but output is incomplete (summarized?)"
	case fmSandboxFail:
		return "sandbox did not enforce security property"
	case fmInfraError:
		return "infrastructure error"
	default:
		return string(mode)
	}
}

// failWithClassification records a test failure with the failure mode and
// emits a GHA annotation so the run summary shows the cause by indicator.
// Use instead of t.Errorf in assertion helpers.
func failWithClassification(t *testing.T, assertName string, mode failureMode, output string) {
	t.Helper()
	msg := failureMessage(assertName, mode, output)
	t.Errorf("%s", msg)
	ghaAnnotation(t, "error", assertName, mode, output)
	localBanner(assertName, mode)
}

// localBanner prints a colored one-line failure banner to stdout so a
// local `go test -v` run shows the assertion name + failure mode at a
// glance, mirroring the GHA annotation. No-op in CI (GHA parses workflow
// commands from stdout; a colored banner would clutter the log).
func localBanner(assertName string, mode failureMode) {
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		return
	}
	const red = "\033[31m"
	const bold = "\033[1m"
	const reset = "\033[0m"
	fmt.Printf("%s%s FAIL [%s] %s%s\n", red, bold, assertName, mode, reset)
}

// failureMessage renders the assertion-failure text for a given mode.
func failureMessage(assertName string, mode failureMode, output string) string {
	classification := classifyAgentOutput(output)
	switch mode {
	case fmAgentNeverRan:
		return fmt.Sprintf("FAIL [%s]: %s — agent produced no output; likely infra (API error, crash, or never started)\n%s",
			assertName, mode, classification)
	case fmAgentRefused:
		return fmt.Sprintf("FAIL [%s]: %s — agent ran but did not run the probe (refused or off-script)\n%s",
			assertName, mode, classification)
	case fmAgentPartial:
		return fmt.Sprintf("FAIL [%s]: %s — agent ran probe but output is incomplete (summarized?)\n%s",
			assertName, mode, classification)
	case fmSandboxFail:
		return fmt.Sprintf("FAIL [%s]: %s — sandbox did not enforce security property\n%s",
			assertName, mode, classification)
	case fmInfraError:
		return fmt.Sprintf("FAIL [%s]: %s — infrastructure error\n%s",
			assertName, mode, classification)
	default:
		return fmt.Sprintf("FAIL [%s]: %s\n%s", assertName, mode, classification)
	}
}
