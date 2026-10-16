package config

import (
	"os"
	"path/filepath"
	"reflect"
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

// TestDocsWarnings covers every reason a docs value cannot be shown. Each
// one is a warning naming the field, never a load error.
func TestDocsWarnings(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# s\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		ref  string
		want string // "" means no warning
	}{
		{"", ""},
		{"SKILL.md", ""},
		{"SKILL.md#obtaining-slack-tokens", ""},
		{"docs/../SKILL.md", ""}, // stays inside after cleaning
		{"missing.md#gone", "missing.md not found in the skill directory"},
		{"docs", "docs is a directory, not a file"},
		{"#section", "does not name a file"},
		{"https://example.com/help", "not a URL"},
		{"~/notes.md", "'~' is not expanded"},
		{"/etc/passwd", "not an absolute path"},
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
			if err := m.Validate(); err != nil {
				t.Errorf("%s docs %q: Validate must not fail over docs: %v", kind, tc.ref, err)
			}
			got := m.DocsWarnings(dir)
			if tc.want == "" {
				if len(got) != 0 {
					t.Errorf("%s docs %q: unexpected warnings %q", kind, tc.ref, got)
				}
				continue
			}
			if len(got) != 1 || !strings.HasPrefix(got[0], "sidecar."+kind+"[0].docs: ") || !strings.Contains(got[0], tc.want) {
				t.Errorf("%s docs %q: warnings = %q, want one naming the field and containing %q", kind, tc.ref, got, tc.want)
			}
			if abs, _ := DocsPath(dir, tc.ref); docsRefProblem(tc.ref) != "" && abs != "" {
				t.Errorf("%s docs %q: DocsPath = %q, want \"\" for an unusable value", kind, tc.ref, abs)
			}
		}
	}
}

