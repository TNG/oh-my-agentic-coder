package config

import (
	"os"
	"path/filepath"
	"testing"
)

const mcpYAML = "mcp:\n  unregistered: refuse\n"

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadLauncherIgnoresProjectLocalMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, ".opencode", "oh-my-agentic-coder.yaml"), mcpYAML)

	lc, _, err := LoadLauncher(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if lc.MCP != (MCPConfig{}) {
		t.Fatalf("project-local mcp block must be ignored, got %+v", lc.MCP)
	}
}

func TestLoadLauncherIgnoresProjectLocalMCPWithGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	workdir := t.TempDir()
	writeFile(t, filepath.Join(home, ".config", "omac", "config.yaml"), "mcp:\n  unregistered: warn\n")
	writeFile(t, filepath.Join(workdir, ".opencode", "oh-my-agentic-coder.yaml"), "mcp:\n  unregistered: allow\n")

	lc, _, err := LoadLauncher(workdir)
	if err != nil {
		t.Fatal(err)
	}
	if lc.MCP.Unregistered != "warn" {
		t.Fatalf("global value must win, got %+v", lc.MCP)
	}
}

func TestLoadLauncherHonorsGlobalMCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFile(t, filepath.Join(home, ".config", "omac", "config.yaml"), mcpYAML)

	lc, _, err := LoadLauncher(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := MCPConfig{Unregistered: "refuse"}
	if lc.MCP != want {
		t.Fatalf("got %+v, want %+v", lc.MCP, want)
	}
}

func TestLoadLauncherRejectsInvalidGlobalMCP(t *testing.T) {
	for _, body := range []string{
		"mcp:\n  unregistered: block\n",
		"mcp:\n  unregistered: ignore\n",
	} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		writeFile(t, filepath.Join(home, ".config", "omac", "config.yaml"), body)
		if _, _, err := LoadLauncher(t.TempDir()); err == nil {
			t.Fatalf("expected error for %q", body)
		}
	}
}

func TestHarnessMCPInjectionKinds(t *testing.T) {
	want := map[string]MCPInjectionKind{
		"opencode":    MCPInjectOpenCodeEnv,
		"claude-code": MCPInjectClaudeFlag,
	}
	for name, kind := range want {
		h, ok := LookupHarness(name)
		if !ok || h.MCPInjection != kind {
			t.Fatalf("%s: got %v, want %v", name, h.MCPInjection, kind)
		}
	}
	for _, h := range harnessRegistry() {
		if _, ok := want[h.Name]; !ok && h.MCPInjection != MCPInjectNone {
			t.Fatalf("%s must not inject MCP", h.Name)
		}
	}
}
