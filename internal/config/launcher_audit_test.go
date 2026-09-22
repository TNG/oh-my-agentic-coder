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

// TestGlobalAuditExplicitDisable verifies that audit settings in the
// user-global config are honoured (only the global config may control audit).
func TestGlobalAuditExplicitDisable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	globalDir := filepath.Join(home, ".config", "omac")
	if err := os.MkdirAll(globalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := `audit:
  enabled: false
  path: /var/log/omac/audit.jsonl
  syslog: true
  strict: true
`
	if err := os.WriteFile(filepath.Join(globalDir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	lc, _, err := LoadLauncher(t.TempDir())
	if err != nil {
		t.Fatalf("LoadLauncher: %v", err)
	}
	if lc.Audit.AuditEnabled() {
		t.Fatalf("explicit enabled:false in global config must be preserved")
	}
	if lc.Audit.Path != "/var/log/omac/audit.jsonl" || !lc.Audit.Syslog || !lc.Audit.Strict {
		t.Fatalf("audit fields not parsed: %+v", lc.Audit)
	}
}

func TestLoadLauncherAuditUnsetDefaultsOn(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(LocalConfigDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	// Config present but with no audit block: audit should default on.
	yaml := "facade:\n  idle_timeout_secs: 60\n"
	if err := os.WriteFile(ProjectLauncherConfigPath(dir), []byte(yaml), 0o644); err != nil {
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
