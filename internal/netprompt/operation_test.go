package netprompt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestChooserStubAnswersWithoutADialog(t *testing.T) {
	t.Setenv("OMAC_APPROVAL_STUB", "Allow once")
	c, ok := NewChooser()
	if !ok {
		t.Fatal("stub chooser reported unavailable")
	}
	got, err := c.Choose(context.Background(), "t", "x", []string{"Allow once", "Deny"}, "Deny")
	if err != nil || got != "Allow once" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestSanitizeDialogTextKeepsNewlinesDropsControls(t *testing.T) {
	got := sanitizeDialogText("a\nb\rc\x00d\x1b[31me\u0085f")
	if got != "a\nb c d [31me f" {
		t.Errorf("got %q", got)
	}
	if strings.ContainsAny(got, "\r\x00\x1b") {
		t.Errorf("control characters survived: %q", got)
	}
}

// On Linux without a display, no dialog can be shown, so the chooser must
// say it is unavailable rather than let a failed zenity read as "deny".
func TestChooserWithoutDisplayIsUnavailable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("display detection is Linux-only")
	}
	t.Setenv("OMAC_APPROVAL_STUB", "")
	t.Setenv("DISPLAY", "")
	t.Setenv("WAYLAND_DISPLAY", "")
	if _, ok := NewChooser(); ok {
		t.Error("chooser reported available without a display")
	}
}

// The agent controls the path and query. Characters that render as a line
// break or reorder text must not let it forge a line of the dialog.
func TestSanitizeDialogTextDropsUnicodeLineBreaksAndFormatChars(t *testing.T) {
	got := sanitizeDialogText("x=1\u2028Declared: safe\u2029y\u202Ez\u200B")
	if strings.ContainsAny(got, "\u2028\u2029\u202E\u200B") {
		t.Errorf("survived: %q", got)
	}
	if got != "x=1 Declared: safe y z " {
		t.Errorf("got %q", got)
	}
}

func TestDialogFailureIsNotTheUsersCancel(t *testing.T) {
	for code, wantErr := range map[int]bool{1: false, 5: true, 255: true} {
		dir := t.TempDir()
		for _, name := range []string{"zenity", "kdialog"} {
			script := fmt.Sprintf("#!/bin/sh\nexit %d\n", code)
			if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		t.Setenv("PATH", dir)
		t.Setenv("DISPLAY", ":99")
		for _, b := range []dialogBackendChoice{zenityChoice{}, kdialogChoice{}} {
			label, err := b.choose(context.Background(), "t", "x", []string{"Deny"}, "Deny")
			if (err != nil) != wantErr || label != "" {
				t.Errorf("%s exit %d: label=%q err=%v, want error=%v", b.name(), code, label, err, wantErr)
			}
		}
	}
}

// zenity 4 parses list text as markup even with --no-markup and replaces
// text it cannot parse, such as a query with "&", by its default line. The
// text is escaped, and the window is tall enough to show every choice.
func TestZenityChoiceArgsEscapeMarkupAtFullHeight(t *testing.T) {
	t.Setenv("OMAC_PROMPT_HEIGHT", "")
	args := zenityChoiceArgs("omac: t", "Query: a=1&b=<b>x</b>\nnext", []string{"Allow", "Deny"}, "Deny")
	joined := strings.Join(args, "\x00")
	if strings.Contains(joined, "--no-markup") {
		t.Errorf("--no-markup does not apply to zenity 4 lists: %q", args)
	}
	if !strings.Contains(joined, "Query: a=1&amp;b=&lt;b&gt;x&lt;/b&gt;\nnext") {
		t.Errorf("text not escaped: %q", args)
	}
	if !strings.Contains(joined, "--height\x00"+fmt.Sprint(dialogHeight)) {
		t.Errorf("dialog is not full height: %q", args)
	}
}

func TestKdialogChoiceTextStaysPlain(t *testing.T) {
	args := kdialogChoiceArgs("t", "<b>first</b>\nsecond <b>x</b>", []string{"A"}, "A")
	text := args[5]
	if text != "‹b›first‹/b›\nsecond <b>x</b>" {
		t.Errorf("text = %q", text)
	}
}

// exitWith runs a shell that writes stderr and exits, to get a real
// *exec.ExitError.
func exitWith(t *testing.T, stderr string, code int) error {
	t.Helper()
	_, err := exec.Command("sh", "-c", fmt.Sprintf("printf '%%s' %q >&2; exit %d", stderr, code)).Output()
	return err
}

func TestDialogExitErrorTellsCancelFromNoDisplay(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		stderr    string
		code      int
		wantError bool
	}{
		"cancel":                     {"", 1, false},
		"cancel with gtk noise":      {"(zenity:1): Gtk-WARNING **: Unable to acquire the address of the accessibility bus", 1, false},
		"zenity 4 without a display": {"(zenity:1): Gtk-WARNING **: 17:10:03.804: Failed to open display", 1, true},
		"zenity 3 without a display": {"(zenity:1): Gtk-WARNING **: cannot open display: :77", 1, true},
		"kdialog without a display":  {"qt.qpa.xcb: could not connect to display :77", 1, true},
		"other exit code":            {"", 255, true},
	} {
		err := dialogExitError(ctx, "zenity", exitWith(t, c.stderr, c.code))
		if (err != nil) != c.wantError {
			t.Errorf("%s: err = %v, want error %v", name, err, c.wantError)
		}
	}
}

