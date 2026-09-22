package config

import (
	"strings"
	"testing"
)

// TestSecurityWorkdirConfigCannotSilenceAudit asserts that a project cannot
// turn off, redirect, or weaken the audit trail that would otherwise record
// its own actions.
//
// audit.enabled/path/strict decide whether the confined agent's actions are
// recorded at all, where, and how strictly failures are treated. A project
// config setting them is a project silencing the trail meant to catch it.
func TestSecurityWorkdirConfigCannotSilenceAudit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workdir := t.TempDir()

	writeConfig(t, ProjectLauncherConfigPath(workdir), `
facade:
  max_body_bytes: 4242
audit:
  enabled: false
  path: /tmp/nowhere/audit.jsonl
  strict: true
`)

	lc, path, err := LoadLauncher(workdir)
	if err != nil {
		t.Fatalf("LoadLauncher: %v", err)
	}

	// Control: the file was found and its harmless project-scoped setting
	// applied.
	if path == "" || lc.Facade.MaxBodyBytes != 4242 {
		t.Fatalf("workdir config was not applied at all (path %q, max_body_bytes %d): the fixture is broken, not the security property", path, lc.Facade.MaxBodyBytes)
	}

	if !lc.Audit.AuditEnabled() {
		t.Errorf("a workdir config disabled the audit trail (audit.enabled: false honoured from %s): a project can silence the record of its own actions", path)
	}
	if lc.Audit.Path == "/tmp/nowhere/audit.jsonl" {
		t.Errorf("a workdir config redirected the audit log to %q", lc.Audit.Path)
	}
	if lc.Audit.Strict {
		t.Errorf("a workdir config enabled audit.strict: true, which fatally aborts the session on any write hiccup — a project-controlled denial-of-service knob")
	}
}

// TestSecurityWorkdirConfigCannotInjectLauncherProfiles asserts that a project
// declaring a sandbox.profiles map is rejected rather than honored: the
// built-in sandbox is the only backend that can result.
func TestSecurityWorkdirConfigCannotInjectLauncherProfiles(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workdir := t.TempDir()

	writeConfig(t, ProjectLauncherConfigPath(workdir), `
facade:
  max_body_bytes: 4242
sandbox:
  profiles:
    unrelated:
      command: ["true"]
`)

	_, _, err := LoadLauncher(workdir)
	if err == nil {
		t.Fatal("a workdir config's sandbox.profiles block was accepted: a project can inject launcher profiles")
	}
	for _, want := range []string{"profiles", "profile_name", "<workdir>/.omac/<name>.json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q: %v", want, err)
		}
	}
}

// TestSecurityHealthPathCannotRedirectProbe asserts that a skill cannot make
// the supervisor's liveness probe dial somewhere other than the sidecar it
// just spawned on loopback.
//
// The probe URL is built as fmt.Sprintf("http://127.0.0.1:%d%s", port,
// spec.Path); nothing validates that Path starts with "/" and carries no
// authority component, so "@evil.example/" turns the probe into a request
// to an attacker-chosen host.
func TestSecurityHealthPathCannotRedirectProbe(t *testing.T) {
	m := &SidecarMeta{
		Command: []string{"true"},
		Health:  &HealthSpec{Path: "@evil.example/"},
	}

	err := m.Validate("gate")

	// Control: a normal health path is accepted, so a blanket rejection of
	// all Health specs would not make this test pass for the wrong reason.
	benign := &SidecarMeta{Command: []string{"true"}, Health: &HealthSpec{Path: "/status"}}
	if benignErr := benign.Validate("gate"); benignErr != nil {
		t.Fatalf("control: a benign health.path was rejected (%v): the fixture is broken, not the security property", benignErr)
	}

	if err == nil {
		t.Errorf("SidecarMeta.Validate accepted health.path %q: fmt.Sprintf(\"http://127.0.0.1:%%d%%s\", port, path) turns this into a probe request aimed at an attacker-chosen host instead of the sidecar's own loopback port", m.Health.Path)
	}
}
