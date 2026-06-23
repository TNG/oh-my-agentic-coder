package builtinskills

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestNamesIncludesWriteASkill(t *testing.T) {
	found := false
	for _, n := range Names() {
		if n == "omac-write-a-skill" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Names() = %v, want to include omac-write-a-skill", Names())
	}
}

func TestMaterializeCreateUnchangedUpdate(t *testing.T) {
	parent := t.TempDir()
	skillMd := filepath.Join(parent, "omac-write-a-skill", "SKILL.md")
	ref := filepath.Join(parent, "omac-write-a-skill", "references", "creating-a-skill.md")

	// create
	res, err := Materialize("omac-write-a-skill", parent, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCreated {
		t.Fatalf("status = %s, want created", res.Status)
	}
	for _, p := range []string{skillMd, ref} {
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

// TestBundleGuideMatchesRepoRoot is the single-source-of-truth guard: the
// embedded reference copy must stay byte-identical to the repo-root
// CREATING_A_SKILL.md. If this fails, re-copy the guide into the bundle.
func TestBundleGuideMatchesRepoRoot(t *testing.T) {
	root, err := os.ReadFile(filepath.Join("..", "..", "CREATING_A_SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	bundled, err := os.ReadFile(filepath.Join("assets", "omac-write-a-skill", "references", "creating-a-skill.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(root, bundled) {
		t.Fatal("bundle references/creating-a-skill.md drifted from repo-root CREATING_A_SKILL.md; re-copy it")
	}
}