// A fake zenity on PATH stands in for the real one: the chooser runs it with
// the dialog text and reads the chosen label, a cancel, or a dead display.
func TestChooserRunsZenity(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("zenity is the Linux backend")
	}
	dir := t.TempDir()
	argv := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argv + "\ncase \"$FAKE_ZENITY\" in\n" +
		"  cancel) echo 'Gtk-WARNING: a11y bus' >&2; exit 1;;\n" +
		"  nodisplay) echo 'Gtk-WARNING: Failed to open display' >&2; exit 1;;\n" +
		"  *) echo \"$FAKE_ZENITY\";;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "zenity"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("DISPLAY", ":99")
	t.Setenv("OMAC_APPROVAL_STUB", "")
	c, ok := NewChooser()
	if !ok {
		t.Fatal("chooser unavailable with zenity on PATH")
	}
	choose := func() (string, error) {
		return c.Choose(context.Background(), "omac: t", "Query: a=1&b=2", []string{"Allow once", "Deny"}, "Deny")
	}
	t.Setenv("FAKE_ZENITY", "Allow once")
	if got, err := choose(); got != "Allow once" || err != nil {
		t.Errorf("answer: %q, %v", got, err)
	}
	if raw, _ := os.ReadFile(argv); !strings.Contains(string(raw), "Query: a=1&amp;b=2") {
		t.Errorf("zenity argv:\n%s", raw)
	}
	t.Setenv("FAKE_ZENITY", "cancel")
	if got, err := choose(); got != "" || err != nil {
		t.Errorf("cancel: %q, %v", got, err)
	}
	t.Setenv("FAKE_ZENITY", "nodisplay")
	if _, err := choose(); err == nil || !strings.Contains(err.Error(), "Failed to open display") {
		t.Errorf("dead display read as cancel: %v", err)
	}
}

func TestChooserStubOffValues(t *testing.T) {
	for _, v := range []string{"0", "false", "No", "OFF", " "} {
		t.Setenv("OMAC_APPROVAL_STUB", v)
		t.Setenv("DISPLAY", "")
		t.Setenv("WAYLAND_DISPLAY", "")
		c, _ := NewChooser()
		if c.Stub() != "" {
			t.Errorf("OMAC_APPROVAL_STUB=%q turned the stub on with %q", v, c.Stub())
		}
	}
	t.Setenv("OMAC_APPROVAL_STUB", "Deny")
	if c, _ := NewChooser(); c.Stub() != "Deny" {
		t.Errorf("stub = %q", c.Stub())
	}
}
