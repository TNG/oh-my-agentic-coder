package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadLauncherReadsGlobalConfigFromXDGConfigHome(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	workdir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	if err := os.MkdirAll(filepath.Join(xdg, "omac"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "omac", "config.yaml"), []byte("mcp:\n  unregistered: ignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lc, path, err := LoadLauncher(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(xdg, "omac", "config.yaml"); path != want {
		t.Fatalf("config path = %q, want %q", path, want)
	}
	if lc.MCP.Unregistered != "ignore" {
		t.Fatalf("mcp.unregistered = %q, want ignore", lc.MCP.Unregistered)
	}
}

func TestLoadLauncherIgnoresHomeConfigWhenXDGConfigHomeIsSet(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	workdir := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)

	if err := os.MkdirAll(filepath.Join(home, ".config", "omac"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "omac", "config.yaml"), []byte("mcp:\n  unregistered: ignore\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lc, path, err := LoadLauncher(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if path != "" || lc.MCP.Unregistered != "" {
		t.Fatalf("read %q with mcp.unregistered=%q, want the ~/.config file to be ignored", path, lc.MCP.Unregistered)
	}
}
