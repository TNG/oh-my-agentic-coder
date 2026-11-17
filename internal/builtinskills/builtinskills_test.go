package builtinskills

import (
	"bytes"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestNamesIncludesBuiltins(t *testing.T) {
	for _, want := range []string{"omac-vm", "omac-write-a-skill"} {
		if !slices.Contains(Names(), want) {
			t.Errorf("Names() = %v, want to include %s", Names(), want)
		}
	}
}

// Harnesses match a skill by the frontmatter name, and `omac setup` refreshes
// only directories that carry the marker. A Windows checkout embeds CRLF.
func TestBundlesNameMatchesDirAndCarryMarker(t *testing.T) {
	for _, name := range Names() {
		raw, err := fs.ReadFile(assetsFS, assetsRoot+"/"+name+"/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		lf := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
		variants := map[string][]byte{"LF": lf, "CRLF": bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))}
		for eol, b := range variants {
			fm, ok := frontmatter(b)
			if !ok {
				t.Fatalf("%s (%s): SKILL.md has no frontmatter", name, eol)
			}
			var meta struct {
				Name string `yaml:"name"`
			}
			if err := yaml.Unmarshal(fm, &meta); err != nil {
				t.Fatalf("%s (%s): frontmatter: %v", name, eol, err)
			}
			if meta.Name != name {
				t.Errorf("%s (%s): frontmatter name = %q, want the directory name", name, eol, meta.Name)
			}
			if !markerRe.Match(fm) {
				t.Errorf("%s (%s): frontmatter lacks %s: \"true\"", name, eol, MarkerKey)
			}
		}
	}
}

func TestMaterializeCreateUnchangedUpdate(t *testing.T) {
	parent := t.TempDir()
	skillMd := filepath.Join(parent, "omac-write-a-skill", "SKILL.md")

	// create
	res, err := Materialize("omac-write-a-skill", parent, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCreated {
		t.Fatalf("status = %s, want created", res.Status)
	}
	for _, p := range []string{skillMd} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("expected file written: %s: %v", p, err)
		}
	}

	// unchanged on re-run
	res, err = Materialize("omac-write-a-skill", parent, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusUnchanged {
		t.Fatalf("status = %s, want unchanged", res.Status)
	}

	// drift an omac-owned file (marker stays in SKILL.md) → updated + restored
	orig, err := os.ReadFile(skillMd)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillMd, append(append([]byte{}, orig...), []byte("\n<!-- drift -->\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Materialize("omac-write-a-skill", parent, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusUpdated {
		t.Fatalf("status = %s, want updated", res.Status)
	}
	got, err := os.ReadFile(skillMd)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, orig) {
		t.Fatal("SKILL.md was not refreshed to the embedded version")
	}
}

// SKILL.md links to the bundle's other files, so they are written, checked
// and refreshed like SKILL.md.
func TestSecondaryFileIsWrittenCheckedAndRestored(t *testing.T) {
	want, err := fs.ReadFile(assetsFS, assetsRoot+"/omac-vm/setup.md")
	if err != nil {
		t.Fatal(err)
	}
	drifts := map[string]func(path string) error{
		"edited":  func(p string) error { return os.WriteFile(p, []byte("old\n"), 0o644) },
		"deleted": os.Remove,
	}
	for name, drift := range drifts {
		t.Run(name, func(t *testing.T) {
			parent := t.TempDir()
			setup := filepath.Join(parent, "omac-vm", "setup.md")
			assertWritten := func() {
				t.Helper()
				got, err := os.ReadFile(setup)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("setup.md differs from the embedded version (err %v)", err)
				}
			}
			if _, err := Materialize("omac-vm", parent, false); err != nil {
				t.Fatal(err)
			}
			assertWritten()
			if err := drift(setup); err != nil {
				t.Fatal(err)
			}
			if st, err := Check("omac-vm", parent); err != nil || st != StateStale {
				t.Fatalf("Check = %s, %v; want %s", st, err, StateStale)
			}
			res, err := Materialize("omac-vm", parent, false)
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != StatusUpdated {
				t.Fatalf("status = %s, want updated", res.Status)
			}
			assertWritten()
		})
	}
}

var sectionRefRe = regexp.MustCompile(`"([^"]+)"\s+(?:in\s+\[[^\]]+\]\(([^)]+\.md)\)|above)`)

// The skills point agents at sections by heading, as in
// `"Sandbox grants" in [setup.md](setup.md)` or `"Sandbox grants" above`;
// renaming a heading must fail here.
func TestSectionReferencesExist(t *testing.T) {
	found := 0
	err := fs.WalkDir(assetsFS, assetsRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".md" {
			return err
		}
		text, err := fs.ReadFile(assetsFS, p)
		if err != nil {
			return err
		}
		for _, m := range sectionRefRe.FindAllStringSubmatch(string(text), -1) {
			found++
			file := p
			if m[2] != "" {
				file = path.Join(path.Dir(p), m[2])
			}
			target, err := fs.ReadFile(assetsFS, file)
			if err != nil {
				t.Errorf("%s: %q refers to a missing file: %v", p, m[0], err)
				continue
			}
			title := strings.Join(strings.Fields(m[1]), " ")
			heading := regexp.MustCompile(`(?m)^#+ (\d+\. )?` + regexp.QuoteMeta(title) + `\s*$`)
			if !heading.Match(target) {
				t.Errorf("%s: %s has no heading %q", p, file, title)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("found no section references; the pattern no longer matches the skills")
	}
}

func TestMaterializeForeignGuard(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "omac-write-a-skill")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Same name, but no omac-builtin marker → foreign.
	foreign := []byte("---\nname: omac-write-a-skill\n---\nsomeone else's skill\n")
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), foreign, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Materialize("omac-write-a-skill", parent, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusForeign {
		t.Fatalf("status = %s, want foreign", res.Status)
	}
	got, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, foreign) {
		t.Fatal("foreign SKILL.md must be left untouched")
	}

	// --force overrides the guard.
	res, err = Materialize("omac-write-a-skill", parent, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusUpdated {
		t.Fatalf("status = %s, want updated under force", res.Status)
	}
}
