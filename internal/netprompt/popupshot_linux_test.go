//go:build linux

package netprompt

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
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
	err := captureWindow(win, shot, 6, time.Second)
	_ = exec.Command("xdotool", "windowkill", win).Run() // let the backend goroutine return
	select {
	case <-errc:
	case <-time.After(3 * time.Second):
	}
	if err != nil {
		t.Fatal(err)
	}
	assertShot(t, shot)
}

// captureWindow grabs window win to out, retrying until the image is painted.
// xdotool --sync waits for the window to exist, not to draw, so a fresh grab
// can be a blank rectangle; a painted dialog has many colours, a blank window
// only one or two. Retries settle GTK/Qt's first paint.
func captureWindow(win, out string, attempts int, wait time.Duration) error {
	var colors int
	for i := 0; i < attempts; i++ {
		time.Sleep(wait)
		if o, err := exec.Command("import", "-window", win, out).CombinedOutput(); err != nil {
			return fmt.Errorf("import failed: %v: %s", err, o)
		}
		var err error
		if colors, err = pngColors(out); err != nil {
			return err
		}
		if colors > 4 {
			return nil
		}
	}
	return fmt.Errorf("captured a blank window (%d colours) after %d attempts — dialog did not paint", colors, attempts)
}

// pngColors returns the number of distinct colours in an image (ImageMagick %k).
func pngColors(path string) (int, error) {
	out, err := exec.Command("identify", "-format", "%k", path).Output()
	if err != nil {
		return 0, fmt.Errorf("identify failed: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("parse colour count %q: %v", out, err)
	}
	return n, nil
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
