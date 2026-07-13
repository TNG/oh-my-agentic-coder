package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultAuditEnabled(t *testing.T) {
	lc := DefaultLauncherConfig()
	if !lc.Audit.AuditEnabled() {
		t.Fatalf("audit should default to enabled")
	}
	if lc.Audit.Strict || lc.Audit.Syslog || lc.Audit.Path != "" {
		t.Fatalf("unexpected non-default audit values: %+v", lc.Audit)
	}
}

func TestLoadLauncherAuditExplicitDisable(t *testing.T) {
	dir := t.TempDir()
	ocDir := filepath.Join(dir, ".opencode")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `audit:
  enabled: false
  path: /var/log/omac/audit.jsonl
  syslog: true
  strict: true
`
	if err := os.WriteFile(filepath.Join(ocDir, "oh-my-agentic-coder.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	lc, _, err := LoadLauncher(dir)
	if err != nil {
		t.Fatalf("LoadLauncher: %v", err)
	}
	if lc.Audit.AuditEnabled() {
		t.Fatalf("explicit enabled:false must be preserved")
	}
	if lc.Audit.Path != "/var/log/omac/audit.jsonl" || !lc.Audit.Syslog || !lc.Audit.Strict {
		t.Fatalf("audit fields not parsed: %+v", lc.Audit)
	}
}

func TestLoadLauncherAuditUnsetDefaultsOn(t *testing.T) {
	dir := t.TempDir()
	ocDir := filepath.Join(dir, ".opencode")
	if err := os.MkdirAll(ocDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Config present but with no audit block: audit should default on.
	yaml := "facade:\n  idle_timeout_secs: 60\n"
	if err := os.WriteFile(filepath.Join(ocDir, "oh-my-agentic-coder.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	lc, _, err := LoadLauncher(dir)
	if err != nil {
		t.Fatalf("LoadLauncher: %v", err)
	}
	if !lc.Audit.AuditEnabled() {
		t.Fatalf("unset audit block should default to enabled")
	}
}
