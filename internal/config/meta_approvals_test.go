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

// An empty block is a declaration that lists nothing as safe, so every
// route asks. It must not be confused with a missing block (nothing asks).
func TestApprovals_EmptyBlockAsksForEverything(t *testing.T) {
	for name, block := range map[string]string{
		"empty list": "  approvals:\n    endpoints: []\n",
		"empty map":  "  approvals: {}\n",
		"no value":   "  approvals:\n",
		"null":       "  approvals: null\n",
		"via merge":  "  <<: {approvals: null}\n",
	} {
		m, err := loadApprovalsMeta(t, block)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if m.Sidecar.Approvals == nil {
			t.Fatalf("%s: block read as absent", name)
		}
		set, err := m.Sidecar.Approvals.Compile()
		if err != nil {
			t.Fatalf("%s: Compile: %v", name, err)
		}
		if _, ok := set.Lookup("GET", []string{"status"}); ok {
			t.Errorf("%s: GET /status matched an empty declaration", name)
		}
	}
}

// A misspelled block key would otherwise read as "no block": nothing asks.
func TestApprovals_MisspelledBlockKeyIsRefused(t *testing.T) {
	for _, k := range []string{"aprovals", "approval", "Approvals", "approvls", "aproval"} {
		_, err := loadApprovalsMeta(t, "  "+k+":\n    endpoints:\n      - {match: \"GET /a\", approval: none}\n")
		if err == nil || !strings.Contains(err.Error(), "did you mean approvals") {
			t.Errorf("%s: err = %v", k, err)
		}
	}
	// At the top level the block would be ignored.
	p := filepath.Join(t.TempDir(), MetaFileName)
	os.WriteFile(p, []byte("name: t\napprovals:\n  endpoints: []\nsidecar:\n  command: [\"true\"]\n"), 0o600)
	if _, err := LoadMeta(p); err == nil || !strings.Contains(err.Error(), "belongs under sidecar") {
		t.Errorf("top-level approvals: err = %v", err)
	}
	// Real sidecar keys are not near-misses.
	if _, err := loadApprovalsMeta(t, "  protocols: [\"http\"]\n"); err != nil {
		t.Errorf("protocols refused: %v", err)
	}
}

func TestApprovals_Refused(t *testing.T) {
	cases := map[string]string{
		"unknown key":       "  approvals:\n    endpoints:\n      - {match: \"GET /a\", approval: none}\n    default: none\n",
		"unknown entry key": "  approvals:\n    endpoints:\n      - {match: \"GET /a\", approval: none, body: {}}\n",
		"typo approval":     "  approvals:\n    endpoints:\n      - {match: \"GET /a\", aproval: none}\n",
		"bad value":         "  approvals:\n    endpoints:\n      - {match: \"GET /a\", approval: ask}\n",
		"none on ANY":       "  approvals:\n    endpoints:\n      - {match: \"ANY /a\", approval: none}\n",
		"none on **":        "  approvals:\n    endpoints:\n      - {match: \"GET /a/**\", approval: none}\n",
		"equal specificity": "  approvals:\n    endpoints:\n      - {match: \"GET /a/{x}\", approval: none}\n      - {match: \"GET /a/{y}\", approval: required}\n",
		"bad match":         "  approvals:\n    endpoints:\n      - {match: \"/a\", approval: none}\n",
	}
	for name, block := range cases {
		if _, err := loadApprovalsMeta(t, block); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "approvals") {
			t.Errorf("%s: error %q does not name the block", name, err)
		}
	}
}
