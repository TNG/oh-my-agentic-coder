package builtinskills

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

func TestNamesIncludesBuiltins(t *testing.T) {
	for _, want := range []string{"omac-vm", "omac-write-a-skill"} {
		if !slices.Contains(Names(), want) {
			t.Errorf("Names() = %v, want to include %s", Names(), want)
		}
	}
}

// Harnesses match a skill by the frontmatter name, and `omac setup` refreshes
// only directories that carry the marker.
func TestBundlesNameMatchesDirAndCarryMarker(t *testing.T) {
	for _, name := range Names() {
		b, err := fs.ReadFile(assetsFS, assetsRoot+"/"+name+"/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		fm, ok := frontmatter(b)
		if !ok {
			t.Fatalf("%s: SKILL.md has no frontmatter", name)
		}
		if !regexp.MustCompile(`(?m)^name: ` + regexp.QuoteMeta(name) + `$`).Match(fm) {
			t.Errorf("%s: frontmatter name does not match the directory", name)
		}
		if !markerRe.Match(fm) {
			t.Errorf("%s: frontmatter lacks %s: \"true\"", name, MarkerKey)
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
