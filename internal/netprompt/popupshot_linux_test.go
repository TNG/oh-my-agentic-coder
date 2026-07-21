//go:build linux

package netprompt

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRenderPopupScreenshot renders the real network-approval dialog under an
// X display and captures a PNG, so CI can surface how the dialog actually looks
// — label alignment (space-padding aligns only in monospace; GTK/Qt draw
// proportional), wrapping, and truncation against the fixed height. It is
// advisory: the artifact is for human eyes, not a pass/fail assertion on
// appearance.
//
// Gated on OMAC_POPUP_SHOT=1 so it never runs in the normal unit suite; needs
// the chosen dialog tool plus imagemagick (import) and xdotool, and a DISPLAY
// (xvfb-run in CI). OMAC_POPUP_SHOT_BACKEND selects zenity (default) or kdialog.
func TestRenderPopupScreenshot(t *testing.T) {
	if os.Getenv("OMAC_POPUP_SHOT") == "" {
		t.Skip("set OMAC_POPUP_SHOT=1 to render the real dialog (needs a dialog tool + DISPLAY)")
	}
	if os.Getenv("DISPLAY") == "" {
		t.Skip("no DISPLAY set; render under xvfb-run")
	}
	backend := linuxShotBackend(os.Getenv("OMAC_POPUP_SHOT_BACKEND"))
	for _, bin := range []string{backend.name(), "import", "xdotool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%q not found on PATH", bin)
		}
	}

	shot := shotPath(t, backend.name())
	errc := make(chan error, 1)
	go func() {
		_, err := backend.show(context.Background(), shotHost, shotPort,
			RegisteredSuffixHint(shotHost), shotIntent, shotCause, shotOrigin)
		errc <- err
	}()

	win := waitForWindow(t, shotTitle, 15*time.Second)
	if out, err := exec.Command("import", "-window", win, shot).CombinedOutput(); err != nil {
		t.Fatalf("screenshot failed: %v: %s", err, out)
	}
	_ = exec.Command("xdotool", "windowkill", win).Run() // let the backend goroutine return
	select {
	case <-errc:
	case <-time.After(3 * time.Second):
	}
	assertShot(t, shot)
}

func linuxShotBackend(name string) dialogBackend {
	if strings.EqualFold(name, "kdialog") {
		return kdialogBackend{}
	}
	return zenityBackend{}
}

// waitForWindow blocks until a window titled title exists (xdotool --sync),
// bounded by timeout, and returns its id.
func waitForWindow(t *testing.T, title string, timeout time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xdotool", "search", "--sync", "--name", title).Output()
	if err != nil {
		t.Fatalf("dialog window %q did not appear within %s: %v", title, timeout, err)
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		t.Fatalf("no window id for %q", title)
	}
	return ids[len(ids)-1]
}
