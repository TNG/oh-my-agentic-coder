package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
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

func TestMCPServeRefusesATamperedSnapshot(t *testing.T) {
	wd, def := mcpTestSetup(t)
	e, err := mcpreg.Register(mcpreg.GlobalScope(), def, []string{"opencode"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.DefPath, []byte("name: demo\nserver:\n  command: [sh, -c, evil]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIDECAR_PORT", "1")
	t.Setenv("OMAC_SIDECAR_SECRET", "s3cret")
	env, read := captureEnv(t, wd)
	if code := runMCP([]string{"serve", "--def", e.DefPath}, env); code != ExitConfigInvalid {
		t.Fatalf("serve exit %d, want %d", code, ExitConfigInvalid)
	}
	if _, stderr := read(); !strings.Contains(stderr, "hash") {
		t.Errorf("stderr %q", stderr)
	}
}

func TestMCPRegisterDropsReviewedBlockAndWarnsOnNoneApprovals(t *testing.T) {
	wd, def := mcpTestSetup(t)
	t.Setenv("OMAC_SECRET_API_TOKEN", "tok-123")
	hashes := map[string]string{"echo": "abc"}
	body := "name: demo\nserver:\n  command: [npx, -y, demo]\nsecrets: [API_TOKEN]\ntools:\n  - {name: echo, class: read}\napprovals:\n  write: none\n  delete: none\nreviewed:\n  tools_hash: " + mcppolicy.ListHash(hashes) + "\n  tools: {echo: abc}\n  at: 2026-01-01T00:00:00Z\n"
	if err := os.WriteFile(def, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	env, read := captureEnv(t, wd)
	if code := runMCP([]string{"register", def, "--harness", "opencode"}, env); code != ExitOK {
		_, e := read()
		t.Fatalf("register exit %d: %s", code, e)
	}
	out, errOut := read()
	if !strings.Contains(out, "dropped the reviewed block") || !strings.Contains(out, "omac mcp tools demo --classify") {
		t.Errorf("no drop notice: %q", out)
	}
	if !strings.Contains(errOut, "warning:") || !strings.Contains(errOut, "write and delete") {
		t.Errorf("no approvals warning: %q", errOut)
	}
	e, err := mcpreg.Load(mcpreg.ScopeFor(wd), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if e.Def.Review != nil {
		t.Fatalf("review kept: %+v", e.Def.Review)
	}
	if len(e.Def.Tools) != 1 || e.Def.Approvals == nil || e.Def.Approvals.Write != approval.None {
		t.Errorf("other fields changed: %+v", e.Def)
	}
}

func TestMCPRegisterPlainFileHasNoNoticeOrWarning(t *testing.T) {
	wd, def := mcpTestSetup(t)
	t.Setenv("OMAC_SECRET_API_TOKEN", "tok-123")
	env, read := captureEnv(t, wd)
	if code := runMCP([]string{"register", def, "--harness", "opencode"}, env); code != ExitOK {
		t.Fatal("register failed")
	}
	out, errOut := read()
	if strings.Contains(out, "dropped") || strings.Contains(errOut, "warning") {
		t.Errorf("unexpected output %q %q", out, errOut)
	}
}
