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

// TestInertCommentsEveryContentLine guards #213: the default marker is
// bound over shell configs, so every line that carries content must be
// a comment — one uncommented line is one command the login shell runs.
func TestInertCommentsEveryContentLine(t *testing.T) {
	got := Inert(Default().MarkerFile)
	for i, line := range strings.Split(got, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") {
			t.Errorf("line %d is executable content: %q", i+1, line)
		}
	}
	// Still readable and greppable after neutralization.
	for _, want := range []string{"X-Omac-Sandbox: denied", "/sandbox/intent", "intentionally"} {
		if !strings.Contains(got, want) {
			t.Errorf("inert marker lost %q:\n%s", want, got)
		}
	}
}

func TestInertIsIdempotentAndPreservesShape(t *testing.T) {
	const in = "X-Omac-Sandbox: denied\n\n# already a comment\n  indented line\n"
	const want = "# X-Omac-Sandbox: denied\n\n# already a comment\n#   indented line\n"
	got := Inert(in)
	if got != want {
		t.Errorf("Inert(%q) = %q; want %q", in, got, want)
	}
	if again := Inert(got); again != got {
		t.Errorf("Inert not idempotent: %q -> %q", got, again)
	}
	if Inert("") != "" {
		t.Errorf("Inert(\"\") = %q; want empty", Inert(""))
	}
}

// TestInertNeutralizesHostileOverride pins the scope note in #213:
// denial.marker_file is profile-configurable, so a profile author must
// not be able to place a command into a file the sandboxed process
// executes.
func TestInertNeutralizesHostileOverride(t *testing.T) {
	got := Inert(Resolve(Text{MarkerFile: "touch /tmp/pwned\nrm -rf ~\n"}).MarkerFile)
	if got != "# touch /tmp/pwned\n# rm -rf ~\n" {
		t.Errorf("hostile marker not neutralized: %q", got)
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
