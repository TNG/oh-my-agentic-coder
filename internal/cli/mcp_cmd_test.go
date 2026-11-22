package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcpreg"
)

func mcpTestSetup(t *testing.T) (wd, def string) {
	t.Helper()
	isolateHome(t)
	t.Setenv("OMAC_KEYCHAIN_MOCK", filepath.Join(t.TempDir(), "keychain.json"))
	t.Setenv("OMAC_SOCKET", "")
	t.Setenv("OMAC_FACADE_TOKEN", "")
	wd = t.TempDir()
	def = filepath.Join(t.TempDir(), "mcp.yaml")
	body := "name: demo\nserver:\n  command: [npx, -y, demo]\nsecrets: [API_TOKEN]\n"
	if err := os.WriteFile(def, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return wd, def
}

func TestMCPRegisterListShowDeregister(t *testing.T) {
	wd, def := mcpTestSetup(t)
	t.Setenv("OMAC_SECRET_API_TOKEN", "tok-123")

	env, read := captureEnv(t, wd)
	if code := runMCP([]string{"register", def, "--harness", "opencode,claude"}, env); code != ExitOK {
		_, e := read()
		t.Fatalf("register exit %d: %s", code, e)
	}

	env, read = captureEnv(t, wd)
	if code := runMCP([]string{"list"}, env); code != ExitOK {
		t.Fatalf("list exit %d", code)
	}
	if out, _ := read(); !strings.Contains(out, "demo") || !strings.Contains(out, "opencode,claude-code") {
		t.Fatalf("list output: %q", out)
	}

	env, read = captureEnv(t, wd)
	if code := runMCP([]string{"show", "demo"}, env); code != ExitOK {
		t.Fatalf("show exit %d", code)
	}
	out, _ := read()
	if !strings.Contains(out, "API_TOKEN (stored)") || strings.Contains(out, "tok-123") {
		t.Fatalf("show output: %q", out)
	}

	env, _ = captureEnv(t, wd)
	if code := runMCP([]string{"deregister", "demo"}, env); code != ExitOK {
		t.Fatalf("deregister exit %d", code)
	}
	if _, err := mcpreg.Load(mcpreg.ScopeFor(wd), "demo"); err == nil {
		t.Fatal("still registered")
	}
}

func TestMCPRegisterRequiresHarness(t *testing.T) {
	wd, def := mcpTestSetup(t)
	env, _ := captureEnv(t, wd)
	if code := runMCP([]string{"register", def}, env); code != ExitMisuse {
		t.Fatalf("exit %d, want %d", code, ExitMisuse)
	}
}

func TestMCPRegisterGlobalRefusesCommandInsideWorkdir(t *testing.T) {
	wd, _ := mcpTestSetup(t)
	def := filepath.Join(t.TempDir(), "mcp.yaml")
	body := "name: demo\nserver:\n  command: [" + filepath.Join(wd, "srv") + "]\n"
	if err := os.WriteFile(def, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env, _ := captureEnv(t, wd)
	if code := runMCP([]string{"register", def, "--harness", "opencode", "--scope", "global"}, env); code != ExitMisuse {
		t.Fatalf("exit %d", code)
	}
}

func TestMCPMutationsRefusedInsideSandbox(t *testing.T) {
	for _, v := range []string{"OMAC_SOCKET", "OMAC_FACADE_TOKEN"} {
		t.Run(v, func(t *testing.T) {
			wd, def := mcpTestSetup(t)
			t.Setenv(v, "x")
			for _, args := range [][]string{
				{"register", def, "--harness", "opencode"},
				{"deregister", "demo"},
			} {
				env, read := captureEnv(t, wd)
				if code := runMCP(args, env); code != ExitMisuse {
					t.Fatalf("%v: exit %d", args, code)
				}
				if _, e := read(); !strings.Contains(e, "inside the omac sandbox") {
					t.Fatalf("%v: stderr %q", args, e)
				}
			}
			if entries, _ := mcpreg.List(mcpreg.ScopeFor(wd)); len(entries) != 0 {
				t.Fatal("registered from inside the sandbox")
			}
		})
	}
}

func TestMCPListAllMarksStaleScope(t *testing.T) {
	wd, def := mcpTestSetup(t)
	t.Setenv("OMAC_SECRET_API_TOKEN", "tok")
	env, _ := captureEnv(t, wd)
	if code := runMCP([]string{"register", def, "--harness", "opencode"}, env); code != ExitOK {
		t.Fatalf("register exit %d", code)
	}
	if err := os.RemoveAll(wd); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	env, read := captureEnv(t, other)
	if code := runMCP([]string{"list", "--all"}, env); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if out, _ := read(); !strings.Contains(out, "demo") || !strings.Contains(out, "stale") {
		t.Fatalf("output %q", out)
	}
	env, read = captureEnv(t, other)
	runMCP([]string{"list"}, env)
	if out, _ := read(); strings.Contains(out, "demo") {
		t.Fatalf("plain list leaked another scope: %q", out)
	}
}
