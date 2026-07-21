//go:build darwin

package netprompt

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestRenderPopupScreenshot renders the osascript network dialog and captures a
// full-screen PNG. Best-effort: osascript GUI dialogs need a WindowServer
// session, which headless CI runners may not provide — the CI job is
// continue-on-error and a desktop-only capture is itself the signal that the
// dialog did not render. Advisory only; appearance is not asserted.
//
// Gated on OMAC_POPUP_SHOT=1 so it never runs in the normal unit suite. There
// is no reliable per-window wait without accessibility permissions, so it uses
// a fixed settle delay before screencapture.
func TestRenderPopupScreenshot(t *testing.T) {
	if os.Getenv("OMAC_POPUP_SHOT") == "" {
		t.Skip("set OMAC_POPUP_SHOT=1 to render the real dialog")
	}
	for _, bin := range []string{"osascript", "screencapture"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%q not found on PATH", bin)
		}
	}

	shot := shotPath(t, "osascript")
	errc := make(chan error, 1)
	go func() {
		_, err := osascriptBackend{}.show(context.Background(), shotHost, shotPort,
			RegisteredSuffixHint(shotHost), shotIntent, shotCause, shotOrigin)
		errc <- err
	}()

	time.Sleep(3 * time.Second) // no window-id wait available; let the dialog map
	if out, err := exec.Command("screencapture", "-x", shot).CombinedOutput(); err != nil {
		t.Fatalf("screencapture failed: %v: %s", err, out)
	}
	// Dismiss with Escape via System Events; best-effort (may lack permission).
	_ = exec.Command("osascript", "-e", `tell application "System Events" to key code 53`).Run()
	select {
	case <-errc:
	case <-time.After(3 * time.Second):
	}
	assertShot(t, shot)
}
