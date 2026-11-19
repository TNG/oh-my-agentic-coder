package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadApprovalsMeta(t *testing.T, block string) (*Meta, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), MetaFileName)
	doc := "name: t\nsidecar:\n  command: [\"true\"]\n" + block
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return LoadMeta(p)
}

func TestApprovals_Valid(t *testing.T) {
	m, err := loadApprovalsMeta(t, `  approvals:
    version: 1
    endpoints:
      - match: "GET /status"
        approval: none
      - match: "POST /echo"
        approval: required
        description: "Echoes the body"
      - match: "ANY /**"
        approval: required
`)
	if err != nil {
		t.Fatal(err)
	}
	set, err := m.Sidecar.Approvals.Compile()
	if err != nil || set == nil {
		t.Fatalf("Compile: %v", err)
	}
}

func TestApprovals_AbsentIsUngated(t *testing.T) {
	m, err := loadApprovalsMeta(t, "")
	if err != nil || m.Sidecar.Approvals != nil {
		t.Fatalf("err=%v approvals=%v", err, m.Sidecar.Approvals)
	}
}

func TestApprovals_Refused(t *testing.T) {
	cases := map[string]string{
		"unknown version":   "  approvals:\n    version: 2\n    endpoints:\n      - {match: \"GET /a\", approval: none}\n",
		"missing version":   "  approvals:\n    endpoints:\n      - {match: \"GET /a\", approval: none}\n",
		"unknown key":       "  approvals:\n    version: 1\n    endpoints:\n      - {match: \"GET /a\", approval: none}\n    default: none\n",
		"unknown entry key": "  approvals:\n    version: 1\n    endpoints:\n      - {match: \"GET /a\", approval: none, body: {}}\n",
		"typo approval":     "  approvals:\n    version: 1\n    endpoints:\n      - {match: \"GET /a\", aproval: none}\n",
		"bad value":         "  approvals:\n    version: 1\n    endpoints:\n      - {match: \"GET /a\", approval: ask}\n",
		"none on ANY":       "  approvals:\n    version: 1\n    endpoints:\n      - {match: \"ANY /a\", approval: none}\n",
		"none on **":        "  approvals:\n    version: 1\n    endpoints:\n      - {match: \"GET /a/**\", approval: none}\n",
		"equal specificity": "  approvals:\n    version: 1\n    endpoints:\n      - {match: \"GET /a/{x}\", approval: none}\n      - {match: \"GET /a/{y}\", approval: required}\n",
		"no endpoints":      "  approvals:\n    version: 1\n    endpoints: []\n",
		"bad match":         "  approvals:\n    version: 1\n    endpoints:\n      - {match: \"/a\", approval: none}\n",
	}
	for name, block := range cases {
		if _, err := loadApprovalsMeta(t, block); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "approvals") {
			t.Errorf("%s: error %q does not name the block", name, err)
		}
	}
}
