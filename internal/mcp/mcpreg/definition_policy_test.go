package mcpreg

import (
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
	if p.Server != "demo" || p.Access != mcppolicy.ReadWriteDelete || p.Pin != nil || len(p.Tools) != 0 {
		t.Errorf("policy = %+v", p)
	}
	want := mcppolicy.Approvals{Read: approval.None, Write: approval.Required, Delete: approval.Required}
	if p.Approvals != want {
		t.Errorf("approvals = %+v", p.Approvals)
	}
}

func TestPolicyFromDefinition(t *testing.T) {
	pinHash := mcppolicy.ListHash(map[string]string{"get": "h"})
	raw := policyBase + `access: read-write
tools:
  - {name: get, class: read}
  - {name: put, class: write}
  - {name: hidden, class: "off"}
approvals:
  read: required
  delete: none
pin:
  tools_hash: ` + pinHash + `
  tools: {get: h}
  pinned_at: 2026-01-02T03:04:05Z
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
	if p.Pin == nil || p.Pin.Tools["get"] != "h" || p.Pin.PinnedAt.Year() != 2026 {
		t.Errorf("pin = %+v", p.Pin)
	}
}

func TestParseDefinitionRefusesBadPolicyFields(t *testing.T) {
	cases := map[string]string{
		"bad access":           "access: admin\n",
		"duplicate tool":       "tools:\n  - {name: a, class: read}\n  - {name: a, class: write}\n",
		"bad class":            "tools:\n  - {name: a, class: execute}\n",
		"missing class":        "tools:\n  - {name: a}\n",
		"empty tool name":      "tools:\n  - {name: \"\", class: read}\n",
		"bad approval mode":    "approvals:\n  write: sometimes\n",
		"unknown approval key": "approvals:\n  admin: none\n",
		"unknown tool key":     "tools:\n  - {name: a, class: read, extra: 1}\n",
		"pin hash mismatch":    "pin:\n  tools_hash: abc\n  tools: {a: h}\n",
		"unknown key":          "bogus: 1\n",
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
