package builtinskills

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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

// SKILL.md links to the bundle's other files, so they must be written too.
func TestMaterializeWritesWholeBundle(t *testing.T) {
	parent := t.TempDir()
	if _, err := Materialize("omac-vm", parent, false); err != nil {
		t.Fatal(err)
	}
	want, err := fs.ReadFile(assetsFS, assetsRoot+"/omac-vm/setup.md")
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(parent, "omac-vm", "setup.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("setup.md differs from the embedded version")
	}
}

func TestMissingSecondaryFileIsStaleAndRestored(t *testing.T) {
	parent := t.TempDir()
	if _, err := Materialize("omac-vm", parent, false); err != nil {
		t.Fatal(err)
	}
	setup := filepath.Join(parent, "omac-vm", "setup.md")
	if err := os.Remove(setup); err != nil {
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
	if _, err := os.Stat(setup); err != nil {
		t.Fatalf("setup.md not restored: %v", err)
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
