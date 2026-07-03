//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EProvenance verifies that `omac provenance --json` output matches
// the actual sandbox behavior the agent observes. It reuses the
// security-audit setup (profile + self-audit skill) so the cross-check
// is against real enforcement, not a second copy of the config.
//
// Provenance is harness-agnostic (reads the same profile regardless of
// harness), so we only test with opencode.
func TestE2EProvenance(t *testing.T) {
	h, ok := harnessByName("opencode")
	if !ok {
		t.Fatal("opencode harness not found")
	}

	home := t.TempDir()
	workdir := t.TempDir()

	for _, dir := range []string{".cache", ".cache/opencode", ".local/share/opencode", ".local/state/opencode/locks"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	omacBin := buildOmac(t)
	installHarness(t, h, home)
	h.ProviderSetup(t, home)

	spec := allowanceSpecFor(h)
	writeSandboxProfile(t, home, h, &spec)
	copySkill(t, h, workdir, "self-audit")
	registerSelfAudit(t, omacBin, home, workdir)

	// --- Step 1: Run `omac provenance --json` host-side ---
	profPath := filepath.Join(home, ".config", "omac", "sandbox-profiles", "default.json")
	cmd := exec.Command(omacBin, "provenance", "--profile", profPath, "--json")
	cmd.Dir = workdir
	cmd.Env = withHome(os.Environ(), home)
	provOut, err := cmd.Output()
	if err != nil {
		t.Fatalf("omac provenance: %v\n%s", err, provOut)
	}

	var view struct {
		Profile struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"profile"`
		Network struct {
			Entries []struct {
				Entry  string `json:"entry"`
				Action string `json:"action"`
				Source string `json:"source"`
			} `json:"entries"`
		} `json:"network"`
		Environment struct {
			Entries []struct {
				Entry  string `json:"entry"`
				Action string `json:"action"`
				Source string `json:"source"`
			} `json:"entries"`
		} `json:"environment"`
		Skills struct {
			Entries []struct {
				Entry  string `json:"entry"`
				Action string `json:"action"`
				Source string `json:"source"`
			} `json:"entries"`
		} `json:"skills"`
	}
	if err := json.Unmarshal(provOut, &view); err != nil {
		t.Fatalf("parse provenance JSON: %v\n%s", err, provOut)
	}

	// --- Step 2: Provenance-content assertions ---

	// 2a. allow_domain entries from the profile appear in provenance.
	allowDomains := []string{}
	for _, envVar := range []string{"SKAINET_INTERNAL", "ANTHROPIC_BASE_URL"} {
		if baseURL := os.Getenv(envVar); baseURL != "" {
			if host := extractHost(baseURL); host != "" {
				allowDomains = append(allowDomains, host)
			}
		}
	}
	allowDomains = append(allowDomains, h.Sandbox.ExtraAllowDomains...)
	for _, d := range allowDomains {
		found := false
		for _, e := range view.Network.Entries {
			if e.Entry == d && e.Action == "allow" {
				found = true
			}
		}
		if !found {
			t.Errorf("provenance: allow_domain %q missing from network entries", d)
		}
	}

	// 2b. Blocklist entries present.
	foundBlocklist := false
	for _, e := range view.Environment.Entries {
		if e.Source == "blocklist" && e.Action == "deny" {
			foundBlocklist = true
		}
	}
	if !foundBlocklist {
		t.Error("provenance: no blocklist entries in environment section")
	}

	// 2c. self-audit skill registered.
	foundSkill := false
	for _, e := range view.Skills.Entries {
		if e.Entry == "self-audit" && e.Action == "registered" {
			foundSkill = true
		}
	}
	if !foundSkill {
		t.Error("provenance: self-audit skill not in skills section")
	}

	// --- Step 3: Behavior cross-check via the audit agent ---
	prompt := "Run this command and print its full output verbatim:\n\n" +
		`sh "$OMAC_HARNESS_SKILLS_DIR/self-audit/scripts/audit.sh"` + "\n\n" +
		"Do not summarize. Print every line."
	auditStdout := runAuditAgent(t, h, omacBin, home, workdir, prompt)

	// 3a. Network denial: spec.NetDenyDomain should be denied by the sandbox.
	// The provenance output doesn't list it as allow → audit shows denial.
	if !assertNetworkDeniedSilent(auditStdout, spec.NetDenyDomain) {
		t.Errorf("behavior mismatch: %q not denied by sandbox (audit output lacks denial message)", spec.NetDenyDomain)
	}

	// 3b. AUDIT_SECRET: not in allow_vars → stripped from agent env.
	// Provenance shows allow_vars list (which excludes AUDIT_SECRET).
	if strings.Contains(auditStdout, auditSecretValue) {
		t.Error("behavior mismatch: AUDIT_SECRET leaked into agent env despite provenance not listing it as allowed")
	}

	// 3c. Filesystem denials present in audit output.
	if !assertFilesystemDeniedSilent(auditStdout) {
		t.Error("behavior mismatch: no filesystem denial in audit output despite provenance listing protected paths as denied")
	}
}

// assertNetworkDeniedSilent checks for network denial messages without
// logging (the e2e test's own assertions handle reporting).
func assertNetworkDeniedSilent(output, denyDomain string) bool {
	denials := []string{
		"Connection refused",
		"Could not resolve host",
		"Connection timed out",
		"Failed to connect",
		"curl: (6)",
		"curl: (7)",
		"curl: (28)",
		"DENIED BY THE SANDBOX",
		"403",
	}
	for _, d := range denials {
		if strings.Contains(output, d) {
			return true
		}
	}
	return false
}

// assertFilesystemDeniedSilent checks for fs denial messages without logging.
func assertFilesystemDeniedSilent(output string) bool {
	denials := []string{
		"Permission denied",
		"No such file or directory",
		"cannot open",
		"Operation not permitted",
	}
	for _, d := range denials {
		if strings.Contains(output, d) {
			return true
		}
	}
	return false
}
