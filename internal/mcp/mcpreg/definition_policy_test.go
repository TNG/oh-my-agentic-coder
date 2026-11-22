package mcpreg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TNG/oh-my-agentic-coder/internal/approval"
	"github.com/TNG/oh-my-agentic-coder/internal/mcp/mcppolicy"
)

const policyBase = "name: demo\nserver:\n  command: [demo-server]\n"

func TestPolicyDefaults(t *testing.T) {
	def, err := ParseDefinition([]byte(policyBase))
	if err != nil {
		t.Fatal(err)
	}
	p := def.Policy()
	if p.Server != "demo" || p.Access != mcppolicy.ReadWriteDelete || p.Review != nil || len(p.Tools) != 0 {
		t.Errorf("policy = %+v", p)
	}
	want := mcppolicy.Approvals{Read: approval.None, Write: approval.Required, Delete: approval.Required}
	if p.Approvals != want {
		t.Errorf("approvals = %+v", p.Approvals)
	}
}

func TestPolicyFromDefinition(t *testing.T) {
	reviewHash := mcppolicy.ListHash(map[string]string{"get": "h"})
	raw := policyBase + `access: read-write
tools:
  - {name: get, class: read}
  - {name: put, class: write}
  - {name: hidden, class: "off"}
approvals:
  read: required
  delete: none
reviewed:
  tools_hash: ` + reviewHash + `
  tools: {get: h}
  at: 2026-01-02T03:04:05Z
`
	def, err := ParseDefinition([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	p := def.Policy()
	if p.Access != mcppolicy.ReadWrite {
		t.Errorf("access = %q", p.Access)
	}
	if p.Tools["get"] != mcppolicy.Read || p.Tools["put"] != mcppolicy.Write || p.Tools["hidden"] != mcppolicy.Off {
		t.Errorf("tools = %v", p.Tools)
	}
	want := mcppolicy.Approvals{Read: approval.Required, Write: approval.Required, Delete: approval.None}
	if p.Approvals != want {
		t.Errorf("a partial approvals block keeps the defaults for the rest: %+v", p.Approvals)
	}
	if p.Review == nil || p.Review.Tools["get"] != "h" || p.Review.At.Year() != 2026 {
		t.Errorf("review = %+v", p.Review)
	}
}

func TestParseDefinitionRefusesBadPolicyFields(t *testing.T) {
	cases := map[string]string{
		"bad access":             "access: admin\n",
		"duplicate tool":         "tools:\n  - {name: a, class: read}\n  - {name: a, class: write}\n",
		"bad class":              "tools:\n  - {name: a, class: execute}\n",
		"missing class":          "tools:\n  - {name: a}\n",
		"empty tool name":        "tools:\n  - {name: \"\", class: read}\n",
		"bad approval mode":      "approvals:\n  write: sometimes\n",
		"unknown approval key":   "approvals:\n  admin: none\n",
		"unknown tool key":       "tools:\n  - {name: a, class: read, extra: 1}\n",
		"reviewed hash mismatch": "reviewed:\n  tools_hash: abc\n  tools: {a: h}\n",
		"unknown key":            "bogus: 1\n",
	}
	for name, extra := range cases {
		if _, err := ParseDefinition([]byte(policyBase + extra)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := ParseDefinition([]byte(policyBase + "tools:\n  - {name: a, class: read}\n  - {name: a, class: write}\n"))
	if err == nil || !strings.Contains(err.Error(), `"a"`) {
		t.Errorf("error should name the tool: %v", err)
	}
}

func TestResolveCommandRefusesAProgramPATHFindsInTheWorkdir(t *testing.T) {
	workdir := t.TempDir()
	bin := filepath.Join(workdir, "node_modules", ".bin")
	outside := t.TempDir()
	for _, dir := range []string{bin, outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mcp-demo"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	def := &Definition{Name: "demo", Server: ServerSpec{Command: []string{"mcp-demo", "--flag"}}}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+outside)
	if _, err := ResolveCommand(def, workdir); err == nil || !strings.Contains(err.Error(), "inside the workdir") {
		t.Fatalf("program in the workdir: %v", err)
	}

	t.Setenv("PATH", outside+string(os.PathListSeparator)+bin)
	got, err := ResolveCommand(def, workdir)
	if err != nil || got != filepath.Join(outside, "mcp-demo") {
		t.Fatalf("program outside: %q %v", got, err)
	}
	if _, err := ResolveCommand(&Definition{Server: ServerSpec{Command: []string{"no-such-program-xyz"}}}, workdir); err == nil {
		t.Fatal("a missing program must fail")
	}
}

func TestDefinitionRefusesAVariableSetTwice(t *testing.T) {
	for name, body := range map[string]string{
		"secret and env":         "name: demo\nserver:\n  command: [demo-server]\n  env: {TOKEN: x}\nsecrets: [TOKEN]\n",
		"secret and mode":        policyBase + "secrets: [MODE]\nmode: {name: MODE, value: ro}\n",
		"env and passthrough":    "name: demo\nserver:\n  command: [demo-server]\n  env: {HTTPS_PROXY: x}\n  env_passthrough: [HTTPS_PROXY]\n",
		"secret and passthrough": "name: demo\nserver:\n  command: [demo-server]\n  env_passthrough: [TOKEN]\nsecrets: [TOKEN]\n",
	} {
		if _, err := ParseDefinition([]byte(body)); err == nil || !strings.Contains(err.Error(), "one place") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