// A skill whose docs value is unusable kept loading before omac knew the
// key. It must keep loading now: start, serve and list all use LoadMeta.
func TestLoadMeta_BadDocsStillLoads(t *testing.T) {
	p := writeMeta(t, `name: s
sidecar:
  command: ["true"]
  secrets:
    - name: TOKEN
      docs: ../README.md
  config:
    - name: FIELD
      docs: /abs/path.md
`)
	m, err := LoadMeta(p)
	if err != nil {
		t.Fatalf("LoadMeta refused a skill over its docs value: %v", err)
	}
	if got := m.DocsWarnings(filepath.Dir(p)); len(got) != 2 {
		t.Errorf("DocsWarnings = %q, want two warnings", got)
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

func TestLoadMetaWithWarnings_UnknownKeys(t *testing.T) {
	p := writeMeta(t, `name: s
licence: MIT
sidecar:
  command: ["true"]
  install_scripts:
    linux: install.sh
    any-os-key: fine.sh
  health:
    path: /status
    retries: 3
  secrets:
    - name: A
      hint: known now
    - name: B
      hnit: typo
      Pattern: "^x"
  config:
    - &field
      name: C
      colour: red
    - <<: *field
      name: D
      extra: 1
`)
	m, warnings, err := LoadMetaWithWarnings(p)
	if err != nil {
		t.Fatalf("LoadMetaWithWarnings: %v", err)
	}
	// Loading still succeeds and keeps the known values.
	if m.Sidecar.Secrets[0].Hint != "known now" || m.Sidecar.Config[1].Name != "D" {
		t.Fatalf("known fields not loaded: %+v", m.Sidecar)
	}
	want := []string{
		"licence: unknown field (ignored)",
		"sidecar.health.retries: unknown field (ignored)",
		"sidecar.secrets[1].hnit: unknown field (ignored)",
		"sidecar.secrets[1].Pattern: unknown field (ignored)",
		"sidecar.config[0].colour: unknown field (ignored)",
		"sidecar.config[1].colour: unknown field (ignored)", // merged in from &field
		"sidecar.config[1].extra: unknown field (ignored)",
	}
	if strings.Join(warnings, "\n") != strings.Join(want, "\n") {
		t.Errorf("warnings =\n%s\nwant\n%s", strings.Join(warnings, "\n"), strings.Join(want, "\n"))
	}

	// LoadMeta behaves as before: same result, no error.
	if _, err := LoadMeta(p); err != nil {
		t.Errorf("LoadMeta: %v", err)
	}
}

func TestLoadMetaWithWarnings_CleanFileHasNone(t *testing.T) {
	p := writeMeta(t, `name: s
type: skill
version: 0.1.0
description: d
author: a
dependencies: []
sidecar:
  command: ["true"]
  mount: s
  env_passthrough: [A]
  secrets:
    - {name: A, description: d, required: false, pattern: "^a", default_from_env: X, multiline: true, hint: h, docs: SKILL.md}
  config:
    - {name: B, description: d, type: enum, required: true, default: x, default_from_env: Y, choices: [x], hint: h, docs: SKILL.md#s}
  health: {path: /status, initial_delay_ms: 1, timeout_ms: 2, interval_ms: 3}
  install_scripts: {linux: i.sh, macos: m.sh}
  protocols: [http]
  limits: {max_body_bytes: 1, idle_timeout_secs: 2}
`)
	_, warnings, err := LoadMetaWithWarnings(p)
	if err != nil {
		t.Fatalf("LoadMetaWithWarnings: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("a file using every documented field should not warn, got:\n%s", strings.Join(warnings, "\n"))
	}
}

// The omac.yaml files shipped in this repository must stay warning-free,
// so the unknown-key check does not flag a field omac really uses.
func TestLoadMetaWithWarnings_RepoSkillsHaveNone(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", ".opencode", "skills", "*", MetaFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("control: no omac.yaml found under .opencode/skills; the glob is wrong")
	}
	for _, p := range paths {
		_, warnings, err := LoadMetaWithWarnings(p)
		if err != nil {
			t.Errorf("%s: %v", p, err)
		}
		if len(warnings) != 0 {
			t.Errorf("%s: unexpected warnings:\n%s", p, strings.Join(warnings, "\n"))
		}
	}
}

// When validation fails, the unknown-key warnings still come back, since a
// typo such as `nmae:` is usually what broke validation.
func TestLoadMetaWithWarnings_ReportsUnknownKeysOnValidationError(t *testing.T) {
	p := writeMeta(t, `name: s
sidecar:
  command: ["true"]
  secrets:
    - nmae: TOKEN
`)
	m, warnings, err := LoadMetaWithWarnings(p)
	if err == nil || !strings.Contains(err.Error(), "not a valid env var name") {
		t.Fatalf("control: want the validation error for the missing name, got m=%v err=%v", m, err)
	}
	if m != nil {
		t.Errorf("a failed load must not return a Meta")
	}
	if want := "sidecar.secrets[0].nmae: unknown field (ignored)"; len(warnings) != 1 || warnings[0] != want {
		t.Errorf("warnings = %q, want [%q]", warnings, want)
	}
	if _, err := LoadMeta(p); err == nil {
		t.Errorf("LoadMeta should fail the same way")
	}
}

// TestLoadMetaWithWarnings_MatchesLoadMeta pins that both loaders return
// the same Meta and the same error for awkward inputs. LoadMeta decodes
// the bytes directly; LoadMetaWithWarnings goes through a yaml.Node first,
// and the two paths must not drift apart.
func TestLoadMetaWithWarnings_MatchesLoadMeta(t *testing.T) {
	cases := map[string]string{
		"empty file":     "",
		"comment only":   "# nothing here\n",
		"document start": "---\n",
		"null document":  "~\n",
		"multi-document": "name: first\n---\nname: second\n",
		"merge with anchor": `name: s
sidecar:
  command: ["true"]
  config:
    - &base
      name: A
      description: shared
    - <<: *base
      name: B
`,
		"alias reused as list item": `name: s
sidecar:
  command: ["true"]
  secrets:
    - &tok
      name: TOKEN
    - *tok
`,
		"type error command":   "name: s\nsidecar:\n  command: a\n",
		"type error name":      "name: [x]\n",
		"type error timeout":   "name: s\nsidecar:\n  command: [\"true\"]\n  health:\n    timeout_ms: abc\n",
		"sequence at root":     "- name: s\n",
		"duplicate keys":       "name: a\nname: b\n",
		"tab indentation":      "name: s\nsidecar:\n\tcommand: [\"true\"]\n",
		"bad docs still loads": "name: s\nsidecar:\n  command: [\"true\"]\n  secrets:\n    - name: T\n      docs: ../README.md\n",
		"valid":                "name: s\nsidecar:\n  command: [\"true\"]\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeMeta(t, body)
			m1, err1 := LoadMeta(p)
			m2, _, err2 := LoadMetaWithWarnings(p)
			if errString(err1) != errString(err2) {
				t.Errorf("errors differ:\n LoadMeta:             %v\n LoadMetaWithWarnings: %v", err1, err2)
			}
			if !reflect.DeepEqual(m1, m2) {
				t.Errorf("Meta differs:\n LoadMeta:             %+v\n LoadMetaWithWarnings: %+v", m1, m2)
			}
			// Control: the cases really exercise both outcomes, so the
			// comparison above is not only ever comparing two nils.
			if wantOK := loadsFine[name]; wantOK != (err1 == nil) {
				t.Errorf("control: LoadMeta error = %v, want success %v", err1, wantOK)
			}
		})
	}
}

// loadsFine names the cases of TestLoadMetaWithWarnings_MatchesLoadMeta
// that load without error; every other case must fail.
var loadsFine = map[string]bool{
	"multi-document":            true, // only the first document is read
	"merge with anchor":         true,
	"alias reused as list item": false, // two secrets named TOKEN collide
	"bad docs still loads":      true,
	"valid":                     true,
}

// A decode error (a value of the wrong type) still comes with the
// unknown-key warnings.
func TestLoadMetaWithWarnings_UnknownKeysOnDecodeError(t *testing.T) {
	p := writeMeta(t, "name: s\nsidecar:\n  comand: [\"true\"]\n  command: a\n")
	_, warnings, err := LoadMetaWithWarnings(p)
	if err == nil || !strings.Contains(err.Error(), "parse meta") {
		t.Fatalf("control: want a decode error, got %v", err)
	}
	if want := "sidecar.comand: unknown field (ignored)"; len(warnings) != 1 || warnings[0] != want {
		t.Errorf("warnings = %q, want [%q]", warnings, want)
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
