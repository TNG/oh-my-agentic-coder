//go:build e2e || e2e_fast

package e2e

import "strings"

// This file holds the pure decision predicates behind the security-audit
// assertions. They take probe output (a string) and return a bool/string —
// no *testing.T, no live agent/sandbox — so the marker-absence logic that
// backs each assertion is unit-testable in the fast (e2e_fast) build that
// runs at PR time. The *testing.T-based assert* wrappers that call these
// live in e2e_test.go behind the full e2e tag. See security_assertions_test.go.

// fsReadLeaked reports whether any path probed by audit.sh's fs_read
// section was readable. probe_read prints an explicit "READABLE" marker
// only when a path was NOT blocked; denied paths print the OS error
// instead and never contain that word. Checking the marker's presence
// per-section — rather than passing when any denial substring appears
// anywhere — means a single leaked path among the ~14 probed here trips
// the assertion instead of being masked by other paths that were denied.
func fsReadLeaked(output string) bool {
	return strings.Contains(extractProbe(output, "fs_read"), "READABLE")
}

// fsWriteLeaked reports whether any path probed by audit.sh's fs_write
// section was writable. A denied write is silent, so the "WRITABLE" marker's
// presence — not the absence of a denial substring — is the leak signal.
func fsWriteLeaked(output string) bool {
	return strings.Contains(extractProbe(output, "fs_write"), "WRITABLE")
}

// symlinkEscapeLeaked reports whether the read half and/or write half of
// audit.sh's symlink escape probe leaked. See fsReadLeaked.
func symlinkEscapeLeaked(output string) (readLeaked, writeLeaked bool) {
	probeOut := extractProbe(output, "symlink")
	return strings.Contains(probeOut, "READABLE"), strings.Contains(probeOut, "WRITABLE")
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

// secretLeaked reports whether the plaintext secret value appears anywhere
// in the audit output — i.e. the sandbox leaked it into the agent's view.
func secretLeaked(output, secret string) bool {
	return secret != "" && strings.Contains(output, secret)
}

// envVarsLeaked returns the subset of denyVars that appear (as "VAR=") in
// the audit output — vars the environment filter should have stripped.
func envVarsLeaked(output string, denyVars []string) []string {
	var leaked []string
	for _, v := range denyVars {
		if strings.Contains(output, v+"=") {
			leaked = append(leaked, v)
		}
	}
	return leaked
}

// netProbeDenied reports whether audit.sh's net section shows a denial —
// a proxy/DNS/connection failure that proves the request was blocked.
func netProbeDenied(output string) bool {
	probeOut := extractProbe(output, "net")
	for _, d := range netDenialMarkers {
		if strings.Contains(probeOut, d) {
			return true
		}
	}
	return false
}

// netDenialMarkers are the strings that indicate a network probe was blocked
// (by the omac proxy, by DNS failure, or by a refused/timed-out connection).
var netDenialMarkers = []string{
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

// exposureRecord documents one negative security property for a harness
// running with --no-sandbox: whether the audit probe ran to completion and
// whether the property is exposed (unenforced). It turns the previously
// silent "skip all negative assertions" path into an explicit, asserted,
// human-readable record of the real risk surface (issue #66).
type exposureRecord struct {
	Property string      // e.g. "filesystem read isolation"
	Probe    string      // audit.sh probe name, e.g. "fs_read"
	Mode     failureMode // fmPass means the probe ran and completed
	Exposed  bool        // true when the property is unenforced (only meaningful once Mode==fmPass)
}

// Ran reports whether the probe section was present and complete, so the
// exposure reading below it is trustworthy.
func (e exposureRecord) Ran() bool { return e.Mode == fmPass }

// noSandboxExposureReport classifies each negative security property for a
// --no-sandbox audit run: did its probe complete, and does the output show
// the property is exposed. Pure (no *testing.T) so the classification is
// unit-testable at PR time; the live wrapper asserts every probe ran and
// logs the resulting risk-surface table (see assertNoSandboxAuditReported).
func noSandboxExposureReport(output, secret string, denyVars []string) []exposureRecord {
	return []exposureRecord{
		{"secret isolation", "secret", classifyProbe(output, "secret"), secretLeaked(output, secret)},
		{"env filtering", "env", classifyProbe(output, "env"), len(envVarsLeaked(output, denyVars)) > 0},
		{"filesystem read isolation", "fs_read", classifyProbe(output, "fs_read"), fsReadLeaked(output)},
		{"filesystem write protection", "fs_write", classifyProbe(output, "fs_write"), fsWriteLeaked(output)},
		{"symlink escape", "symlink", classifyProbe(output, "symlink"), symlinkEscapeReadOrWrite(output)},
		{"network isolation", "net", classifyProbe(output, "net"), !netProbeDenied(output)},
	}
}

// symlinkEscapeReadOrWrite collapses the two-valued symlinkEscapeLeaked into
// a single "either half leaked" bool for the exposure report.
func symlinkEscapeReadOrWrite(output string) bool {
	r, w := symlinkEscapeLeaked(output)
	return r || w
}
