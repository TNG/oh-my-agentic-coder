package netprompt

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Rendered-size estimates for the default GTK/Qt dialog font. Deliberately
// conservative (see the reviewer's ~8 px/char heuristic). The floors below are
// derived from the *actual* content (optionLabels/promptText), not pinned to
// magic numbers, so if a label or the prompt grows the required size grows with
// it and the guard fails unless dialogWidth/dialogHeight grow too.
const (
	perCharPx    = 8   // per-char advance in the default UI font
	widthChrome  = 120 // radio column + margins + scrollbar gutter
	perRowPx     = 28  // one radiolist row
	perLinePx    = 22  // one prompt-text line
	heightChrome = 130 // title bar + button row + margins

	// The pre-fix dialog used height 320; the PR's own rationale is that
	// 320/360 collapse the radiolist to a two-row scroll box on Ubuntu. Any
	// derived floor must stay strictly above this, or a drift back into the
	// buggy range would pass silently.
	knownBadHeight = 320
)

// longestLabel returns the widest label by character count.
func longestLabel(labels []string) string {
	longest := ""
	for _, l := range labels {
		if len(l) > len(longest) {
			longest = l
		}
	}
	return longest
}

// TestDialogWidthFitsLongestLabel is the T2 guard: the bug being fixed is
// "too narrow to show the longest option label", so the property under test is
// "dialogWidth renders the longest label", not "dialogWidth >= some number".
func TestDialogWidthFitsLongestLabel(t *testing.T) {
	longest := longestLabel(optionLabels("example.com"))
	need := len(longest)*perCharPx + widthChrome
	if dialogWidth < need {
		t.Errorf("dialogWidth = %d, but longest label %q needs >= %d px (%d chars * %d + %d chrome)",
			dialogWidth, longest, need, len(longest), perCharPx, widthChrome)
	}
}

// TestDialogHeightFitsAllRowsAndPrompt is the T1 guard: the floor is derived
// from the option rows plus the wrapped prompt lines plus chrome, and is
// asserted to sit above the known-bad 320 so the regression cannot creep back.
//
// The prompt is measured at its worst case, not its shortest: the Origin and
// Likely-cause lines are both populated on the production path, so sizing
// against the bare prompt leaves the real dialog scrolling — the very bug the
// constant exists to prevent. It cannot cover a wrapped intent line, which is
// unbounded; OMAC_PROMPT_HEIGHT is the escape hatch until that is capped.
func TestDialogHeightFitsAllRowsAndPrompt(t *testing.T) {
	opts := optionLabels("example.com")
	worst := promptText("api.github.com", 443,
		"fetch the pinned plugin tarball", "harness plugin install into a cold cache",
		"opencode (pid 4711)", len(opts))
	lines := strings.Count(worst, "\n") + 1
	need := len(opts)*perRowPx + lines*perLinePx + heightChrome
	if need <= knownBadHeight {
		t.Fatalf("derived height floor %d <= known-bad %d; estimates too low to guard the regression",
			need, knownBadHeight)
	}
	if dialogHeight < need {
		t.Errorf("dialogHeight = %d, but %d options + %d prompt lines need >= %d px",
			dialogHeight, len(opts), lines, need)
	}
}

func TestZenityArgsCarrySize(t *testing.T) {
	args := zenityArgs("api.github.com", 443, "github.com", "", "", "")
	if got := flagValue(args, "--width"); got != strconv.Itoa(dialogWidth) {
		t.Errorf("zenity --width = %q, want %d", got, dialogWidth)
	}
	if got := flagValue(args, "--height"); got != strconv.Itoa(dialogHeight) {
		t.Errorf("zenity --height = %q, want %d", got, dialogHeight)
	}
}

// TestKdialogGeometryFormatAndOrdering pins the two kdialog properties the
// reviewer flagged as unverified. Verified against KDE/kdialog master (KF6):
// --geometry is a registered QCommandLineOption whose value accepts the X11
// "WxH" form, and QCommandLineParser treats it as an option regardless of
// position, so placing it before --radiolist is safe. --radiolist takes its
// prompt text as the immediate positional argument, ahead of the option
// triples.
func TestKdialogGeometryFormatAndOrdering(t *testing.T) {
	args := kdialogArgs("api.github.com", 443, "github.com", "", "", "")

	geo := flagValue(args, "--geometry")
	if !regexp.MustCompile(`^\d+x\d+$`).MatchString(geo) {
		t.Errorf("kdialog --geometry = %q, want WxH (e.g. 520x640)", geo)
	}
	if want := strconv.Itoa(dialogWidth) + "x" + strconv.Itoa(dialogHeight); geo != want {
		t.Errorf("kdialog --geometry = %q, want %q", geo, want)
	}

	gi, ri := indexOf(args, "--geometry"), indexOf(args, "--radiolist")
	if gi < 0 || ri < 0 || gi > ri {
		t.Fatalf("--geometry (idx %d) must appear before --radiolist (idx %d)", gi, ri)
	}
	if got := args[ri+1]; got != promptText("api.github.com", 443, "", "", "", len(optionLabels("github.com"))) {
		t.Errorf("arg after --radiolist = %q, want the prompt text", got)
	}
}

