package netprompt

import (
	"strconv"
	"testing"
)

// Below these the GTK/Qt auto-size collapses the radiolist and truncates the
// longest option label ("Allow permanently (this host)"). This is the
// regression guard for the "popups too small on Ubuntu" fix.
const (
	minReadableWidth  = 480
	minReadableHeight = 300
)

func TestDialogDimensionsAreReadable(t *testing.T) {
	if dialogWidth < minReadableWidth {
		t.Errorf("dialogWidth = %d, want >= %d", dialogWidth, minReadableWidth)
	}
	if dialogHeight < minReadableHeight {
		t.Errorf("dialogHeight = %d, want >= %d", dialogHeight, minReadableHeight)
	}
}

func TestZenityArgsCarrySize(t *testing.T) {
	args := zenityArgs("api.github.com", 443, "github.com", "")
	if got := flagValue(args, "--width"); got != strconv.Itoa(dialogWidth) {
		t.Errorf("zenity --width = %q, want %d", got, dialogWidth)
	}
	if got := flagValue(args, "--height"); got != strconv.Itoa(dialogHeight) {
		t.Errorf("zenity --height = %q, want %d", got, dialogHeight)
	}
}

func TestKdialogArgsCarrySize(t *testing.T) {
	args := kdialogArgs("api.github.com", 443, "github.com", "")
	want := strconv.Itoa(dialogWidth) + "x" + strconv.Itoa(dialogHeight)
	if got := flagValue(args, "--geometry"); got != want {
		t.Errorf("kdialog --geometry = %q, want %q", got, want)
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
