package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/TNG/oh-my-agentic-coder/internal/approver"
	"github.com/TNG/oh-my-agentic-coder/internal/registry"
)

func addVerdict(t *testing.T, scope, skill, match string) {
	t.Helper()
	v := approver.Verdict{ID: approver.VerdictID(scope, skill, match, "required"), Scope: scope, Skill: skill, Match: match, Approval: "required", CreatedAt: time.Now()}
	if err := approver.NewStore("").Add(v); err != nil {
		t.Fatal(err)
	}
}

func storedKeys(t *testing.T) map[string]bool {
	t.Helper()
	all, err := approver.NewStore("").List()
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]bool{}
	for _, v := range all {
		m[v.Scope+"|"+v.Skill] = true
	}
	return m
}

func TestMCPDeregisterRemovesOnlyThisServersApprovalsInThisProject(t *testing.T) {
	wd, def := mcpTestSetup(t)
	t.Setenv("OMAC_SECRET_API_TOKEN", "tok")
	env, _ := captureEnv(t, wd)
	if code := runMCP([]string{"register", def, "--harness", "opencode"}, env); code != ExitOK {
		t.Fatal(code)
	}
	here := projectScope(wd).Key
	addVerdict(t, here, "mcp:demo", "tool a (write)")
	addVerdict(t, here, "mcp:demo", "tool b (delete)")
	addVerdict(t, here, "mcp:other", "tool a (write)")
	addVerdict(t, "wd-elsewhere", "mcp:demo", "tool a (write)")
	addVerdict(t, here, "demo", "POST /x") // a skill of the same name

	env, read := captureEnv(t, wd)
	if code := runMCP([]string{"deregister", "demo"}, env); code != ExitOK {
		t.Fatal(code)
	}
	if out, _ := read(); !strings.Contains(out, "removed 2 stored approval(s)") {
		t.Errorf("output %q", out)
	}
	got := storedKeys(t)
	if got[here+"|mcp:demo"] || !got[here+"|mcp:other"] || !got["wd-elsewhere|mcp:demo"] || !got[here+"|demo"] || len(got) != 3 {
		t.Fatalf("stored after deregister: %v", got)
	}
}

func TestMCPDeregisterGlobalRemovesServerApprovalsOfAllScopes(t *testing.T) {
	wd, def := mcpTestSetup(t)
	t.Setenv("OMAC_SECRET_API_TOKEN", "tok")
	env, _ := captureEnv(t, wd)
	if code := runMCP([]string{"register", def, "--harness", "opencode", "--scope", "global"}, env); code != ExitOK {
		t.Fatal(code)
	}
	addVerdict(t, projectScope(wd).Key, "mcp:demo", "tool a (write)")
	addVerdict(t, "global", "mcp:demo", "tool a (write)")
	addVerdict(t, "wd-elsewhere", "mcp:demo", "tool a (write)")
	addVerdict(t, "wd-elsewhere", "mcp:other", "tool a (write)")
	env, read := captureEnv(t, wd)
	if code := runMCP([]string{"deregister", "demo", "--scope", "global"}, env); code != ExitOK {
		t.Fatal(code)
	}
	if out, _ := read(); !strings.Contains(out, "removed 3 stored approval(s)") {
		t.Errorf("output %q", out)
	}
	if got := storedKeys(t); len(got) != 1 || !got["wd-elsewhere|mcp:other"] {
		t.Fatalf("stored: %v", got)
	}
}

func TestMCPAddReplaceKeepsStoredApprovals(t *testing.T) {
	wd, _ := mcpTestSetup(t)
	if code, _, e := runAdd(t, wd, "add", "demo", "--harness", "opencode", "--", "/bin/true"); code != ExitOK {
		t.Fatal(e)
	}
	addVerdict(t, projectScope(wd).Key, "mcp:demo", "tool a (write)")
	if code, _, e := runAdd(t, wd, "add", "demo", "--replace", "--", "/bin/false"); code != ExitOK {
		t.Fatal(e)
	}
	if len(storedKeys(t)) != 1 {
		t.Fatal("--replace dropped the stored approval")
	}
}

func TestDeregisterSkillRemovesOnlyThatSkillsApprovalsInThisProject(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	stageRegisteredSkill(t, wd, "demo")
	stageRegisteredSkill(t, wd, "keep")
	here := projectScope(wd).Key
	addVerdict(t, here, "demo", "POST /a")
	addVerdict(t, here, "keep", "POST /a")
	addVerdict(t, "wd-elsewhere", "demo", "POST /a")
	addVerdict(t, here, "mcp:demo", "tool a (write)")

	env, read := captureEnv(t, wd)
	if code := runDeregister([]string{"demo"}, env); code != ExitOK {
		t.Fatal(code)
	}
	if out, _ := read(); !strings.Contains(out, "removed 1 stored approval(s)") {
		t.Errorf("output %q", out)
	}
	got := storedKeys(t)
	if got[here+"|demo"] || !got[here+"|keep"] || !got["wd-elsewhere|demo"] || !got[here+"|mcp:demo"] || len(got) != 3 {
		t.Fatalf("stored: %v", got)
	}
}

func TestDeregisterGlobalSkillRemovesApprovalsOfAllScopes(t *testing.T) {
	isolateHome(t)
	wd := t.TempDir()
	reg, err := registry.LoadGlobal()
	if err != nil {
		t.Fatal(err)
	}
	reg.Upsert(registry.Entry{Name: "demo", SkillDir: "/skills/demo", BundleHash: "x"})
	reg.Upsert(registry.Entry{Name: "keep", SkillDir: "/skills/keep", BundleHash: "y"})
	if err := registry.SaveGlobal(reg); err != nil {
		t.Fatal(err)
	}
	here := projectScope(wd).Key
	addVerdict(t, here, "demo", "POST /a")
	addVerdict(t, "wd-elsewhere", "demo", "POST /a")
	addVerdict(t, "wd-elsewhere", "keep", "POST /a")
	addVerdict(t, here, "mcp:demo", "tool a (write)")

	env, read := captureEnv(t, wd)
	if code := runDeregister([]string{"--global", "demo"}, env); code != ExitOK {
		t.Fatal(code)
	}
	if out, _ := read(); !strings.Contains(out, "removed 2 stored approval(s)") {
		t.Errorf("output %q", out)
	}
	got := storedKeys(t)
	if len(got) != 2 || !got["wd-elsewhere|keep"] || !got[here+"|mcp:demo"] {
		t.Fatalf("stored: %v", got)
	}
}
