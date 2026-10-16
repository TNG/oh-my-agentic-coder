package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMeta writes body as omac.yaml into a fresh skill directory and
// returns the file path.
func writeMeta(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, MetaFileName)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMeta_ParsesHintAndDocs(t *testing.T) {
	p := writeMeta(t, `name: s
sidecar:
  command: ["true"]
  secrets:
    - name: TOKEN
      description: The token.
      hint: |
        1. Open the site.
        2. Copy the token.
      docs: SKILL.md#getting-a-token
  config:
    - name: REGION
      hint: Pick the region closest to you.
      docs: docs/regions.md
`)
	m, err := LoadMeta(p)
	if err != nil {
		t.Fatalf("LoadMeta: %v", err)
	}
	sec := m.Sidecar.Secrets[0]
	if sec.Hint != "1. Open the site.\n2. Copy the token.\n" {
		t.Errorf("secret hint = %q", sec.Hint)
	}
	if sec.Docs != "SKILL.md#getting-a-token" {
		t.Errorf("secret docs = %q", sec.Docs)
	}
	c := m.Sidecar.Config[0]
	if c.Hint != "Pick the region closest to you." || c.Docs != "docs/regions.md" {
		t.Errorf("config hint/docs = %q / %q", c.Hint, c.Docs)
	}
}

func TestSidecar_DocsRefValidation(t *testing.T) {
	cases := []struct {
		ref     string
		wantErr string // "" means valid
	}{
		{"", ""},
		{"SKILL.md", ""},
		{"SKILL.md#obtaining-slack-tokens", ""},
		{"docs/tokens.md#step-2", ""},
		{"docs/../SKILL.md", ""}, // stays inside after cleaning
		{"#section", "does not name a file"},
		{"/etc/passwd", "not an absolute path"},
		{"/home/me/SKILL.md#x", "not an absolute path"},
		{"../other-skill/SKILL.md", "stay inside the skill directory"},
		{"docs/../../SKILL.md", "stay inside the skill directory"},
		{"..", "stay inside the skill directory"},
	}
	for _, tc := range cases {
		for _, kind := range []string{"secrets", "config"} {
			m := validMeta()
			if kind == "secrets" {
				m.Sidecar.Secrets = []SecretSpec{{Name: "TOKEN", Docs: tc.ref}}
			} else {
				m.Sidecar.Config = []ConfigSpec{{Name: "FIELD", Docs: tc.ref}}
			}
			err := m.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("%s docs %q: unexpected error: %v", kind, tc.ref, err)
				}
				continue
			}
			if err == nil {
				t.Errorf("%s docs %q: expected an error containing %q", kind, tc.ref, tc.wantErr)
				continue
			}
			if !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), "sidecar."+kind+"[0].docs") {
				t.Errorf("%s docs %q: error %q should name the field and contain %q", kind, tc.ref, err, tc.wantErr)
			}
		}
	}
}

func TestDocsPath_AbsoluteWithAnchor(t *testing.T) {
	dir := t.TempDir()
	abs, anchor := DocsPath(dir, "docs/tokens.md#step-2")
	if abs != filepath.Join(dir, "docs", "tokens.md") || anchor != "step-2" {
		t.Errorf("DocsPath = %q, %q", abs, anchor)
	}
	if abs, anchor := DocsPath(dir, ""); abs != "" || anchor != "" {
		t.Errorf("empty ref: DocsPath = %q, %q; want empty", abs, anchor)
	}
}

func TestMeta_MissingDocs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := validMeta()
	m.Sidecar.Secrets = []SecretSpec{
		{Name: "A", Docs: "SKILL.md#present"},
		{Name: "B", Docs: "missing.md#gone"},
		{Name: "C"},
	}
	m.Sidecar.Config = []ConfigSpec{{Name: "F", Docs: "nope.md"}}

	got := m.MissingDocs(dir)
	want := []string{
		"sidecar.secrets[1].docs: missing.md not found in the skill directory",
		"sidecar.config[0].docs: nope.md not found in the skill directory",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("MissingDocs =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