func TestDialogDimensionsEnvOverride(t *testing.T) {
	t.Setenv("OMAC_PROMPT_WIDTH", "800")
	t.Setenv("OMAC_PROMPT_HEIGHT", "900")

	w, h := dialogDimensions()
	if w != 800 || h != 900 {
		t.Fatalf("dialogDimensions() = %dx%d, want 800x900", w, h)
	}
	if got := flagValue(zenityArgs("h", 1, "s", "", "", ""), "--width"); got != "800" {
		t.Errorf("zenity --width = %q, want 800", got)
	}
	if got := flagValue(kdialogArgs("h", 1, "s", "", "", ""), "--geometry"); got != "800x900" {
		t.Errorf("kdialog --geometry = %q, want 800x900", got)
	}
}

func TestDialogDimensionsEnvInvalidFallsBack(t *testing.T) {
	t.Setenv("OMAC_PROMPT_WIDTH", "not-a-number")
	t.Setenv("OMAC_PROMPT_HEIGHT", "-5")

	w, h := dialogDimensions()
	if w != dialogWidth || h != dialogHeight {
		t.Errorf("dialogDimensions() = %dx%d with invalid env, want defaults %dx%d",
			w, h, dialogWidth, dialogHeight)
	}
}

func TestAlertArgs(t *testing.T) {
	const title = "omac: protected file created"
	const body = "/workdir/.env matches a sandbox-protected pattern"

	z := alertZenityArgs(title, body)
	if z[0] != "--info" {
		t.Errorf("zenity alert first arg = %q, want --info", z[0])
	}
	if got := flagValue(z, "--title"); got != title {
		t.Errorf("zenity alert --title = %q, want %q", got, title)
	}
	if got := flagValue(z, "--text"); got != body {
		t.Errorf("zenity alert --text = %q, want %q", got, body)
	}

	k := alertKdialogArgs(title, body)
	if got := flagValue(k, "--title"); got != title {
		t.Errorf("kdialog alert --title = %q, want %q", got, title)
	}
	if got := flagValue(k, "--msgbox"); got != body {
		t.Errorf("kdialog alert --msgbox = %q, want %q", got, body)
	}
}

// TestChooseArgsCarrySizeAndPlainText guards the generic Choose dialog: the
// protected-file dialog can offer up to fourteen long options, and the
// previous argv carried neither a size nor a plain-text flag. --no-markup and
// unescaped labels are what keep an option containing &, <, or > returnable as
// the same string the caller passes in.
func TestChooseArgsCarrySizeAndPlainText(t *testing.T) {
	width, height := dialogDimensions()
	const label = "Ignore this file permanently: /work/a&b/<x>/.env"
	const message = "Create <one> & keep it?"

	z := chooseZenityArgs("omac: choose", message, []string{label}, width, height)
	if len(z) < 2 || z[0] != "--list" || z[1] != "--no-markup" {
		t.Errorf("zenity args = %v, want --list --no-markup first", z)
	}
	if got := flagValue(z, "--width"); got != strconv.Itoa(width) {
		t.Errorf("zenity --width = %q, want %d", got, width)
	}
	if got := flagValue(z, "--height"); got != strconv.Itoa(height) {
		t.Errorf("zenity --height = %q, want %d", got, height)
	}
	if got := flagValue(z, "--text"); got != message {
		t.Errorf("zenity --text = %q, want the plain message %q", got, message)
	}
	if got := z[len(z)-1]; got != label {
		t.Errorf("zenity option = %q, want the plain label %q", got, label)
	}

	k := chooseKdialogArgs("omac: choose", message, []string{label}, width, height)
	if got, want := flagValue(k, "--geometry"), strconv.Itoa(width)+"x"+strconv.Itoa(height); got != want {
		t.Errorf("kdialog --geometry = %q, want %q", got, want)
	}
	if got := flagValue(k, "--menu"); got != message {
		t.Errorf("kdialog --menu = %q, want the plain message %q", got, message)
	}
	if got := k[len(k)-1]; got != label {
		t.Errorf("kdialog option = %q, want the plain label %q", got, label)
	}
	for _, arg := range append(append([]string{}, z...), k...) {
		if strings.Contains(arg, "&amp;") || strings.Contains(arg, "&lt;") || strings.Contains(arg, "&gt;") {
			t.Fatalf("plain-text dialog arg is Pango-escaped: %q", arg)
		}
	}
}

func TestChooseContextEmptyOptions(t *testing.T) {
	choice, err := ChooseContext(context.Background(), "title", "message", nil)
	if err != nil || choice != "" {
		t.Fatalf("ChooseContext with no options = %q, %v; want empty, nil", choice, err)
	}
}

// flagValue returns the argument following flag in args, or "".
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// indexOf returns the index of target in args, or -1.
func indexOf(args []string, target string) int {
	for i, a := range args {
		if a == target {
			return i
		}
	}
	return -1
}
