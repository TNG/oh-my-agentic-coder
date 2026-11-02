package netprompt

import (
	"context"
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
