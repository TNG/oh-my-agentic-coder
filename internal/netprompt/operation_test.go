package netprompt

import (
	"context"
	"fmt"
	"os"
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
