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

func TestApprovals_AliasToAnchorOutsideTheBlock(t *testing.T) {
	p := filepath.Join(t.TempDir(), MetaFileName)
	doc := "name: t\nx-safe: &safe {match: \"GET /status\", approval: none}\nsidecar:\n  command: [\"true\"]\n  approvals:\n    endpoints:\n      - *safe\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Sidecar.Approvals.Endpoints; len(got) != 1 || got[0].Match != "GET /status" {
		t.Errorf("endpoints = %+v", got)
	}
}

func TestApprovals_UnknownKeyBehindAliasIsRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), MetaFileName)
	doc := "name: t\nx-e: &e {match: \"GET /a\", aproval: none}\nsidecar:\n  command: [\"true\"]\n  approvals:\n    endpoints:\n      - *e\n"
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMeta(p); err == nil || !strings.Contains(err.Error(), "sidecar.approvals.endpoints[0].aproval") {
		t.Errorf("err = %v, want the misspelled key named", err)
	}
}

func writeApprovalsDoc(t *testing.T, doc string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), MetaFileName)
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// A key that stops the load is reported by the error alone, not also as an
// "ignored" warning.
func TestApprovals_RefusedKeyIsNotWarnedAsIgnored(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown key in block": "name: t\nsidecar:\n  command: [\"true\"]\n  approvals:\n    endpoints: []\n    default: none\n",
		"unknown entry key":    "name: t\nsidecar:\n  command: [\"true\"]\n  approvals:\n    endpoints:\n      - {match: \"GET /a\", aproval: none}\n",
		"misspelled block":     "name: t\nsidecar:\n  command: [\"true\"]\n  aprovals: {}\n",
		"top-level block":      "name: t\napprovals: {}\nsidecar:\n  command: [\"true\"]\n",
	} {
		_, warnings, err := LoadMetaWithWarnings(writeApprovalsDoc(t, doc))
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(warnings) != 0 {
			t.Errorf("%s: warnings %q next to error %v", name, warnings, err)
		}
	}
	_, warnings, err := LoadMetaWithWarnings(writeApprovalsDoc(t, "name: t\nsidecar:\n  command: [\"true\"]\n  hnit: x\n"))
	if err != nil || len(warnings) != 1 {
		t.Errorf("other unknown keys still warn: warnings=%q err=%v", warnings, err)
	}
}

func TestApprovals_WrongKindNamesTheBlock(t *testing.T) {
	for doc, want := range map[string]string{
		"name: t\nsidecar: \"cmd\"\n": "sidecar must be a mapping",
		"- a\n- b\n":                  "omac.yaml must be a mapping",
		"name: t\nsidecar:\n  command: [\"true\"]\n  approvals: [1]\n": "sidecar.approvals must be a mapping",
	} {
		_, err := LoadMeta(writeApprovalsDoc(t, doc))
		if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "plain") {
			t.Errorf("%q: err = %v, want %q", doc, err, want)
		}
	}
}

// The keys pulled in by a merge are seen even when a value elsewhere in the
// mapping could not be decoded into a generic map (here a duplicate key).
func TestApprovals_MergedBlockNextToUndecodableValue(t *testing.T) {
	doc := "name: t\nsidecar:\n  <<: {approvals: null}\n  command: [\"true\"]\n  x-ext: {a: 1, a: 2}\n"
	m, err := LoadMeta(writeApprovalsDoc(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if m.Sidecar.Approvals == nil {
		t.Error("merged approvals block read as absent")
	}
}

func TestApprovals_MergedMisspellingIsRefused(t *testing.T) {
	for _, doc := range []string{
		"name: t\nsidecar:\n  <<: {aprovals: {endpoints: []}}\n  command: [\"true\"]\n",
		"name: t\nx-d: &d {aprovals: {endpoints: []}}\nsidecar:\n  <<: [*d]\n  command: [\"true\"]\n",
	} {
		if _, err := LoadMeta(writeApprovalsDoc(t, doc)); err == nil || !strings.Contains(err.Error(), "did you mean approvals") {
			t.Errorf("%q: err = %v", doc, err)
		}
	}
}

func TestApprovals_ExtensionKeysHoldAnchors(t *testing.T) {
	for _, k := range []string{"x-approvals", "x_approvals", "X-Approval"} {
		doc := "name: t\n" + k + ": &s {match: \"GET /status\", approval: none}\nsidecar:\n  command: [\"true\"]\n  approvals:\n    endpoints: [*s]\n"
		if _, err := LoadMeta(writeApprovalsDoc(t, doc)); err != nil {
			t.Errorf("%s: %v", k, err)
		}
	}
}
