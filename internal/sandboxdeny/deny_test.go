package sandboxdeny

import (
	"strings"
	"testing"
)

func TestDefaultHasSentinel(t *testing.T) {
	d := Default()
	if !strings.HasPrefix(d.MarkerFile, "X-Omac-Sandbox: denied") {
		t.Errorf("MarkerFile missing sentinel header: %q", d.MarkerFile)
	}
	if d.MarkerDirName != ".omac-denied" {
		t.Errorf("MarkerDirName = %q; want .omac-denied", d.MarkerDirName)
	}
	if !strings.Contains(strings.ToLower(d.FacadeNote), "intentionally") {
		t.Errorf("FacadeNote lacks deterrent wording: %q", d.FacadeNote)
	}
}

func TestResolveOverrideWins(t *testing.T) {
	over := Text{
		MarkerFile:    "CUSTOM",
		MarkerDirName: ".custom",
		FacadeNote:    "custom note",
	}
	got := Resolve(over)
	if got.MarkerFile != "CUSTOM" {
		t.Errorf("MarkerFile = %q; want CUSTOM", got.MarkerFile)
	}
	if got.MarkerDirName != ".custom" {
		t.Errorf("MarkerDirName = %q; want .custom", got.MarkerDirName)
	}
	if got.FacadeNote != "custom note" {
		t.Errorf("FacadeNote = %q; want custom note", got.FacadeNote)
	}
}

func TestResolveEmptyFallsBack(t *testing.T) {
	got := Resolve(Text{})
	def := Default()
	if got.MarkerFile != def.MarkerFile {
		t.Errorf("MarkerFile = %q; want default %q", got.MarkerFile, def.MarkerFile)
	}
	if got.FacadeNote != def.FacadeNote {
		t.Errorf("FacadeNote = %q; want default", got.FacadeNote)
	}
}

func TestDefaultMentionsIntent(t *testing.T) {
	d := Default()
	if !strings.Contains(d.MarkerFile, "/sandbox/intent") {
		t.Errorf("MarkerFile should mention /sandbox/intent: %q", d.MarkerFile)
	}
	if !strings.Contains(d.FacadeNote, "/sandbox/intent") {
		t.Errorf("FacadeNote should mention /sandbox/intent: %q", d.FacadeNote)
	}
}
